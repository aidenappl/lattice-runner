package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/aidenappl/lattice-runner/telemetry"
	"github.com/gorilla/websocket"
)

// Envelope is the standard message format from the orchestrator.
type Envelope struct {
	Type      string `json:"type"`
	CommandID string `json:"command_id,omitempty"`
	// RequestID and TraceID carry the orchestrator's correlation ids for the
	// command. An older orchestrator omits them; the runner then behaves as
	// it always has.
	RequestID string         `json:"request_id,omitempty"`
	TraceID   string         `json:"trace_id,omitempty"`
	WorkerID  string         `json:"worker_id,omitempty"`
	IssuedAt  *time.Time     `json:"issued_at,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// OutgoingMessage is sent from the runner to the orchestrator.
type OutgoingMessage struct {
	Type      string `json:"type"`
	CommandID string `json:"command_id,omitempty"`
	// RequestID and TraceID echo the ids of the command a reply belongs to.
	// Unsolicited messages (heartbeat, container_sync, metrics, logs) leave
	// them empty.
	RequestID string         `json:"request_id,omitempty"`
	TraceID   string         `json:"trace_id,omitempty"`
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

	// dropsMu guards drops: messages dropped for want of queue room, by
	// message type, since the last TakeDrops.
	dropsMu sync.Mutex
	drops   map[string]int64
}

func NewWSClient(orchestratorURL, token string, reconnectInterval time.Duration) *WSClient {
	return &WSClient{
		url:               orchestratorURL,
		token:             token,
		send:              make(chan []byte, 256),
		reconnectInterval: reconnectInterval,
	}
}

func (c *WSClient) OnMessage(handler func(Envelope)) {
	c.onMessage = handler
}

// SendJSON enqueues a message on a best-effort basis: if the send queue is full
// the message is DROPPED. Use this only for pure telemetry (heartbeat, metrics,
// container_sync, logs, lifecycle_log, progress) where a dropped message is
// tolerable. For a reply the orchestrator blocks on, use SendJSONReliable.
func (c *WSClient) SendJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	select {
	case c.send <- b:
		return nil
	default:
		mt := messageType(b)
		c.countDrop(mt)
		slog.WarnContext(context.Background(), "ws send queue full, dropping message", "component", "ws",
			"queue_len", len(c.send), "queue_cap", cap(c.send), "message_type", mt)
		return ErrSendQueueFull
	}
}

// ErrSendQueueFull is returned (wrapped, for SendJSONReliable) when a message is
// dropped because the send queue had no room. The drop has already been logged
// once, with the message type, so callers should not log it again.
var ErrSendQueueFull = errors.New("send queue full")

// messageType reads the "type" field of an encoded message for drop logs. Many
// callers pass OutgoingMessage, but SendJSON accepts any value, so it is read
// back from the JSON rather than type-asserted.
func messageType(b []byte) string {
	var m struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(b, &m) != nil || m.Type == "" {
		return "untyped"
	}
	return m.Type
}

// SendJSONReliable enqueues a message, blocking until the send queue has room or
// the timeout elapses (rather than dropping immediately when full). Use this for
// command_id-correlated replies that lattice-api blocks waiting on — exec_output,
// list_volumes_response, list_networks_response, backup_dest_test_result,
// db_delete_snapshot_result and the db_*_status replies — so a burst of telemetry
// can't silently drop the one message a caller is awaiting. Still bounded: if the
// write pump is gone (disconnected) the send can't complete and it gives up after
// reliableSendTimeout instead of blocking forever.
func (c *WSClient) SendJSONReliable(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	select {
	case c.send <- b:
		return nil
	case <-time.After(reliableSendTimeout):
		mt := messageType(b)
		c.countDrop(mt)
		slog.WarnContext(context.Background(), "ws reliable send timed out, dropping message", "component", "ws",
			"timeout_ms", reliableSendTimeout.Milliseconds(), "queue_len", len(c.send), "queue_cap", cap(c.send),
			"message_type", mt)
		return fmt.Errorf("%w after %v", ErrSendQueueFull, reliableSendTimeout)
	}
}

// countDrop records one message of type msgType dropped for want of queue room.
func (c *WSClient) countDrop(msgType string) {
	c.dropsMu.Lock()
	defer c.dropsMu.Unlock()
	if c.drops == nil {
		c.drops = map[string]int64{}
	}
	c.drops[msgType]++
}

// TakeDrops returns the messages dropped since the last call, by message type,
// and starts counting afresh. The map is never nil.
func (c *WSClient) TakeDrops() map[string]int64 {
	c.dropsMu.Lock()
	defer c.dropsMu.Unlock()
	d := c.drops
	c.drops = nil
	if d == nil {
		d = map[string]int64{}
	}
	return d
}

// QueueStats reports how many messages are waiting in the send queue and its
// capacity.
func (c *WSClient) QueueStats() (length, capacity int) {
	return len(c.send), cap(c.send)
}

// reliableSendTimeout bounds how long SendJSONReliable waits for queue room.
const reliableSendTimeout = 10 * time.Second

// Connect establishes a WebSocket connection and maintains it with auto-reconnect.
func (c *WSClient) Connect(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := c.dial(ctx); err != nil {
			slog.WarnContext(ctx, "ws connection failed, will reconnect", "component", "ws", "error", err)
			slog.InfoContext(ctx, "ws reconnecting", "component", "ws", "backoff", c.reconnectInterval.String())
			select {
			case <-time.After(c.reconnectInterval):
			case <-ctx.Done():
				return
			}
			continue
		}

		slog.InfoContext(ctx, "ws connected to orchestrator", "component", "ws")
		c.run(ctx)
		// A disconnect caused by shutdown is expected; any other is a warning.
		if ctx.Err() != nil {
			slog.InfoContext(ctx, "ws disconnected from orchestrator", "component", "ws")
		} else {
			slog.WarnContext(ctx, "ws disconnected from orchestrator", "component", "ws")
		}

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

	// A pump panic still ends the process, as it always has — systemd restarts
	// the runner clean. CrashGuard only makes sure the report leaves first.
	go func() {
		defer wg.Done()
		defer telemetry.CrashGuard("ws.read_pump")
		c.readPump(runCtx, cancel)
	}()

	go func() {
		defer wg.Done()
		defer telemetry.CrashGuard("ws.write_pump")
		c.writePump(runCtx)
	}()

	wg.Wait()
	c.close()
}

func (c *WSClient) readPump(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()

	// Deploy payloads can be large (many containers/env vars/networks/volumes).
	// Keep this comfortably above expected command size.
	c.conn.SetReadLimit(4 * 1024 * 1024)
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
				slog.WarnContext(ctx, "ws read error, will reconnect", "component", "ws", "error", err)
			}
			return
		}

		var env Envelope
		if err := json.Unmarshal(payload, &env); err != nil {
			slog.WarnContext(ctx, "ws invalid json from orchestrator", "component", "ws", "error", err)
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
				slog.WarnContext(ctx, "ws write error, will reconnect", "component", "ws", "error", err)
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
				slog.WarnContext(ctx, "ws ping write error, will reconnect", "component", "ws", "error", err)
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

// Drain blocks until the outbound send queue is empty or timeout elapses.
// Call this before Close() on a graceful shutdown so queued messages are
// transmitted before the connection drops.
func (c *WSClient) Drain(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(c.send) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
