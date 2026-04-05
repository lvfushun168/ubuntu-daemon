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

func TestAdapterChatHandlesAgentAssistantEvents(t *testing.T) {
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
				"nonce": "nonce-agent-1",
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
				"key":        "session-agent-1",
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
				"runId":  "run-agent-1",
				"status": "started",
			},
		}); err != nil {
			t.Fatalf("write chat response: %v", err)
		}

		events := []map[string]interface{}{
			{
				"type":  "event",
				"event": "agent",
				"payload": map[string]interface{}{
					"runId":      "run-agent-1",
					"sessionKey": "session-agent-1",
					"stream":     "assistant",
					"delta": map[string]interface{}{
						"text": "OK",
					},
				},
			},
			{
				"type":  "event",
				"event": "chat",
				"payload": map[string]interface{}{
					"runId":      "run-agent-1",
					"sessionKey": "session-agent-1",
					"state":      "final",
				},
			},
		}
		for _, event := range events {
			if err := conn.WriteJSON(event); err != nil {
				t.Fatalf("write event: %v", err)
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	adapter := newTestAdapter(t, wsURL, "token-agent-1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "session-agent-1",
		Role:      "user",
		Text:      "please reply ok",
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-agent-1",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 1 {
		data, _ := json.Marshal(replies)
		t.Fatalf("expected 1 reply, got %d %s", len(replies), string(data))
	}
	if replies[0].Text != "OK" || !replies[0].IsFinal || !replies[0].IsEnd {
		t.Fatalf("unexpected agent reply: %+v", replies[0])
	}
}

func TestAdapterChatMatchesPrefixedSessionKeyForAgentEvents(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]interface{}{
			"type":    "event",
			"event":   "connect.challenge",
			"payload": map[string]interface{}{"nonce": "nonce-agent-2"},
		}); err != nil {
			t.Fatalf("write challenge: %v", err)
		}

		var connectReq map[string]interface{}
		if err := conn.ReadJSON(&connectReq); err != nil {
			t.Fatalf("read connect request: %v", err)
		}
		if err := conn.WriteJSON(map[string]interface{}{
			"type": "res",
			"id":   connectReq["id"],
			"ok":   true,
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
				"runId": "run-agent-2",
			},
		}); err != nil {
			t.Fatalf("write chat response: %v", err)
		}

		events := []map[string]interface{}{
			{
				"type":  "event",
				"event": "agent",
				"payload": map[string]interface{}{
					"sessionKey": "agent:main:session-agent-2",
					"stream":     "assistant",
					"delta":      "你好",
				},
			},
			{
				"type":  "event",
				"event": "chat",
				"payload": map[string]interface{}{
					"runId":      "run-agent-2",
					"sessionKey": "agent:main:session-agent-2",
					"state":      "final",
				},
			},
		}
		for _, event := range events {
			if err := conn.WriteJSON(event); err != nil {
				t.Fatalf("write event: %v", err)
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	adapter := newTestAdapter(t, wsURL, "token-agent-2")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "session-agent-2",
		Role:      "user",
		Text:      "say hi",
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-agent-2",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 1 || replies[0].Text != "你好" || !replies[0].IsFinal || !replies[0].IsEnd {
		t.Fatalf("unexpected replies: %+v", replies)
	}
}

func TestAdapterChatUsesFinalSnapshotForCumulativeSessionMessages(t *testing.T) {
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
				"nonce": "nonce-5",
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
				"key":        "session-5",
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
				"runId":  "run-5",
				"status": "started",
			},
		}); err != nil {
			t.Fatalf("write chat response: %v", err)
		}

		events := []map[string]interface{}{
			{
				"type":  "event",
				"event": "session.message",
				"payload": map[string]interface{}{
					"runId":      "run-5",
					"sessionKey": "session-5",
					"state":      "streaming",
					"message": map[string]interface{}{
						"role": "assistant",
						"content": []map[string]interface{}{
							{
								"type": "text",
								"text": "我是",
							},
						},
					},
				},
			},
			{
				"type":  "event",
				"event": "session.message",
				"payload": map[string]interface{}{
					"runId":      "run-5",
					"sessionKey": "session-5",
					"state":      "final",
					"message": map[string]interface{}{
						"role": "assistant",
						"content": []map[string]interface{}{
							{
								"type": "text",
								"text": "我是你的 AI 助手",
							},
						},
					},
				},
			},
		}
		for _, event := range events {
			if err := conn.WriteJSON(event); err != nil {
				t.Fatalf("write session.message event: %v", err)
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	adapter := newTestAdapter(t, wsURL, "token-5")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "session-5",
		Role:      "user",
		Text:      "who are you",
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-5",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 1 {
		data, _ := json.Marshal(replies)
		t.Fatalf("expected 1 reply, got %d %s", len(replies), string(data))
	}
	if replies[0].Text != "我是你的 AI 助手" || !replies[0].IsFinal || !replies[0].IsEnd {
		t.Fatalf("unexpected final snapshot reply: %+v", replies[0])
	}
}

func TestAdapterChatPreservesCodeBlocksInStructuredContent(t *testing.T) {
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
				"nonce": "nonce-6",
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
				"key":        "session-6",
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
				"runId":  "run-6",
				"status": "started",
			},
		}); err != nil {
			t.Fatalf("write chat response: %v", err)
		}

		if err := conn.WriteJSON(map[string]interface{}{
			"type":  "event",
			"event": "session.message",
			"payload": map[string]interface{}{
				"runId":      "run-6",
				"sessionKey": "session-6",
				"state":      "final",
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{
							"type": "text",
							"text": "请执行下面命令：",
						},
						{
							"type":     "code",
							"language": "bash",
							"code":     "openclaw gateway restart",
						},
						{
							"type": "text",
							"text": "执行后检查日志。",
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
	adapter := newTestAdapter(t, wsURL, "token-6")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "session-6",
		Role:      "user",
		Text:      "how to restart gateway",
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-6",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 1 {
		data, _ := json.Marshal(replies)
		t.Fatalf("expected 1 reply, got %d %s", len(replies), string(data))
	}
	expected := "请执行下面命令：\n\n```bash\nopenclaw gateway restart\n```\n\n执行后检查日志。"
	if replies[0].Text != expected {
		t.Fatalf("unexpected rich text reply: %#v", replies[0].Text)
	}
	if !replies[0].IsFinal || !replies[0].IsEnd || replies[0].FinishReason != "stop" {
		t.Fatalf("unexpected final reply flags: %+v", replies[0])
	}
}

func TestAdapterChatUsesFinalSnapshotWhenStructuredContentRewritesEarlierChunks(t *testing.T) {
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
				"nonce": "nonce-7",
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
				"key":        "session-7",
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
				"runId":  "run-7",
				"status": "started",
			},
		}); err != nil {
			t.Fatalf("write chat response: %v", err)
		}

		events := []map[string]interface{}{
			{
				"type":  "event",
				"event": "session.message",
				"payload": map[string]interface{}{
					"runId":      "run-7",
					"sessionKey": "session-7",
					"state":      "streaming",
					"message": map[string]interface{}{
						"role": "assistant",
						"content": []map[string]interface{}{
							{
								"type": "text",
								"text": "1. `web_search` 失败了",
							},
						},
					},
				},
			},
			{
				"type":  "event",
				"event": "session.message",
				"payload": map[string]interface{}{
					"runId":      "run-7",
					"sessionKey": "session-7",
					"state":      "final",
					"message": map[string]interface{}{
						"role": "assistant",
						"content": []map[string]interface{}{
							{
								"type": "text",
								"text": "1. `web_search` 失败了（fetch failed）\n\n**你能做的：**",
							},
							{
								"type":     "code",
								"language": "bash",
								"code":     "curl -o cat1.jpg \"https://images.unsplash.com/photo-1514888286974-6c03e2ca1dba?w=800\"",
							},
						},
					},
				},
			},
		}
		for _, event := range events {
			if err := conn.WriteJSON(event); err != nil {
				t.Fatalf("write session.message event: %v", err)
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	adapter := newTestAdapter(t, wsURL, "token-7")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "session-7",
		Role:      "user",
		Text:      "show me download command",
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-7",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 1 {
		data, _ := json.Marshal(replies)
		t.Fatalf("expected collapsed final reply, got %d %s", len(replies), string(data))
	}
	expected := "1. `web_search` 失败了（fetch failed）\n\n**你能做的：**\n\n```bash\ncurl -o cat1.jpg \"https://images.unsplash.com/photo-1514888286974-6c03e2ca1dba?w=800\"\n```"
	if replies[0].Text != expected {
		t.Fatalf("unexpected rewritten final reply: %#v", replies[0].Text)
	}
	if !replies[0].IsFinal || !replies[0].IsEnd || replies[0].ChunkSeq != 1 {
		t.Fatalf("unexpected final flags: %+v", replies[0])
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

func TestAdapterChatRecoversAssistantReplyFromTranscriptOnTerminalOnlyEvent(t *testing.T) {
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
				"nonce": "nonce-transcript",
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
			"type": "hello-ok",
			"auth": map[string]interface{}{
				"deviceToken": "device-token-transcript",
			},
		}); err != nil {
			t.Fatalf("write hello-ok: %v", err)
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
				"key":        "cloud-session-1",
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
				"runId":  "run-transcript",
				"status": "started",
			},
		}); err != nil {
			t.Fatalf("write chat response: %v", err)
		}

		if err := conn.WriteJSON(map[string]interface{}{
			"type":  "event",
			"event": "chat",
			"payload": map[string]interface{}{
				"runId":      "run-transcript",
				"sessionKey": "agent:main:cloud-session-1",
				"sessionId":  "openclaw-session-1",
				"message": map[string]interface{}{
					"role":      "user",
					"timestamp": float64(1000),
				},
				"state": "final",
			},
		}); err != nil {
			t.Fatalf("write terminal event: %v", err)
		}
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")
	if err := os.WriteFile(envPath, []byte("OPENCLAW_GATEWAY_TOKEN='token-transcript'\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	transcriptDir := filepath.Join(tmpDir, "agents", "main", "sessions")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	transcript := strings.Join([]string{
		`{"type":"message","message":{"role":"user","timestamp":1000,"content":[{"type":"text","text":"请只回复OK"}]}}`,
		`{"type":"message","message":{"role":"assistant","timestamp":1200,"content":[{"type":"thinking","thinking":"hidden"},{"type":"text","text":"OK"}]}}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(transcriptDir, "openclaw-session-1.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	cfg := &config.Config{
		DeviceID:      "device-1",
		DaemonVersion: "0.1.0",
		Store: config.StoreConfig{
			StateFile: filepath.Join(tmpDir, "state.json"),
		},
		OpenClaw: config.OpenClawConfig{
			WorkDir:            tmpDir,
			EnvFile:            envPath,
			GatewayWSURL:       "ws" + strings.TrimPrefix(server.URL, "http"),
			GatewayTokenEnvKey: "OPENCLAW_GATEWAY_TOKEN",
		},
	}
	adapter := NewAdapter(cfg, log.New(os.Stdout, "", 0), store.NewFileStore(cfg.Store.StateFile))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "cloud-session-1",
		Role:      "user",
		Text:      "请只回复OK",
		Stream:    true,
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-transcript",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 1 {
		data, _ := json.Marshal(replies)
		t.Fatalf("expected 1 reply, got %d %s", len(replies), string(data))
	}
	if replies[0].Text != "OK" {
		t.Fatalf("unexpected transcript reply: %+v", replies[0])
	}
	if !replies[0].IsFinal || !replies[0].IsEnd || replies[0].FinishReason != "stop" {
		t.Fatalf("unexpected final flags: %+v", replies[0])
	}
}

func TestAdapterChatWaitsForTranscriptAssistantNewerThanCurrentUserMessage(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]interface{}{
			"type":    "event",
			"event":   "connect.challenge",
			"payload": map[string]interface{}{"nonce": "nonce-transcript-delay"},
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
			"type": "hello-ok",
			"auth": map[string]interface{}{"deviceToken": "device-token-delay"},
		}); err != nil {
			t.Fatalf("write hello-ok: %v", err)
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
				"key":        "cloud-session-delay",
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
				"runId":  "run-delay",
				"status": "started",
			},
		}); err != nil {
			t.Fatalf("write chat response: %v", err)
		}

		if err := conn.WriteJSON(map[string]interface{}{
			"type":  "event",
			"event": "chat",
			"payload": map[string]interface{}{
				"runId":      "run-delay",
				"sessionKey": "agent:main:cloud-session-delay",
				"sessionId":  "openclaw-session-delay",
				"message": map[string]interface{}{
					"role":      "user",
					"timestamp": float64(2000),
				},
				"state": "final",
			},
		}); err != nil {
			t.Fatalf("write terminal event: %v", err)
		}
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")
	if err := os.WriteFile(envPath, []byte("OPENCLAW_GATEWAY_TOKEN='token-transcript-delay'\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	transcriptDir := filepath.Join(tmpDir, "agents", "main", "sessions")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	transcriptPath := filepath.Join(transcriptDir, "openclaw-session-delay.jsonl")
	initialTranscript := strings.Join([]string{
		`{"type":"message","message":{"role":"user","timestamp":1000,"content":[{"type":"text","text":"上一轮"}]}}`,
		`{"type":"message","message":{"role":"assistant","timestamp":1500,"content":[{"type":"text","text":"旧回复"}]}}`,
		`{"type":"message","message":{"role":"user","timestamp":2000,"content":[{"type":"text","text":"当前轮"}]}}`,
	}, "\n")
	if err := os.WriteFile(transcriptPath, []byte(initialTranscript), 0o600); err != nil {
		t.Fatalf("write initial transcript: %v", err)
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		updatedTranscript := initialTranscript + "\n" +
			`{"type":"message","message":{"role":"assistant","timestamp":2300,"content":[{"type":"text","text":"新回复"}]}}`
		_ = os.WriteFile(transcriptPath, []byte(updatedTranscript), 0o600)
	}()

	cfg := &config.Config{
		DeviceID:      "device-1",
		DaemonVersion: "0.1.0",
		Store: config.StoreConfig{
			StateFile: filepath.Join(tmpDir, "state.json"),
		},
		OpenClaw: config.OpenClawConfig{
			WorkDir:            tmpDir,
			EnvFile:            envPath,
			GatewayWSURL:       "ws" + strings.TrimPrefix(server.URL, "http"),
			GatewayTokenEnvKey: "OPENCLAW_GATEWAY_TOKEN",
		},
	}
	adapter := NewAdapter(cfg, log.New(os.Stdout, "", 0), store.NewFileStore(cfg.Store.StateFile))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "cloud-session-delay",
		Role:      "user",
		Text:      "当前轮",
		Stream:    true,
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-transcript-delay",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 1 {
		data, _ := json.Marshal(replies)
		t.Fatalf("expected 1 reply, got %d %s", len(replies), string(data))
	}
	if replies[0].Text != "新回复" {
		t.Fatalf("unexpected delayed transcript reply: %+v", replies[0])
	}
}

func TestAdapterChatIgnoresAgentToolCompletionBeforeFinalAssistantReply(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]interface{}{
			"type":    "event",
			"event":   "connect.challenge",
			"payload": map[string]interface{}{"nonce": "nonce-agent-tool"},
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
				"runId":  "run-agent-tool",
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
					"runId":      "run-agent-tool",
					"sessionKey": "agent:main:cloud-session-agent-tool",
					"stream":     "assistant",
					"delta":      "好的，我来创作一幅正在喝茶的猫。",
				},
			},
			{
				"type":  "event",
				"event": "agent",
				"payload": map[string]interface{}{
					"runId":      "run-agent-tool",
					"sessionKey": "agent:main:cloud-session-agent-tool",
					"stream":     "tool",
					"status":     "completed",
					"toolName":   "image_generate",
					"mediaUrl":   "https://example.com/cat-drinking-tea.png",
				},
			},
			{
				"type":  "event",
				"event": "session.message",
				"payload": map[string]interface{}{
					"runId":      "run-agent-tool",
					"sessionKey": "agent:main:cloud-session-agent-tool",
					"state":      "final",
					"message": map[string]interface{}{
						"role": "assistant",
						"content": []map[string]interface{}{
							{
								"type": "text",
								"text": "完成啦，这是正在喝茶的猫。",
							},
						},
					},
				},
			},
		}
		for _, event := range events {
			if err := conn.WriteJSON(event); err != nil {
				t.Fatalf("write event: %v", err)
			}
		}
	}))
	defer server.Close()

	adapter := newTestAdapter(t, "ws"+strings.TrimPrefix(server.URL, "http"), "token-agent-tool")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "cloud-session-agent-tool",
		Role:      "user",
		Text:      "请创作一幅正在喝茶的猫",
		Stream:    true,
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-agent-tool",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 2 {
		data, _ := json.Marshal(replies)
		t.Fatalf("expected streamed reply plus final reply, got %d %s", len(replies), string(data))
	}
	if replies[0].Text != "好的，我来创作一幅正在喝茶的猫。" || replies[0].IsFinal || replies[0].IsEnd {
		t.Fatalf("unexpected first reply: %+v", replies[0])
	}
	if replies[1].Text != "完成啦，这是正在喝茶的猫。" {
		t.Fatalf("unexpected final reply: %+v", replies[1])
	}
	if len(replies[1].Attachments) != 1 || replies[1].Attachments[0].PreviewURL != "https://example.com/cat-drinking-tea.png" {
		t.Fatalf("expected attachment from tool event, got %+v", replies[1].Attachments)
	}
	if !replies[1].IsFinal || !replies[1].IsEnd || replies[1].FinishReason != "stop" {
		t.Fatalf("unexpected final flags: %+v", replies[1])
	}
}

func TestAdapterChatReturnsExplicitErrorInsteadOfEmptySuccessReply(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]interface{}{
			"type":    "event",
			"event":   "connect.challenge",
			"payload": map[string]interface{}{"nonce": "nonce-empty-final"},
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
				"runId":  "run-empty-final",
				"status": "started",
			},
		}); err != nil {
			t.Fatalf("write chat response: %v", err)
		}

		if err := conn.WriteJSON(map[string]interface{}{
			"type":  "event",
			"event": "chat",
			"payload": map[string]interface{}{
				"runId":      "run-empty-final",
				"sessionKey": "agent:main:cloud-session-empty-final",
				"state":      "final",
			},
		}); err != nil {
			t.Fatalf("write terminal event: %v", err)
		}
	}))
	defer server.Close()

	adapter := newTestAdapter(t, "ws"+strings.TrimPrefix(server.URL, "http"), "token-empty-final")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "cloud-session-empty-final",
		Role:      "user",
		Text:      "hello",
		Stream:    true,
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-empty-final",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 1 {
		data, _ := json.Marshal(replies)
		t.Fatalf("expected 1 reply, got %d %s", len(replies), string(data))
	}
	if replies[0].Text != "" {
		t.Fatalf("expected empty text when reply is missing, got %+v", replies[0])
	}
	if replies[0].FinishReason != "error" || replies[0].ErrorCode != "ASSISTANT_REPLY_MISSING" {
		t.Fatalf("expected explicit missing-reply error, got %+v", replies[0])
	}
}

func TestAdapterChatWaitsForFollowUpAssistantAfterToolUseTranscript(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]interface{}{
			"type":    "event",
			"event":   "connect.challenge",
			"payload": map[string]interface{}{"nonce": "nonce-follow-up"},
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
				"key":        "cloud-session-follow-up",
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
				"runId":  "run-follow-up",
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
					"runId":      "run-follow-up",
					"sessionKey": "agent:main:cloud-session-follow-up",
					"stream":     "assistant",
					"delta":      "好的，我来创作一幅正在喝茶的猫。",
				},
			},
			{
				"type":  "event",
				"event": "chat",
				"payload": map[string]interface{}{
					"runId":      "run-follow-up",
					"sessionKey": "agent:main:cloud-session-follow-up",
					"sessionId":  "openclaw-session-follow-up",
					"message": map[string]interface{}{
						"role":      "user",
						"timestamp": float64(2000),
					},
					"state": "final",
				},
			},
		}
		for _, event := range events {
			if err := conn.WriteJSON(event); err != nil {
				t.Fatalf("write event: %v", err)
			}
		}
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")
	if err := os.WriteFile(envPath, []byte("OPENCLAW_GATEWAY_TOKEN='token-follow-up'\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	transcriptDir := filepath.Join(tmpDir, "agents", "main", "sessions")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	transcriptPath := filepath.Join(transcriptDir, "openclaw-session-follow-up.jsonl")
	initialTranscript := strings.Join([]string{
		`{"type":"message","message":{"role":"user","timestamp":2000,"content":[{"type":"text","text":"请创作一幅正在喝茶的猫"}]}}`,
		`{"type":"message","message":{"role":"assistant","timestamp":2100,"content":[{"type":"text","text":"好的，我来创作一幅正在喝茶的猫。"},{"type":"toolCall","name":"image_generate"}],"stopReason":"toolUse"}}`,
	}, "\n")
	if err := os.WriteFile(transcriptPath, []byte(initialTranscript), 0o600); err != nil {
		t.Fatalf("write initial transcript: %v", err)
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		updatedTranscript := initialTranscript + "\n" +
			`{"type":"message","message":{"role":"assistant","timestamp":2300,"content":[{"type":"text","text":"完成啦！这是正在喝茶的猫。\n\n![正在喝茶的猫](https://example.com/cat-drinking-tea.jpg)"}],"stopReason":"stop"}}`
		_ = os.WriteFile(transcriptPath, []byte(updatedTranscript), 0o600)
	}()

	cfg := &config.Config{
		DeviceID:      "device-1",
		DaemonVersion: "0.1.0",
		Store: config.StoreConfig{
			StateFile: filepath.Join(tmpDir, "state.json"),
		},
		OpenClaw: config.OpenClawConfig{
			WorkDir:                tmpDir,
			EnvFile:                envPath,
			GatewayWSURL:           "ws" + strings.TrimPrefix(server.URL, "http"),
			GatewayTokenEnvKey:     "OPENCLAW_GATEWAY_TOKEN",
			ReplyResolveTimeoutSec: 3,
		},
	}
	adapter := NewAdapter(cfg, log.New(os.Stdout, "", 0), store.NewFileStore(cfg.Store.StateFile))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "cloud-session-follow-up",
		Role:      "user",
		Text:      "请创作一幅正在喝茶的猫",
		Stream:    true,
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-follow-up",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 2 {
		data, _ := json.Marshal(replies)
		t.Fatalf("expected 2 replies, got %d %s", len(replies), string(data))
	}
	if replies[0].Text != "好的，我来创作一幅正在喝茶的猫。" {
		t.Fatalf("unexpected first reply: %+v", replies[0])
	}
	if !strings.Contains(replies[1].Text, "完成啦！这是正在喝茶的猫。") {
		t.Fatalf("expected follow-up assistant delta, got %+v", replies[1])
	}
	if len(replies[1].Attachments) != 1 || replies[1].Attachments[0].PreviewURL != "https://example.com/cat-drinking-tea.jpg" {
		t.Fatalf("expected transcript image attachment, got %+v", replies[1].Attachments)
	}
	if !replies[1].IsFinal || !replies[1].IsEnd || replies[1].FinishReason != "stop" {
		t.Fatalf("unexpected final flags: %+v", replies[1])
	}
}

func TestAdapterChatIgnoresStaleSnapshotWithoutRunIdAndFallsBackToTranscript(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]interface{}{
			"type":    "event",
			"event":   "connect.challenge",
			"payload": map[string]interface{}{"nonce": "nonce-stale-snapshot"},
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
				"runId":  "run-stale-snapshot",
				"status": "started",
			},
		}); err != nil {
			t.Fatalf("write chat response: %v", err)
		}

		events := []map[string]interface{}{
			{
				"type":  "event",
				"event": "session.message",
				"payload": map[string]interface{}{
					"sessionKey": "agent:main:cloud-session-stale",
					"sessionId":  "openclaw-session-stale",
					"state":      "final",
					"message": map[string]interface{}{
						"role":      "assistant",
						"timestamp": float64(1500),
						"content": []map[string]interface{}{
							{
								"type": "text",
								"text": "旧回复",
							},
						},
					},
				},
			},
			{
				"type":  "event",
				"event": "chat",
				"payload": map[string]interface{}{
					"runId":      "run-stale-snapshot",
					"sessionKey": "agent:main:cloud-session-stale",
					"sessionId":  "openclaw-session-stale",
					"message": map[string]interface{}{
						"role":      "user",
						"timestamp": float64(2000),
					},
					"state": "final",
				},
			},
		}
		for _, event := range events {
			if err := conn.WriteJSON(event); err != nil {
				t.Fatalf("write event: %v", err)
			}
		}
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")
	if err := os.WriteFile(envPath, []byte("OPENCLAW_GATEWAY_TOKEN='token-stale-snapshot'\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	transcriptDir := filepath.Join(tmpDir, "agents", "main", "sessions")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	transcriptPath := filepath.Join(transcriptDir, "openclaw-session-stale.jsonl")
	initialTranscript := strings.Join([]string{
		`{"type":"message","message":{"role":"user","timestamp":1000,"content":[{"type":"text","text":"上一轮"}]}}`,
		`{"type":"message","message":{"role":"assistant","timestamp":1500,"content":[{"type":"text","text":"旧回复"}]}}`,
		`{"type":"message","message":{"role":"user","timestamp":2000,"content":[{"type":"text","text":"当前轮"}]}}`,
	}, "\n")
	if err := os.WriteFile(transcriptPath, []byte(initialTranscript), 0o600); err != nil {
		t.Fatalf("write initial transcript: %v", err)
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		updatedTranscript := initialTranscript + "\n" +
			`{"type":"message","message":{"role":"assistant","timestamp":2400,"content":[{"type":"text","text":"当前轮新回复"}]}}`
		_ = os.WriteFile(transcriptPath, []byte(updatedTranscript), 0o600)
	}()

	cfg := &config.Config{
		DeviceID:      "device-1",
		DaemonVersion: "0.1.0",
		Store: config.StoreConfig{
			StateFile: filepath.Join(tmpDir, "state.json"),
		},
		OpenClaw: config.OpenClawConfig{
			WorkDir:            tmpDir,
			EnvFile:            envPath,
			GatewayWSURL:       "ws" + strings.TrimPrefix(server.URL, "http"),
			GatewayTokenEnvKey: "OPENCLAW_GATEWAY_TOKEN",
		},
	}
	adapter := NewAdapter(cfg, log.New(os.Stdout, "", 0), store.NewFileStore(cfg.Store.StateFile))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	replies, err := adapter.Chat(ctx, protocol.ChatMessagePayload{
		SessionID: "cloud-session-stale",
		Role:      "user",
		Text:      "当前轮",
		Stream:    true,
		Metadata: map[string]interface{}{
			"cloud_msg_id": "msg-stale-snapshot",
		},
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(replies) != 1 {
		data, _ := json.Marshal(replies)
		t.Fatalf("expected 1 reply, got %d %s", len(replies), string(data))
	}
	if replies[0].Text != "当前轮新回复" {
		t.Fatalf("unexpected transcript-backed reply: %+v", replies[0])
	}
	if replies[0].RequestMsgID != "msg-stale-snapshot" || replies[0].RunID != "run-stale-snapshot" {
		t.Fatalf("missing correlation fields: %+v", replies[0])
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
