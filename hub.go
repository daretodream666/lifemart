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

// Envelope - обёртка для сообщений
type Envelope struct {
	Type    string          `json:"type"`
	Room    string          `json:"room"`
	Payload json.RawMessage `json:"payload"`
}

// Message - структура сообщения в чате
type Message struct {
	Username  string `json:"username"`
	Text      string `json:"text"`
	Timestamp int64  `json:"timestamp"`
}

// TypingStatus - структура сообщения User is typing
type TypingStatus struct {
	User   string `json:"user"`
	Status bool   `json:"status"`
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
			var env Envelope
			if err := json.Unmarshal(message, &env); err != nil {
				log.Println("unmarshal envelope error:", err)
				continue
			}
			// Сохраняем сообщение в "базу" и ловим ошибки
			if env.Type == "chat_message" {
				var msg Message
				if err := json.Unmarshal(env.Payload, &msg); err == nil {
					if err := h.db.SaveMessage(msg); err != nil {
						log.Println("SaveMessage error:", err)
					}
				}
			}
			// Рассылаем всем в комнате
			for client := range h.rooms[env.Room] {
				if _, ok := h.clients[client]; !ok {
					continue // клиент уже удалён
				}
				select {
				case client.send <- message:
				default:
					// Если буфер отправки полон, клиент отключается
					h.unregister <- client // Отключение клиента через unregister
				}
			}
		}
	}
}

// readPump читает сообщения от клиента.
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}() // При ошибке в ReadMessage выходим из readPump, убираем пользователя, закрываем сокет
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		// Получаем конверт
		var wrap Envelope
		if err := json.Unmarshal(message, &wrap); err != nil {
			log.Println("unmarshal wrap error:", err)
		}
		// Смотрим тип сообщения
		switch wrap.Type {
		// Расркываем конверт с сообщением, добавляем время, пакуем, отправляем
		case "chat_message":
			var msg Message
			if err := json.Unmarshal(wrap.Payload, &msg); err != nil {
				log.Println("Unmarshal chat message error:", err)
			}
			msg.Timestamp = time.Now().Unix()
			jsonMsg, _ := json.Marshal(struct {
				Type    string  `json:"type"`
				Room    string  `json:"room"`
				Payload Message `json:"payload"`
			}{
				Type:    wrap.Type,
				Room:    wrap.Room,
				Payload: msg,
			})

			c.hub.broadcast <- jsonMsg
		// Пользователь печатает, упаковали, отправили
		case "typing_status":
			var ts TypingStatus
			if err := json.Unmarshal(wrap.Payload, &ts); err != nil {
				log.Println("typing status unmarshal error:", err)
			}

			jsonMsg, _ := json.Marshal(struct {
				Type    string       `json:"type"`
				Room    string       `json:"room"`
				Payload TypingStatus `json:"payload"`
			}{
				Type:    wrap.Type,
				Room:    wrap.Room,
				Payload: ts,
			})

			c.hub.broadcast <- jsonMsg

		}
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
		err := c.conn.WriteMessage(websocket.TextMessage, message)
		if err != nil {
			log.Println("WriteMessage error: ")
			return
		}
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

	// Получаем последние сообщения
	lastMessages, err := hub.db.GetLastMessages(client.room, 10)
	if err != nil {
		log.Println("GetLastMessages error:", err)
	} else {
		// Запаковываем каждое последнее сообщение в конверт и отправляем
		for _, msg := range lastMessages {
			env := struct {
				Type    string  `json:"type"`
				Room    string  `json:"room"`
				Payload Message `json:"payload"`
			}{
				Type:    "chat_message",
				Room:    client.room,
				Payload: msg,
			}

			jsonMsg, err := json.Marshal(env)
			if err != nil {
				log.Println("LastMessage Marshal error:")
				continue
			}

			client.send <- jsonMsg
		}
	}
	// Только после этого клиент регистрируется и может получать новые сообщения
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
		{Username: "Alice", Text: "Hello!", Timestamp: time.Now().Unix() - 10},
		{Username: "Bob", Text: "Hi Alice!", Timestamp: time.Now().Unix() - 5},
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
