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
	"openclaw/dameon/internal/store"
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
		device := connectParams["device"].(map[string]interface{})
		if device["id"] == "" || device["publicKey"] == "" || device["signature"] == "" || device["nonce"] != "nonce-1" {
			t.Fatalf("unexpected device params: %#v", device)
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
		if err := conn.WriteJSON(map[string]interface{}{
			"type": "hello-ok",
			"auth": map[string]interface{}{
				"deviceToken": "device-token-1",
				"role":        "operator",
				"scopes":      []string{"operator.read", "operator.write"},
			},
		}); err != nil {
			t.Fatalf("write hello-ok: %v", err)
		}

		var subscribeReq map[string]interface{}
		if err := conn.ReadJSON(&subscribeReq); err != nil {
			t.Fatalf("read subscribe request: %v", err)
		}
		if subscribeReq["method"] != "sessions.messages.subscribe" {
			t.Fatalf("unexpected subscribe method: %#v", subscribeReq)
		}
		subscribeParams := subscribeReq["params"].(map[string]interface{})
		if subscribeParams["key"] != "session-1" {
			t.Fatalf("unexpected subscribe params: %#v", subscribeParams)
		}
		if err := conn.WriteJSON(map[string]interface{}{
			"type": "res",
			"id":   subscribeReq["id"],
			"ok":   true,
			"payload": map[string]interface{}{
				"subscribed": true,
				"key":        "session-1",
			},
		}); err != nil {
			t.Fatalf("write subscribe response: %v", err)
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
		if chatParams["message"] != "hello" {
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
	state, err := store.NewFileStore(filepath.Join(filepath.Dir(adapter.cfg.Store.StateFile), filepath.Base(adapter.cfg.Store.StateFile))).Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.GatewayDeviceToken != "device-token-1" {
		t.Fatalf("expected persisted device token, got %+v", state)
	}
}

func TestAdapterChatIgnoresUnexpectedEventAfterConnect(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]interface{}{
			"type":  "event",
			"event": "connect.challenge",
			"payload": map[string]interface{}{
				"nonce": "nonce-2",
			},
		}); err != nil {
			t.Fatalf("write challenge: %v", err)
		}

		var connectReq map[string]interface{}
		if err := conn.ReadJSON(&connectReq); err != nil {
			t.Fatalf("read connect request: %v", err)
		}

		if err := conn.WriteJSON(map[string]interface{}{
			"type":    "res",
			"id":      connectReq["id"],
			"ok":      true,
			"payload": map[string]interface{}{"status": "ok"},
		}); err != nil {
			t.Fatalf("write connect response: %v", err)
		}
		if err := conn.WriteJSON(map[string]interface{}{
			"type":  "event",
			"event": "node.changed",
			"payload": map[string]interface{}{
				"nodeId": "n-1",
			},
		}); err != nil {
			t.Fatalf("write unexpected post-connect event: %v", err)
		}

		var subscribeReq map[string]interface{}
		if err := conn.ReadJSON(&subscribeReq); err != nil {
			t.Fatalf("read subscribe request: %v", err)
		}
		if subscribeReq["method"] != "sessions.messages.subscribe" {
			t.Fatalf("unexpected subscribe method: %#v", subscribeReq)
		}
		if err := conn.WriteJSON(map[string]interface{}{
			"type": "res",
			"id":   subscribeReq["id"],
			"ok":   true,
			"payload": map[string]interface{}{
				"subscribed": true,
				"key":        "session-2",
			},
		}); err != nil {
			t.Fatalf("write subscribe response: %v", err)
		}

		var chatReq map[string]interface{}
		if err := conn.ReadJSON(&chatReq); err != nil {
			t.Fatalf("read chat request: %v", err)
		}
		if chatReq["method"] != "chat.send" {
			t.Fatalf("unexpected chat method: %#v", chatReq)
		}
		if err := conn.WriteJSON(map[string]interface{}{
			"type": "res",
			"id":   chatReq["id"],
			"ok":   true,
			"payload": map[string]interface{}{
				"runId": "run-2",
			},
		}); err != nil {
			t.Fatalf("write chat response: %v", err)
		}
		if err := conn.WriteJSON(map[string]interface{}{
			"type":  "event",
			"event": "chat",
			"payload": map[string]interface{}{
				"runId":      "run-2",
				"sessionKey": "session-2",
				"delta":      "done",
				"status":     "done",
			},
		}); err != nil {
			t.Fatalf("write chat event: %v", err)
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	adapter := newTestAdapter(t, wsURL, "token-2")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "session-2",
		Role:      "user",
		Text:      "hello again",
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-2",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 1 {
		data, _ := json.Marshal(replies)
		t.Fatalf("expected 1 reply, got %d %s", len(replies), string(data))
	}
	if replies[0].Text != "done" || !replies[0].IsFinal || !replies[0].IsEnd {
		t.Fatalf("unexpected reply: %+v", replies[0])
	}
}

func TestAdapterChatHandlesOpenClawFinalStateSchema(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]interface{}{
			"type":  "event",
			"event": "connect.challenge",
			"payload": map[string]interface{}{
				"nonce": "nonce-3",
			},
		}); err != nil {
			t.Fatalf("write challenge: %v", err)
		}

		var connectReq map[string]interface{}
		if err := conn.ReadJSON(&connectReq); err != nil {
			t.Fatalf("read connect request: %v", err)
		}
		if err := conn.WriteJSON(map[string]interface{}{
			"type":    "res",
			"id":      connectReq["id"],
			"ok":      true,
			"payload": map[string]interface{}{"status": "ok"},
		}); err != nil {
			t.Fatalf("write connect response: %v", err)
		}

		var subscribeReq map[string]interface{}
		if err := conn.ReadJSON(&subscribeReq); err != nil {
			t.Fatalf("read subscribe request: %v", err)
		}
		if subscribeReq["method"] != "sessions.messages.subscribe" {
			t.Fatalf("unexpected subscribe method: %#v", subscribeReq)
		}
		if err := conn.WriteJSON(map[string]interface{}{
			"type": "res",
			"id":   subscribeReq["id"],
			"ok":   true,
			"payload": map[string]interface{}{
				"subscribed": true,
				"key":        "session-3",
			},
		}); err != nil {
			t.Fatalf("write subscribe response: %v", err)
		}

		var chatReq map[string]interface{}
		if err := conn.ReadJSON(&chatReq); err != nil {
			t.Fatalf("read chat request: %v", err)
		}
		if err := conn.WriteJSON(map[string]interface{}{
			"type": "res",
			"id":   chatReq["id"],
			"ok":   true,
			"payload": map[string]interface{}{
				"runId":  "run-3",
				"status": "started",
			},
		}); err != nil {
			t.Fatalf("write chat response: %v", err)
		}

		if err := conn.WriteJSON(map[string]interface{}{
			"type":  "event",
			"event": "chat",
			"payload": map[string]interface{}{
				"runId":      "run-3",
				"sessionKey": "session-3",
				"seq":        1,
				"state":      "final",
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{
							"type": "text",
							"text": "OK",
						},
					},
				},
			},
		}); err != nil {
			t.Fatalf("write chat final event: %v", err)
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	adapter := newTestAdapter(t, wsURL, "token-3")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "session-3",
		Role:      "user",
		Text:      "please reply ok",
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-3",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 1 {
		data, _ := json.Marshal(replies)
		t.Fatalf("expected 1 reply, got %d %s", len(replies), string(data))
	}
	if replies[0].Text != "OK" || !replies[0].IsFinal || !replies[0].IsEnd || replies[0].FinishReason != "stop" {
		t.Fatalf("unexpected final reply: %+v", replies[0])
	}
}

func TestAdapterChatHandlesSessionMessageEvents(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]interface{}{
			"type":  "event",
			"event": "connect.challenge",
			"payload": map[string]interface{}{
				"nonce": "nonce-4",
			},
		}); err != nil {
			t.Fatalf("write challenge: %v", err)
		}

		var connectReq map[string]interface{}
		if err := conn.ReadJSON(&connectReq); err != nil {
			t.Fatalf("read connect request: %v", err)
		}
		if err := conn.WriteJSON(map[string]interface{}{
			"type":    "res",
			"id":      connectReq["id"],
			"ok":      true,
			"payload": map[string]interface{}{"status": "ok"},
		}); err != nil {
			t.Fatalf("write connect response: %v", err)
		}

		var subscribeReq map[string]interface{}
		if err := conn.ReadJSON(&subscribeReq); err != nil {
			t.Fatalf("read subscribe request: %v", err)
		}
		if err := conn.WriteJSON(map[string]interface{}{
			"type": "res",
			"id":   subscribeReq["id"],
			"ok":   true,
			"payload": map[string]interface{}{
				"subscribed": true,
				"key":        "session-4",
			},
		}); err != nil {
			t.Fatalf("write subscribe response: %v", err)
		}

		var chatReq map[string]interface{}
		if err := conn.ReadJSON(&chatReq); err != nil {
			t.Fatalf("read chat request: %v", err)
		}
		if err := conn.WriteJSON(map[string]interface{}{
			"type": "res",
			"id":   chatReq["id"],
			"ok":   true,
			"payload": map[string]interface{}{
				"runId":  "run-4",
				"status": "started",
			},
		}); err != nil {
			t.Fatalf("write chat response: %v", err)
		}

		if err := conn.WriteJSON(map[string]interface{}{
			"type":  "event",
			"event": "session.message",
			"payload": map[string]interface{}{
				"runId":      "run-4",
				"sessionKey": "session-4",
				"state":      "final",
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{
							"type": "text",
							"text": "OK",
						},
					},
				},
			},
		}); err != nil {
			t.Fatalf("write session.message event: %v", err)
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	adapter := newTestAdapter(t, wsURL, "token-4")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "session-4",
		Role:      "user",
		Text:      "please reply ok",
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-4",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 1 {
		data, _ := json.Marshal(replies)
		t.Fatalf("expected 1 reply, got %d %s", len(replies), string(data))
	}
	if replies[0].Text != "OK" || !replies[0].IsFinal || !replies[0].IsEnd || replies[0].FinishReason != "stop" {
		t.Fatalf("unexpected final reply: %+v", replies[0])
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
		Store: config.StoreConfig{
			StateFile: filepath.Join(tmpDir, "state.json"),
		},
		OpenClaw: config.OpenClawConfig{
			EnvFile:            envPath,
			GatewayWSURL:       "ws://127.0.0.1:18789",
			GatewayTokenEnvKey: "OPENCLAW_GATEWAY_TOKEN",
		},
	}

	adapter := NewAdapter(cfg, log.New(os.Stdout, "", 0), store.NewFileStore(cfg.Store.StateFile))
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
		Store: config.StoreConfig{
			StateFile: filepath.Join(tmpDir, "state.json"),
		},
		OpenClaw: config.OpenClawConfig{
			EnvFile:            envPath,
			GatewayWSURL:       wsURL,
			GatewayTokenEnvKey: "OPENCLAW_GATEWAY_TOKEN",
		},
	}
	return NewAdapter(cfg, log.New(os.Stdout, "", 0), store.NewFileStore(cfg.Store.StateFile))
}
