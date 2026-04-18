package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Envelope is the standard message format from the orchestrator.
type Envelope struct {
	Type      string         `json:"type"`
	CommandID string         `json:"command_id,omitempty"`
	WorkerID  string         `json:"worker_id,omitempty"`
	IssuedAt  *time.Time     `json:"issued_at,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// OutgoingMessage is sent from the runner to the orchestrator.
type OutgoingMessage struct {
	Type      string         `json:"type"`
	CommandID string         `json:"command_id,omitempty"`
	Status    string         `json:"status,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
}

type WSClient struct {
	url               string
	token             string
	conn              *websocket.Conn
	send              chan []byte
	reconnectInterval time.Duration
	onMessage         func(Envelope)

	mu     sync.Mutex
	closed bool
}

func NewWSClient(orchestratorURL, token string, reconnectInterval time.Duration) *WSClient {
	return &WSClient{
		url:               orchestratorURL,
		token:             token,
		send:              make(chan []byte, 64),
		reconnectInterval: reconnectInterval,
	}
}

func (c *WSClient) OnMessage(handler func(Envelope)) {
	c.onMessage = handler
}

func (c *WSClient) SendJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	select {
	case c.send <- b:
		return nil
	default:
		return fmt.Errorf("send queue full")
	}
}

// Connect establishes a WebSocket connection and maintains it with auto-reconnect.
func (c *WSClient) Connect(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := c.dial(ctx); err != nil {
			log.Printf("ws: connection failed: %v", err)
			log.Printf("ws: reconnecting in %v...", c.reconnectInterval)
			select {
			case <-time.After(c.reconnectInterval):
			case <-ctx.Done():
				return
			}
			continue
		}

		log.Println("ws: connected to orchestrator")
		c.run(ctx)
		log.Println("ws: disconnected from orchestrator")

		select {
		case <-time.After(c.reconnectInterval):
		case <-ctx.Done():
			return
		}
	}
}

func (c *WSClient) dial(ctx context.Context) error {
	u, err := url.Parse(c.url)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}

	q := u.Query()
	q.Set("token", c.token)
	u.RawQuery = q.Encode()

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.closed = false
	c.mu.Unlock()

	return nil
}

func (c *WSClient) run(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		c.readPump(runCtx, cancel)
	}()

	go func() {
		defer wg.Done()
		c.writePump(runCtx)
	}()

	wg.Wait()
	c.close()
}

func (c *WSClient) readPump(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()

	c.conn.SetReadLimit(64 * 1024)
	_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, payload, err := c.conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("ws: read error: %v", err)
			}
			return
		}

		var env Envelope
		if err := json.Unmarshal(payload, &env); err != nil {
			log.Printf("ws: invalid json from orchestrator: %v", err)
			continue
		}

		if c.onMessage != nil {
			c.onMessage(env)
		}
	}
}

func (c *WSClient) writePump(ctx context.Context) {
	ticker := time.NewTicker(54 * time.Second) // ping period
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case msg := <-c.send:
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err := c.conn.WriteMessage(websocket.TextMessage, msg)
			c.mu.Unlock()
			if err != nil {
				log.Printf("ws: write error: %v", err)
				return
			}

		case <-ticker.C:
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err := c.conn.WriteMessage(websocket.PingMessage, nil)
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (c *WSClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed && c.conn != nil {
		c.closed = true
		_ = c.conn.Close()
	}
}

func (c *WSClient) Close() {
	c.close()
}
