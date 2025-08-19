package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 512 * 1024

	initialReconnectDelay = 1 * time.Second
	maxReconnectDelay     = 30 * time.Second
	reconnectBackoff      = 1.5
)

// Command represents a WebSocket command with server targeting
type Command struct {
	Action   string                 `json:"action"`
	Data     map[string]interface{} `json:"data,omitempty"`
	ID       string                 `json:"id,omitempty"`
	ServerID string                 `json:"server_id,omitempty"` // Target specific server
}

// Response represents a WebSocket response
type Response struct {
	ID       string      `json:"id,omitempty"`
	Success  bool        `json:"success"`
	Data     interface{} `json:"data,omitempty"`
	Error    string      `json:"error,omitempty"`
	ServerID string      `json:"server_id,omitempty"` // Which server responded
}

// ServerConnection manages a single server connection
type ServerConnection struct {
	id                string
	url               string
	conn              *websocket.Conn
	mu                sync.RWMutex
	isConnected       bool
	isConnecting      bool
	reconnectAttempts int
	reconnectDelay    time.Duration
	lastSeen          time.Time

	// Channels
	send     chan Command
	receive  chan Response
	shutdown chan struct{}

	// Parent client reference
	client *MultiServerClient

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc
}

// ServerStats tracks server performance
type ServerStats struct {
	TotalCommands    int64
	SuccessfulCmds   int64
	FailedCmds       int64
	AvgResponseTime  time.Duration
	LastResponseTime time.Time
	ConnectionUptime time.Duration
	ReconnectCount   int64
}

// LoadBalancingStrategy defines how to select servers
type LoadBalancingStrategy int

const (
	RoundRobin LoadBalancingStrategy = iota
	Random
	LeastConnections
	HealthBased
)

// MultiServerClient manages connections to multiple ChromeDP servers
type MultiServerClient struct {
	servers     map[string]*ServerConnection
	serverStats map[string]*ServerStats
	serversMu   sync.RWMutex

	// Load balancing
	strategy        LoadBalancingStrategy
	roundRobinIndex int

	// Channels for unified communication
	commandQueue  chan Command
	responseQueue chan Response

	// Callbacks
	onConnect    func(serverID string)
	onDisconnect func(serverID string, err error)
	onMessage    func(serverID string, response Response)
	onError      func(serverID string, err error)

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	// Configuration
	maxRetries          int
	healthCheckInterval time.Duration
}

// NewMultiServerClient creates a new multi-server WebSocket client
func NewMultiServerClient(strategy LoadBalancingStrategy) *MultiServerClient {
	ctx, cancel := context.WithCancel(context.Background())

	return &MultiServerClient{
		servers:             make(map[string]*ServerConnection),
		serverStats:         make(map[string]*ServerStats),
		strategy:            strategy,
		commandQueue:        make(chan Command, 1000),
		responseQueue:       make(chan Response, 1000),
		ctx:                 ctx,
		cancel:              cancel,
		maxRetries:          5,
		healthCheckInterval: 30 * time.Second,
	}
}

// AddServer adds a ChromeDP server to the client pool
func (mc *MultiServerClient) AddServer(serverID, serverURL string) error {
	mc.serversMu.Lock()
	defer mc.serversMu.Unlock()

	if _, exists := mc.servers[serverID]; exists {
		return fmt.Errorf("server %s already exists", serverID)
	}

	ctx, cancel := context.WithCancel(mc.ctx)

	server := &ServerConnection{
		id:             serverID,
		url:            serverURL,
		send:           make(chan Command, 256),
		receive:        make(chan Response, 256),
		shutdown:       make(chan struct{}),
		client:         mc,
		ctx:            ctx,
		cancel:         cancel,
		reconnectDelay: initialReconnectDelay,
	}

	mc.servers[serverID] = server
	mc.serverStats[serverID] = &ServerStats{}

	// Start connection manager for this server
	go server.connectionManager()

	log.Printf("Added server %s: %s", serverID, serverURL)
	return nil
}

// RemoveServer removes a server from the pool
func (mc *MultiServerClient) RemoveServer(serverID string) error {
	mc.serversMu.Lock()
	defer mc.serversMu.Unlock()

	server, exists := mc.servers[serverID]
	if !exists {
		return fmt.Errorf("server %s not found", serverID)
	}

	server.cancel()
	delete(mc.servers, serverID)
	delete(mc.serverStats, serverID)

	log.Printf("Removed server %s", serverID)
	return nil
}

// Start begins the multi-server client
func (mc *MultiServerClient) Start() error {
	// Start command dispatcher
	go mc.commandDispatcher()

	// Start health checker
	go mc.healthChecker()

	log.Println("Multi-server client started")
	return nil
}

// SendCommand sends a command with optional server targeting
func (mc *MultiServerClient) SendCommand(action string, data map[string]interface{}, targetServerID ...string) (string, error) {
	cmdID := fmt.Sprintf("%s_%d", action, time.Now().UnixNano())

	cmd := Command{
		Action: action,
		Data:   data,
		ID:     cmdID,
	}

	// Set target server if specified
	if len(targetServerID) > 0 && targetServerID[0] != "" {
		cmd.ServerID = targetServerID[0]
	}

	select {
	case mc.commandQueue <- cmd:
		return cmdID, nil
	case <-time.After(writeWait):
		return "", fmt.Errorf("command queue full")
	case <-mc.ctx.Done():
		return "", fmt.Errorf("client shutting down")
	}
}

// SendCommandSync sends a command and waits for response
func (mc *MultiServerClient) SendCommandSync(action string, data map[string]interface{}, timeout time.Duration, targetServerID ...string) (Response, error) {
	cmdID, err := mc.SendCommand(action, data, targetServerID...)
	if err != nil {
		return Response{}, err
	}

	// Wait for response
	ctx, cancel := context.WithTimeout(mc.ctx, timeout)
	defer cancel()

	for {
		select {
		case response := <-mc.responseQueue:
			if response.ID == cmdID {
				return response, nil
			}
			// Put back if not our response
			select {
			case mc.responseQueue <- response:
			default:
			}
		case <-ctx.Done():
			return Response{}, fmt.Errorf("timeout waiting for response")
		}
	}
}

// commandDispatcher routes commands to appropriate servers
func (mc *MultiServerClient) commandDispatcher() {
	for {
		select {
		case cmd := <-mc.commandQueue:
			server := mc.selectServer(cmd.ServerID)
			if server == nil {
				log.Printf("No available server for command %s", cmd.ID)
				continue
			}

			// Add server ID to command for response tracking
			cmd.ServerID = server.id

			select {
			case server.send <- cmd:
			case <-time.After(writeWait):
				log.Printf("Server %s send queue full, dropping command %s", server.id, cmd.ID)
			}

		case <-mc.ctx.Done():
			return
		}
	}
}

// selectServer implements load balancing strategies
func (mc *MultiServerClient) selectServer(targetServerID string) *ServerConnection {
	mc.serversMu.RLock()
	defer mc.serversMu.RUnlock()

	// If specific server requested
	if targetServerID != "" {
		if server, exists := mc.servers[targetServerID]; exists && server.isConnected {
			return server
		}
		return nil
	}

	// Get available servers
	var availableServers []*ServerConnection
	for _, server := range mc.servers {
		if server.isConnected {
			availableServers = append(availableServers, server)
		}
	}

	if len(availableServers) == 0 {
		return nil
	}

	// Apply load balancing strategy
	switch mc.strategy {
	case RoundRobin:
		server := availableServers[mc.roundRobinIndex%len(availableServers)]
		mc.roundRobinIndex++
		return server

	case Random:
		return availableServers[rand.Intn(len(availableServers))]

	case LeastConnections:
		// For simplicity, return random (implement actual connection counting)
		return availableServers[rand.Intn(len(availableServers))]

	case HealthBased:
		// Return server with best response time
		bestServer := availableServers[0]
		bestTime := mc.serverStats[bestServer.id].AvgResponseTime

		for _, server := range availableServers[1:] {
			if avgTime := mc.serverStats[server.id].AvgResponseTime; avgTime < bestTime {
				bestServer = server
				bestTime = avgTime
			}
		}
		return bestServer

	default:
		return availableServers[0]
	}
}

// GetConnectedServers returns list of connected server IDs
func (mc *MultiServerClient) GetConnectedServers() []string {
	mc.serversMu.RLock()
	defer mc.serversMu.RUnlock()

	var connected []string
	for id, server := range mc.servers {
		if server.isConnected {
			connected = append(connected, id)
		}
	}
	return connected
}

// GetServerStats returns statistics for a server
func (mc *MultiServerClient) GetServerStats(serverID string) (*ServerStats, error) {
	mc.serversMu.RLock()
	defer mc.serversMu.RUnlock()

	stats, exists := mc.serverStats[serverID]
	if !exists {
		return nil, fmt.Errorf("server %s not found", serverID)
	}
	return stats, nil
}

// healthChecker periodically checks server health
func (mc *MultiServerClient) healthChecker() {
	ticker := time.NewTicker(mc.healthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			mc.serversMu.RLock()
			for serverID := range mc.servers {
				// Send ping to each server
				go func(sid string) {
					_, err := mc.SendCommandSync("ping", nil, 5*time.Second, sid)
					if err != nil {
						log.Printf("Health check failed for server %s: %v", sid, err)
					}
				}(serverID)
			}
			mc.serversMu.RUnlock()

		case <-mc.ctx.Done():
			return
		}
	}
}

// SetCallbacks sets event callbacks
func (mc *MultiServerClient) SetCallbacks(
	onConnect func(serverID string),
	onDisconnect func(serverID string, err error),
	onMessage func(serverID string, response Response),
	onError func(serverID string, err error),
) {
	mc.onConnect = onConnect
	mc.onDisconnect = onDisconnect
	mc.onMessage = onMessage
	mc.onError = onError
}

// Close gracefully shuts down the client
func (mc *MultiServerClient) Close() error {
	log.Println("Shutting down multi-server client...")

	mc.cancel()

	mc.serversMu.Lock()
	for _, server := range mc.servers {
		server.cancel()
	}
	mc.serversMu.Unlock()

	log.Println("Multi-server client shutdown complete")
	return nil
}

// Server connection methods (similar to single client but with server ID tracking)
func (sc *ServerConnection) connectionManager() {
	for {
		select {
		case <-sc.ctx.Done():
			sc.disconnect()
			return
		default:
			if err := sc.connect(); err != nil {
				if sc.client.onError != nil {
					sc.client.onError(sc.id, fmt.Errorf("connection failed: %v", err))
				}

				if !sc.shouldReconnect() {
					return
				}

				sc.waitForReconnect()
				continue
			}

			sc.reconnectAttempts = 0
			sc.reconnectDelay = initialReconnectDelay

			if sc.client.onConnect != nil {
				sc.client.onConnect(sc.id)
			}

			sc.handleConnection()
		}
	}
}

func (sc *ServerConnection) connect() error {
	sc.mu.Lock()
	if sc.isConnecting {
		sc.mu.Unlock()
		return fmt.Errorf("already connecting")
	}
	sc.isConnecting = true
	sc.mu.Unlock()

	defer func() {
		sc.mu.Lock()
		sc.isConnecting = false
		sc.mu.Unlock()
	}()

	u, err := url.Parse(sc.url)
	if err != nil {
		return err
	}

	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}

	sc.mu.Lock()
	sc.conn = conn
	sc.isConnected = true
	sc.lastSeen = time.Now()
	sc.mu.Unlock()

	conn.SetReadLimit(maxMessageSize)
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		sc.mu.Lock()
		sc.lastSeen = time.Now()
		sc.mu.Unlock()
		return nil
	})

	return nil
}

func (sc *ServerConnection) handleConnection() {
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		sc.readPump()
	}()
	go func() {
		defer wg.Done()
		sc.writePump()
	}()

	wg.Wait()
	sc.disconnect()
}

func (sc *ServerConnection) readPump() {
	defer func() {
		if sc.client.onDisconnect != nil {
			sc.client.onDisconnect(sc.id, fmt.Errorf("read pump terminated"))
		}
	}()

	sc.conn.SetReadDeadline(time.Now().Add(pongWait))

	for {
		select {
		case <-sc.ctx.Done():
			return
		default:
			var response Response
			if err := sc.conn.ReadJSON(&response); err != nil {
				return
			}

			// Add server ID to response
			response.ServerID = sc.id

			if sc.client.onMessage != nil {
				sc.client.onMessage(sc.id, response)
			}

			// Forward to main response queue
			select {
			case sc.client.responseQueue <- response:
			default:
			}
		}
	}
}

func (sc *ServerConnection) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-sc.ctx.Done():
			sc.conn.WriteMessage(websocket.CloseMessage, []byte{})
			return

		case cmd := <-sc.send:
			sc.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := sc.conn.WriteJSON(cmd); err != nil {
				return
			}

		case <-ticker.C:
			sc.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := sc.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (sc *ServerConnection) disconnect() {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.conn != nil {
		sc.conn.Close()
		sc.conn = nil
	}
	sc.isConnected = false
}

func (sc *ServerConnection) shouldReconnect() bool {
	return sc.reconnectAttempts < sc.client.maxRetries
}

func (sc *ServerConnection) waitForReconnect() {
	sc.reconnectAttempts++

	select {
	case <-time.After(sc.reconnectDelay):
		sc.reconnectDelay = time.Duration(float64(sc.reconnectDelay) * reconnectBackoff)
		if sc.reconnectDelay > maxReconnectDelay {
			sc.reconnectDelay = maxReconnectDelay
		}
	case <-sc.ctx.Done():
		return
	}
}

// Example usage
func main() {
	// Create multi-server client with round-robin load balancing
	client := NewMultiServerClient(RoundRobin)

	// Set up callbacks
	client.SetCallbacks(
		func(serverID string) {
			log.Printf("✅ Server %s connected", serverID)
		},
		func(serverID string, err error) {
			log.Printf("❌ Server %s disconnected: %v", serverID, err)
		},
		func(serverID string, response Response) {
			log.Printf("📨 Server %s response: %+v", serverID, response)
		},
		func(serverID string, err error) {
			log.Printf("❗ Server %s error: %v", serverID, err)
		},
	)

	// Add multiple ChromeDP servers
	client.AddServer("browser1", "ws://localhost:8080/ws")
	client.AddServer("browser2", "ws://localhost:8081/ws")
	client.AddServer("browser3", "ws://localhost:8082/ws")

	// Start the client
	if err := client.Start(); err != nil {
		log.Fatalf("Failed to start client: %v", err)
	}

	// Wait for connections
	time.Sleep(3 * time.Second)

	// Send commands to specific servers
	client.SendCommand("navigate", map[string]interface{}{"url": "https://google.com"}, "browser1")
	client.SendCommand("navigate", map[string]interface{}{"url": "https://github.com"}, "browser2")

	// Send commands with load balancing (no specific server)
	for i := 0; i < 10; i++ {
		client.SendCommand("ping", nil)
		time.Sleep(500 * time.Millisecond)
	}

	// Synchronous command with timeout
	response, err := client.SendCommandSync("health", nil, 5*time.Second)
	if err != nil {
		log.Printf("Sync command failed: %v", err)
	} else {
		log.Printf("Sync response: %+v", response)
	}

	// Show connected servers
	connected := client.GetConnectedServers()
	log.Printf("Connected servers: %v", connected)

	// Run for a while
	time.Sleep(30 * time.Second)

	// Graceful shutdown
	client.Close()
}
