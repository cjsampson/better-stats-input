package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// create a server with chromedp and send data to it
// 1. create a client/server relationhip and have a
// 2. need to brush up on machine learning and calculus

var addr = flag.String("addr", "localhost:8080", "http service address")
var mode = flag.String("mode", "client", "host or client")

var upgrader = websocket.Upgrader{}
var clients = make(map[*websocket.Conn]bool)
var broadcast = make(chan string)
var mutex sync.Mutex

func main() {
	flag.Parse()

	go broadcaster()

	if *mode == "host" {
		go startServer()
		// Give the server a moment to start
		time.Sleep(1 * time.Second)
	}

	client()
}

func startServer() {
	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/log", wsLog)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func broadcaster() {
	for {
		msg := <-broadcast
		mutex.Lock()
		for client := range clients {

			err := client.WriteMessage(websocket.TextMessage, []byte(msg))
			if err != nil {
				client.Close()
				delete(clients, client)
			}
		}
		mutex.Unlock()
	}
}

func wsLogHandler(w http.ResponseWriter, r *http.Request) {

}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}
	mutex.Lock()
	clients[conn] = true
	mutex.Unlock()
	handleClient(conn)
}

func handleClient(conn *websocket.Conn) {
	defer func() {
		mutex.Lock()
		conn.Close()
		delete(clients, conn)
		mutex.Unlock()
	}()

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			break
		}
		broadcast <- string(message)
	}
}

func client() {
	u := "ws://" + *addr + "/ws"
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		log.Fatal("dial:", err)
	}
	defer c.Close()

	go func() {
		for {
			_, message, err := c.ReadMessage()
			if err != nil {
				log.Println("read:", err)
				return
			}
			fmt.Printf("%s\n", message)
		}
	}()

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		err := c.WriteMessage(websocket.TextMessage, []byte(scanner.Text()))
		if err != nil {
			log.Println("write:", err)
			return
		}
	}
}
