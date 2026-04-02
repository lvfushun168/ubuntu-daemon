package gateway

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"openclaw/dameon/internal/config"
	"openclaw/dameon/internal/protocol"
)

func TestAdapterChatCompletesGatewayHandshakeAndStreamsReply(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
			t.Fatalf("unexpected authorization header: %s", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]interface{}{
			"type":  "event",
			"event": "connect.challenge",
			"payload": map[string]interface{}{
				"nonce": "nonce-1",
			},
		}); err != nil {
			t.Fatalf("write challenge: %v", err)
		}

		var connectReq map[string]interface{}
		if err := conn.ReadJSON(&connectReq); err != nil {
			t.Fatalf("read connect request: %v", err)
		}
		if connectReq["method"] != "connect" {
			t.Fatalf("unexpected connect method: %#v", connectReq)
		}
		connectParams := connectReq["params"].(map[string]interface{})
		if connectParams["minProtocol"] != float64(3) || connectParams["maxProtocol"] != float64(3) {
			t.Fatalf("unexpected protocol params: %#v", connectParams)
		}
		auth := connectParams["auth"].(map[string]interface{})
		if auth["token"] != "token-1" {
			t.Fatalf("unexpected connect token: %#v", auth)
		}
		client := connectParams["client"].(map[string]interface{})
		if client["id"] != "cli" || client["mode"] != "cli" {
			t.Fatalf("unexpected client params: %#v", client)
		}
		if err := conn.WriteJSON(map[string]interface{}{
			"type":    "res",
			"id":      connectReq["id"],
			"ok":      true,
			"payload": map[string]interface{}{"status": "ok"},
		}); err != nil {
			t.Fatalf("write connect response: %v", err)
		}

		var chatReq map[string]interface{}
		if err := conn.ReadJSON(&chatReq); err != nil {
			t.Fatalf("read chat request: %v", err)
		}
		if chatReq["method"] != "chat.send" {
			t.Fatalf("unexpected chat method: %#v", chatReq)
		}
		chatParams := chatReq["params"].(map[string]interface{})
		if chatParams["sessionKey"] != "session-1" {
			t.Fatalf("unexpected session key: %#v", chatParams)
		}
		if chatParams["idempotencyKey"] != "msg-1" {
			t.Fatalf("unexpected idempotency key: %#v", chatParams)
		}
		if chatParams["text"] != "hello" {
			t.Fatalf("unexpected chat params: %#v", chatParams)
		}
		if err := conn.WriteJSON(map[string]interface{}{
			"type": "res",
			"id":   chatReq["id"],
			"ok":   true,
			"payload": map[string]interface{}{
				"runId":  "run-1",
				"status": "started",
			},
		}); err != nil {
			t.Fatalf("write chat response: %v", err)
		}

		events := []map[string]interface{}{
			{
				"type":  "event",
				"event": "chat",
				"payload": map[string]interface{}{
					"runId":      "run-1",
					"sessionKey": "session-1",
					"delta":      "Hi",
					"status":     "streaming",
				},
			},
			{
				"type":  "event",
				"event": "chat",
				"payload": map[string]interface{}{
					"runId":      "run-1",
					"sessionKey": "session-1",
					"delta":      " there",
					"status":     "done",
				},
			},
		}
		for _, event := range events {
			if err := conn.WriteJSON(event); err != nil {
				t.Fatalf("write chat event: %v", err)
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	adapter := newTestAdapter(t, wsURL, "token-1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "session-1",
		Role:      "user",
		Text:      "hello",
		Stream:    true,
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-1",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 2 {
		data, _ := json.Marshal(replies)
		t.Fatalf("expected 2 replies, got %d %s", len(replies), string(data))
	}
	if replies[0].Text != "Hi" || replies[0].ChunkSeq != 1 || replies[0].IsEnd {
		t.Fatalf("unexpected first reply: %+v", replies[0])
	}
	if replies[1].Text != " there" || !replies[1].IsFinal || !replies[1].IsEnd || replies[1].FinishReason != "stop" {
		t.Fatalf("unexpected final reply: %+v", replies[1])
	}
}

func TestAdapterChatFailsWhenGatewayTokenMissing(t *testing.T) {
	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")
	if err := os.WriteFile(envPath, []byte("OTHER_KEY=value\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	cfg := &config.Config{
		DeviceID:      "device-1",
		DaemonVersion: "0.1.0",
		OpenClaw: config.OpenClawConfig{
			EnvFile:            envPath,
			GatewayWSURL:       "ws://127.0.0.1:18789",
			GatewayTokenEnvKey: "OPENCLAW_GATEWAY_TOKEN",
		},
	}

	adapter := NewAdapter(cfg, log.New(os.Stdout, "", 0))
	_, err := adapter.Chat(context.Background(), protocol.ChatMessagePayload{
		SessionID: "session-1",
		Role:      "user",
		Text:      "hello",
	})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected missing token error, got %v", err)
	}
}

func newTestAdapter(t *testing.T, wsURL, token string) *Adapter {
	t.Helper()

	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")
	if err := os.WriteFile(envPath, []byte("OPENCLAW_GATEWAY_TOKEN='"+token+"'\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	cfg := &config.Config{
		DeviceID:      "device-1",
		DaemonVersion: "0.1.0",
		OpenClaw: config.OpenClawConfig{
			EnvFile:            envPath,
			GatewayWSURL:       wsURL,
			GatewayTokenEnvKey: "OPENCLAW_GATEWAY_TOKEN",
		},
	}
	return NewAdapter(cfg, log.New(os.Stdout, "", 0))
}
