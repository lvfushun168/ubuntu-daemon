package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"openclaw/dameon/internal/config"
	"openclaw/dameon/internal/protocol"
)

type Handler interface {
	Handle(ctx context.Context, envelope protocol.Envelope)
}

type Client struct {
	cfg           *config.Config
	logger        *log.Logger
	handler       Handler
	dialer        *websocket.Dialer
	mu            sync.RWMutex
	conn          *websocket.Conn
	authenticated bool
}

func NewClient(cfg *config.Config, logger *log.Logger, handler Handler) *Client {
	return &Client{
		cfg:     cfg,
		logger:  logger,
		handler: handler,
		dialer: &websocket.Dialer{
			HandshakeTimeout: cfg.Cloud.ConnectTimeout(),
		},
	}
}

func (c *Client) Run(ctx context.Context) error {
	backoff := c.cfg.Cloud.ReconnectMin()
	for {
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		c.logger.Printf("cloud connection closed: %v", err)
		wait := withJitter(backoff)
		c.logger.Printf("retry cloud connection after %s", wait)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff *= 2
		if backoff > c.cfg.Cloud.ReconnectMax() {
			backoff = c.cfg.Cloud.ReconnectMax()
		}
	}
}

func (c *Client) runOnce(ctx context.Context) error {
	conn, _, err := c.dialer.DialContext(ctx, c.cfg.CloudWSURL(), http.Header{})
	if err != nil {
		return fmt.Errorf("dial cloud websocket: %w", err)
	}
	defer conn.Close()

	c.setConnection(conn, false)
	defer c.setConnection(nil, false)

	if err := c.sendAuth(ctx); err != nil {
		return err
	}

	pingTicker := time.NewTicker(c.cfg.Cloud.PingInterval())
	defer pingTicker.Stop()

	readErr := make(chan error, 1)
	go func() {
		readErr <- c.readLoop(ctx, conn)
	}()

	for {
		select {
		case <-ctx.Done():
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutdown"), time.Now().Add(3*time.Second))
			return nil
		case err := <-readErr:
			return err
		case <-pingTicker.C:
			if err := c.sendPing(ctx); err != nil {
				return err
			}
		}
	}
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}

		var ping protocol.PingMessage
		if err := json.Unmarshal(data, &ping); err == nil && ping.Type == protocol.TypePong {
			continue
		}

		var envelope protocol.Envelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			c.logger.Printf("ignore invalid cloud message: %v", err)
			continue
		}

		if envelope.Type == protocol.TypeAuthResp {
			var authResp protocol.AuthResponse
			if err := json.Unmarshal(envelope.Payload, &authResp); err == nil && authResp.Code == 200 {
				c.setAuthenticated(true)
			}
		}

		c.handler.Handle(ctx, envelope)
	}
}

func (c *Client) SendEnvelope(ctx context.Context, msgID, msgType string, payload interface{}) error {
	body := protocol.Envelope{
		MsgID:     msgID,
		Type:      msgType,
		Timestamp: time.Now().UnixMilli(),
	}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body.Payload = raw
	}
	return c.writeJSON(ctx, body)
}

func (c *Client) sendAuth(ctx context.Context) error {
	return c.SendEnvelope(ctx, fmt.Sprintf("auth-%d", time.Now().UnixMilli()), protocol.TypeAuthReq, protocol.AuthRequest{
		DeviceID: c.cfg.DeviceID,
		Version:  c.cfg.DaemonVersion,
	})
}

func (c *Client) sendPing(ctx context.Context) error {
	return c.writeJSON(ctx, protocol.PingMessage{Type: protocol.TypePing})
}

func (c *Client) writeJSON(ctx context.Context, value interface{}) error {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return errors.New("cloud websocket is not connected")
	}
	_ = ctx
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteJSON(value)
}

func (c *Client) setConnection(conn *websocket.Conn, authenticated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn = conn
	c.authenticated = authenticated
}

func (c *Client) setAuthenticated(value bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authenticated = value
}

func withJitter(base time.Duration) time.Duration {
	if base <= 0 {
		return time.Second
	}
	jitter := 0.2 + rand.Float64()*0.2
	return time.Duration(math.Round(float64(base) * (1 + jitter)))
}
