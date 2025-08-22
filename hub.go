// hub.go
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// --- Модель и интерфейс для работы с данными ---

// Message - структура сообщения в чате
type Message struct {
	Room      string `json:"room"`  // Комната для сообщения
	Username  string `json:"username"` // Юзернейм отправителя
	Text      string `json:"text"` // Текст сообщения
	Timestamp int64  `json:"timestamp"` // Время отправки сообщения(timestamp)
}

// Repository - интерфейс для работы с хранилищем сообщений
type Repository interface {
	SaveMessage(msg Message) error
	GetLastMessages(room string, count int) ([]Message, error)
}

// --- WebSocket часть ---

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Client - представляет одного WebSocket пользователя.
type Client struct {
	conn     *websocket.Conn
	hub      *Hub
	send     chan []byte
	room     string
	username string
}

// Hub - управляет всеми клиентами и комнатами.
type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	rooms      map[string]map[*Client]bool
	db         Repository
}

func NewHub(db Repository) *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		rooms:      make(map[string]map[*Client]bool),
		db:         db,
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			if h.rooms[client.room] == nil {
				h.rooms[client.room] = make(map[*Client]bool)
			}
			h.rooms[client.room][client] = true

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}

		case message := <-h.broadcast:
			var msg Message
			json.Unmarshal(message, &msg)

			// Сохраняем сообщение в "базу"
			h.db.SaveMessage(msg)

			// Рассылаем всем в комнате
			for client := range h.rooms[msg.Room] {
				select {
				case client.send <- message:
				default:
					// Если буфер отправки полон, клиент отключается
					close(client.send)
					delete(h.clients, client)
				}
			}
		}
	}
}

// readPump читает сообщения от клиента.
func (c *Client) readPump() {
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			c.hub.unregister <- c
			break
		}

		// Обертываем сообщение в структуру Message
		msg := Message{
			Room:      c.room,
			Username:  c.username,
			Text:      string(message),
			Timestamp: time.Now().Unix(),
		}
		
		jsonMsg, _ := json.Marshal(msg)
		c.hub.broadcast <- jsonMsg
	}
}

// writePump отправляет сообщения клиенту.
func (c *Client) writePump() {
	defer c.conn.Close()
	for {
		message, ok := <-c.send
		if !ok {
			// Канал `send` закрыт.
			c.conn.WriteMessage(websocket.CloseMessage, []byte{})
			return
		}
		c.conn.WriteMessage(websocket.TextMessage, message)
	}
}

// serveWs обрабатывает http запрос и обновляет его до WebSocket.
func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	room := r.URL.Query().Get("room")
	username := r.URL.Query().Get("username")
	if room == "" || username == "" {
		http.Error(w, "Room and username are required", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	
	client := &Client{
		conn:     conn,
		hub:      hub,
		send:     make(chan []byte, 256),
		room:     room,
		username: username,
	}
	client.hub.register <- client

	go client.writePump()
	go client.readPump()
}

// --- Точка входа и мок базы данных ---
type MockDB struct{}

func (m *MockDB) SaveMessage(msg Message) error {
	log.Printf("Сообщение сохранено: %+v\n", msg)
	return nil // Всегда успешно
}

func (m *MockDB) GetLastMessages(room string, count int) ([]Message, error) {
	log.Printf("Запрошены последние %d сообщений для комнаты %s\n", count, room)
	// Возвращаем несколько тестовых сообщений
	return []Message{
		{Room: room, Username: "Alice", Text: "Hello!", Timestamp: time.Now().Unix() - 10},
		{Room: room, Username: "Bob", Text: "Hi Alice!", Timestamp: time.Now().Unix() - 5},
	}, nil
}

func main() {
	db := &MockDB{}
	hub := NewHub(db)
	go hub.Run()
	
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})

	log.Println("Сервер запущен на :8080")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}