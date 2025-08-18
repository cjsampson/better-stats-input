package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// Connection timeouts
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 512 * 1024

	// Reconnection settings
	initialReconnectDelay = 1 * time.Second
	maxReconnectDelay     = 30 * time.Second
	reconnectBackoff      = 1.5
	maxReconnectAttempts  = 0 // 0 means unlimited
)

// Command represents a WebSocket command
type Command struct {
	Action string                 `json:"action"`
	Data   map[string]interface{} `json:"data,omitempty"`
	ID     string                 `json:"id,omitempty"`
}

// Response represents a WebSocket response
type Response struct {
	ID      string      `json:"id,omitempty"`
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// RobustWebSocketClient provides automatic reconnection and error handling
type RobustWebSocketClient struct {
	url               string
	conn              *websocket.Conn
	mu                sync.RWMutex
	isConnected       bool
	isConnecting      bool
	reconnectAttempts int
	reconnectDelay    time.Duration

	// Channels
	send     chan Command
	receive  chan Response
	shutdown chan struct{}
	done     chan struct{}

	// Callbacks
	onConnect    func()
	onDisconnect func(error)
	onMessage    func(Response)
	onError      func(error)

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc
}

// NewRobustWebSocketClient creates a new robust WebSocket client
func NewRobustWebSocketClient(serverURL string) *RobustWebSocketClient {
	ctx, cancel := context.WithCancel(context.Background())

	return &RobustWebSocketClient{
		url:            serverURL,
		send:           make(chan Command, 256),
		receive:        make(chan Response, 256),
		shutdown:       make(chan struct{}),
		done:           make(chan struct{}),
		reconnectDelay: initialReconnectDelay,
		ctx:            ctx,
		cancel:         cancel,
	}
}

// SetCallbacks sets event callbacks
func (c *RobustWebSocketClient) SetCallbacks(
	onConnect func(),
	onDisconnect func(error),
	onMessage func(Response),
	onError func(error),
) {
	c.onConnect = onConnect
	c.onDisconnect = onDisconnect
	c.onMessage = onMessage
	c.onError = onError
}

// Connect starts the WebSocket connection with automatic reconnection
func (c *RobustWebSocketClient) Connect() error {
	go c.connectionManager()
	return nil
}

// SendCommand sends a command to the server
func (c *RobustWebSocketClient) SendCommand(action string, data map[string]interface{}) error {
	cmd := Command{
		Action: action,
		Data:   data,
		ID:     fmt.Sprintf("%s_%d", action, time.Now().UnixNano()),
	}

	select {
	case c.send <- cmd:
		return nil
	case <-time.After(writeWait):
		return fmt.Errorf("send channel full")
	case <-c.ctx.Done():
		return fmt.Errorf("client shutting down")
	}
}

// IsConnected returns the current connection status
func (c *RobustWebSocketClient) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isConnected
}

// Close gracefully shuts down the client
func (c *RobustWebSocketClient) Close() error {
	c.cancel()

	select {
	case <-c.done:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("shutdown timeout")
	}
}

// connectionManager handles the connection lifecycle
func (c *RobustWebSocketClient) connectionManager() {
	defer close(c.done)

	for {
		select {
		case <-c.ctx.Done():
			c.disconnect()
			return
		default:
			if err := c.connect(); err != nil {
				if c.onError != nil {
					c.onError(fmt.Errorf("connection failed: %v", err))
				}

				if !c.shouldReconnect() {
					return
				}

				c.waitForReconnect()
				continue
			}

			// Connection successful, reset reconnect attempts
			c.reconnectAttempts = 0
			c.reconnectDelay = initialReconnectDelay

			if c.onConnect != nil {
				c.onConnect()
			}

			// Handle the connection until it fails
			c.handleConnection()
		}
	}
}

// connect establishes a WebSocket connection
func (c *RobustWebSocketClient) connect() error {
	c.mu.Lock()
	if c.isConnecting {
		c.mu.Unlock()
		return fmt.Errorf("already connecting")
	}
	c.isConnecting = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.isConnecting = false
		c.mu.Unlock()
	}()

	u, err := url.Parse(c.url)
	if err != nil {
		return err
	}

	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.isConnected = true
	c.mu.Unlock()

	// Set connection limits
	conn.SetReadLimit(maxMessageSize)
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	return nil
}

// handleConnection manages reading/writing for an active connection
func (c *RobustWebSocketClient) handleConnection() {
	var wg sync.WaitGroup

	// Start read and write pumps
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.readPump()
	}()
	go func() {
		defer wg.Done()
		c.writePump()
	}()

	// Wait for both pumps to finish
	wg.Wait()

	c.disconnect()
}

// readPump handles incoming messages
func (c *RobustWebSocketClient) readPump() {
	defer func() {
		if c.onDisconnect != nil {
			c.onDisconnect(fmt.Errorf("read pump terminated"))
		}
	}()

	c.conn.SetReadDeadline(time.Now().Add(pongWait))

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			var response Response
			if err := c.conn.ReadJSON(&response); err != nil {
				if !websocket.IsCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					log.Printf("Read error: %v", err)
				}
				return
			}

			if c.onMessage != nil {
				c.onMessage(response)
			}

			// Also send to receive channel for synchronous operations
			select {
			case c.receive <- response:
			default:
				// Channel full, drop message
			}
		}
	}
}

// writePump handles outgoing messages and ping/pong
func (c *RobustWebSocketClient) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			c.conn.WriteMessage(websocket.CloseMessage, []byte{})
			return

		case cmd := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteJSON(cmd); err != nil {
				log.Printf("Write error: %v", err)
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				log.Printf("Ping error: %v", err)
				return
			}
		}
	}
}

// disconnect closes the current connection
func (c *RobustWebSocketClient) disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.isConnected = false
}

// shouldReconnect determines if reconnection should be attempted
func (c *RobustWebSocketClient) shouldReconnect() bool {
	if maxReconnectAttempts > 0 && c.reconnectAttempts >= maxReconnectAttempts {
		return false
	}

	select {
	case <-c.ctx.Done():
		return false
	default:
		return true
	}
}

// waitForReconnect implements exponential backoff
func (c *RobustWebSocketClient) waitForReconnect() {
	c.reconnectAttempts++

	log.Printf("Reconnect attempt %d, waiting %v", c.reconnectAttempts, c.reconnectDelay)

	select {
	case <-time.After(c.reconnectDelay):
		// Increase delay for next attempt
		c.reconnectDelay = time.Duration(float64(c.reconnectDelay) * reconnectBackoff)
		if c.reconnectDelay > maxReconnectDelay {
			c.reconnectDelay = maxReconnectDelay
		}
	case <-c.ctx.Done():
		return
	}
}

// Example usage
func main() {
	client := NewRobustWebSocketClient("ws://localhost:8080/ws")

	// Set up callbacks
	client.SetCallbacks(
		func() {
			log.Println("✅ Connected to server")
		},
		func(err error) {
			log.Printf("❌ Disconnected: %v", err)
		},
		func(response Response) {
			log.Printf("📨 Received: %+v", response)
		},
		func(err error) {
			log.Printf("❗ Error: %v", err)
		},
	)

	// Start connection
	if err := client.Connect(); err != nil {
		log.Fatalf("Failed to start client: %v", err)
	}

	// Wait for connection
	time.Sleep(2 * time.Second)

	// Send some test commands
	commands := []struct {
		action string
		data   map[string]interface{}
	}{
		{"ping", nil},
		{"health", nil},
		{"navigate", map[string]interface{}{"url": "https://example.com"}},
		{"ping", nil},
	}

	for _, cmd := range commands {
		if err := client.SendCommand(cmd.action, cmd.data); err != nil {
			log.Printf("Failed to send command %s: %v", cmd.action, err)
		}
		time.Sleep(1 * time.Second)
	}

	// Keep running for a while to test reconnection
	log.Println("Client running... Try stopping and starting the server to test reconnection")
	time.Sleep(60 * time.Second)

	// Graceful shutdown
	log.Println("Shutting down client...")
	if err := client.Close(); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}
	log.Println("Client shutdown complete")
}
