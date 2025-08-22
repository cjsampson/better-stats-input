package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// create a server with chromedp and send data to it
// 1. create a client/server relationhip and have a
// 2. need to brush up on machine learning and calculus
// 3. understand the websocket load-balancing strategy
// 4. start both services at once
// 5. learn how the routing to the websockets work
// 6. make a more robust api for sending and receiving data

var addr = flag.String("addr", "localhost:8080", "chromedp server address")

func main() {
	flag.Parse()

	u := "ws://" + *addr + "/ws"
	fmt.Printf("Connecting to ChromeDP server at %s\n", u)

	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		log.Fatal("Failed to connect:", err)
	}
	defer c.Close()

	fmt.Println("Connected to ChromeDP server!")
	printHelp()

	// Start response reader goroutine
	go func() {
		for {
			var response Response
			err := c.ReadJSON(&response)
			if err != nil {
				log.Println("Read error:", err)
				return
			}
			printResponse(response)
		}
	}()

	// Main command loop
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")

	for scanner.Scan() {
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			fmt.Print("> ")
			continue
		}

		if input == "help" {
			printHelp()
			fmt.Print("> ")
			continue
		}

		if input == "quit" || input == "exit" {
			break
		}

		cmd := parseCommand(input)
		if cmd != nil {
			err := c.WriteJSON(cmd)
			if err != nil {
				log.Println("Write error:", err)
				break
			}
		}
		fmt.Print("> ")
	}
}

func parseCommand(input string) *Command {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return nil
	}

	cmd := &Command{
		Action: parts[0],
		Data:   make(map[string]interface{}),
		ID:     fmt.Sprintf("cmd_%d", time.Now().UnixNano()),
	}

	switch parts[0] {
	case "navigate", "nav":
		if len(parts) < 2 {
			fmt.Println("Usage: navigate <url>")
			return nil
		}
		cmd.Action = "navigate"
		cmd.Data["url"] = parts[1]

	case "click":
		if len(parts) < 2 {
			fmt.Println("Usage: click <selector>")
			return nil
		}
		cmd.Data["selector"] = parts[1]

	case "move_cursor", "hover":
		if len(parts) < 2 {
			fmt.Println("Usage: move_cursor <selector>")
			return nil
		}
		cmd.Action = "move_cursor"
		cmd.Data["selector"] = parts[1]

	case "type":
		if len(parts) < 3 {
			fmt.Println("Usage: type <selector> <text>")
			return nil
		}
		cmd.Data["selector"] = parts[1]
		cmd.Data["text"] = strings.Join(parts[2:], " ")

	case "get_text":
		if len(parts) < 2 {
			fmt.Println("Usage: get_text <selector>")
			return nil
		}
		cmd.Data["selector"] = parts[1]

	case "get_outer_html", "get_html":
		if len(parts) < 2 {
			fmt.Println("Usage: get_outer_html <selector>")
			return nil
		}
		cmd.Action = "get_outer_html"
		cmd.Data["selector"] = parts[1]

	case "wait":
		if len(parts) < 2 {
			fmt.Println("Usage: wait <selector> [timeout_seconds]")
			return nil
		}
		cmd.Data["selector"] = parts[1]
		if len(parts) > 2 {
			if timeout, err := strconv.ParseFloat(parts[2], 64); err == nil {
				cmd.Data["timeout"] = timeout
			}
		}

	case "screenshot":
		// No additional parameters needed

	case "get_page_info", "info":
		cmd.Action = "get_page_info"
		// No additional parameters needed

	case "json":
		// Allow sending raw JSON commands
		if len(parts) < 2 {
			fmt.Println("Usage: json <json_string>")
			return nil
		}
		jsonStr := strings.Join(parts[1:], " ")
		var rawCmd Command
		if err := json.Unmarshal([]byte(jsonStr), &rawCmd); err != nil {
			fmt.Printf("Invalid JSON: %v\n", err)
			return nil
		}
		return &rawCmd

	default:
		fmt.Printf("Unknown command: %s. Type 'help' for available commands.\n", parts[0])
		return nil
	}

	return cmd
}

func printResponse(response Response) {
	fmt.Printf("\n--- Response [ID: %s] ---\n", response.ID)
	if response.Success {
		fmt.Printf("✓ Success: %v\n", response.Data)
	} else {
		fmt.Printf("✗ Error: %s\n", response.Error)
	}
	fmt.Println("--- End Response ---")
}

func printHelp() {
	fmt.Println("\n=== ChromeDP Client Commands ===")
	fmt.Println("navigate <url>           - Navigate to URL")
	fmt.Println("click <selector>         - Click element")
	fmt.Println("move_cursor <selector>   - Move cursor to element")
	fmt.Println("type <selector> <text>   - Type text into element")
	fmt.Println("get_text <selector>      - Get element text")
	fmt.Println("get_outer_html <selector> - Get element HTML")
	fmt.Println("wait <selector> [timeout] - Wait for element")
	fmt.Println("screenshot               - Take screenshot")
	fmt.Println("get_page_info           - Get page title and URL")
	fmt.Println("json <json_string>      - Send raw JSON command")
	fmt.Println("help                    - Show this help")
	fmt.Println("quit/exit               - Exit client")
	fmt.Println("\nExamples:")
	fmt.Println("  navigate https://google.com")
	fmt.Println("  click input[name='q']")
	fmt.Println("  type input[name='q'] hello world")
	fmt.Println("  wait .search-results 5")
	fmt.Println("  json {\"action\":\"click\",\"data\":{\"selector\":\"#button\"}}")
	fmt.Println("=====================================\n")
}
