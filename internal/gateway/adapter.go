package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"openclaw/dameon/internal/config"
	"openclaw/dameon/internal/protocol"
)

const (
	defaultGatewayWSURL = "ws://127.0.0.1:18789"
	writeTimeout        = 5 * time.Second
	readTimeout         = 30 * time.Second
)

type Adapter struct {
	cfg    *config.Config
	logger *log.Logger
	dialer *websocket.Dialer
}

type gatewayChallenge struct {
	Type    string `json:"type"`
	Event   string `json:"event"`
	Payload struct {
		Nonce string `json:"nonce"`
		TS    int64  `json:"ts"`
	} `json:"payload"`
}

type gatewayRequest struct {
	Type   string      `json:"type"`
	ID     string      `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params,omitempty"`
}

type gatewayResponse struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *gatewayError   `json:"error,omitempty"`
}

type gatewayEvent struct {
	Type         string          `json:"type"`
	Event        string          `json:"event"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	Seq          *int64          `json:"seq,omitempty"`
	StateVersion *int64          `json:"stateVersion,omitempty"`
}

type gatewayError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func NewAdapter(cfg *config.Config, logger *log.Logger) *Adapter {
	return &Adapter{
		cfg:    cfg,
		logger: logger,
		dialer: &websocket.Dialer{HandshakeTimeout: 10 * time.Second},
	}
}

func (a *Adapter) Chat(ctx context.Context, payload protocol.ChatMessagePayload) ([]protocol.ChatReplyPayload, error) {
	conn, err := a.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	cloudMsgID := metadataString(payload.Metadata, "cloud_msg_id")
	if cloudMsgID == "" {
		cloudMsgID = fmt.Sprintf("chat-%d", time.Now().UnixMilli())
	}

	runID, err := a.sendChat(ctx, conn, payload, cloudMsgID)
	if err != nil {
		return nil, err
	}

	replies, err := a.collectChatReplies(ctx, conn, payload.SessionID, runID)
	if err != nil {
		return nil, err
	}
	if len(replies) == 0 {
		return []protocol.ChatReplyPayload{{
			Role:         "assistant",
			ChunkSeq:     1,
			IsFinal:      true,
			IsEnd:        true,
			FinishReason: "error",
			ErrorCode:    "CHAT_EXECUTION_FAILED",
			ErrorMessage: "gateway returned no chat events",
		}}, nil
	}
	return replies, nil
}

func (a *Adapter) connect(ctx context.Context) (*websocket.Conn, error) {
	wsURL := a.cfg.OpenClawConfig().GatewayWSURL
	if strings.TrimSpace(wsURL) == "" {
		wsURL = defaultGatewayWSURL
	}

	token, err := a.loadGatewayToken()
	if err != nil {
		return nil, err
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn, _, err := a.dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		return nil, fmt.Errorf("dial openclaw gateway: %w", err)
	}

	if err := a.completeHandshake(ctx, conn, token); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (a *Adapter) completeHandshake(ctx context.Context, conn *websocket.Conn, token string) error {
	challengeRaw, err := a.readJSON(ctx, conn)
	if err != nil {
		return fmt.Errorf("read connect.challenge: %w", err)
	}

	var challenge gatewayChallenge
	if err := json.Unmarshal(challengeRaw, &challenge); err != nil {
		return fmt.Errorf("parse connect.challenge: %w", err)
	}
	if challenge.Type != "event" || challenge.Event != "connect.challenge" {
		return fmt.Errorf("unexpected gateway handshake frame type=%s event=%s", challenge.Type, challenge.Event)
	}

	connectID := fmt.Sprintf("connect-%d", time.Now().UnixMilli())
	connectReq := gatewayRequest{
		Type:   "req",
		ID:     connectID,
		Method: "connect",
		Params: map[string]interface{}{
			"minProtocol": 3,
			"maxProtocol": 3,
			"client": map[string]interface{}{
				"id":          "cli",
				"displayName": "openclaw-daemon",
				"version":     a.cfg.DaemonVersion,
				"platform":    runtime.GOOS,
				"mode":        "cli",
			},
			"role":        "operator",
			"scopes":      []string{"operator.read", "operator.write"},
			"caps":        []string{},
			"commands":    []string{},
			"permissions": map[string]interface{}{},
			"auth": map[string]interface{}{
				"token": token,
			},
			"locale":    "zh-CN",
			"userAgent": fmt.Sprintf("openclaw-daemon/%s", a.cfg.DaemonVersion),
		},
	}
	if err := a.writeJSON(ctx, conn, connectReq); err != nil {
		return fmt.Errorf("send connect request: %w", err)
	}

	for {
		frameRaw, err := a.readJSON(ctx, conn)
		if err != nil {
			return fmt.Errorf("read connect response: %w", err)
		}

		var envelope struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(frameRaw, &envelope); err != nil {
			continue
		}
		if envelope.Type != "res" || envelope.ID != connectID {
			continue
		}

		var res gatewayResponse
		if err := json.Unmarshal(frameRaw, &res); err != nil {
			return fmt.Errorf("parse connect response: %w", err)
		}
		if !res.OK {
			return gatewayResponseError("connect rejected", res.Error)
		}
		return nil
	}
}

func (a *Adapter) sendChat(ctx context.Context, conn *websocket.Conn, payload protocol.ChatMessagePayload, cloudMsgID string) (string, error) {
	reqID := fmt.Sprintf("chat-%d", time.Now().UnixMilli())
	params := map[string]interface{}{
		"sessionKey":     payload.SessionID,
		"idempotencyKey": cloudMsgID,
		"text":           payload.Text,
	}
	if len(payload.Metadata) > 0 {
		params["metadata"] = payload.Metadata
	}

	if err := a.writeJSON(ctx, conn, gatewayRequest{
		Type:   "req",
		ID:     reqID,
		Method: "chat.send",
		Params: params,
	}); err != nil {
		return "", fmt.Errorf("send chat request: %w", err)
	}

	for {
		frameRaw, err := a.readJSON(ctx, conn)
		if err != nil {
			return "", fmt.Errorf("read chat.send response: %w", err)
		}

		var envelope struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(frameRaw, &envelope); err != nil {
			continue
		}
		if envelope.Type != "res" || envelope.ID != reqID {
			continue
		}

		var res gatewayResponse
		if err := json.Unmarshal(frameRaw, &res); err != nil {
			return "", fmt.Errorf("parse chat.send response: %w", err)
		}
		if !res.OK {
			return "", gatewayResponseError("chat.send rejected", res.Error)
		}

		var ack struct {
			RunID  string `json:"runId"`
			Status string `json:"status"`
		}
		if len(res.Payload) > 0 {
			_ = json.Unmarshal(res.Payload, &ack)
		}
		return ack.RunID, nil
	}
}

func (a *Adapter) collectChatReplies(ctx context.Context, conn *websocket.Conn, sessionID, runID string) ([]protocol.ChatReplyPayload, error) {
	replies := make([]protocol.ChatReplyPayload, 0, 8)
	chunkSeq := 0

	for {
		frameRaw, err := a.readJSON(ctx, conn)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return replies, fmt.Errorf("read chat event: %w", err)
		}

		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(frameRaw, &envelope); err != nil {
			continue
		}
		if envelope.Type != "event" {
			continue
		}

		var event gatewayEvent
		if err := json.Unmarshal(frameRaw, &event); err != nil {
			continue
		}
		if event.Event != "chat" {
			continue
		}

		var payloadMap map[string]interface{}
		if len(event.Payload) > 0 {
			if err := json.Unmarshal(event.Payload, &payloadMap); err != nil {
				continue
			}
		}
		if !matchesChatEvent(payloadMap, sessionID, runID) {
			continue
		}

		text := extractText(payloadMap)
		if text != "" {
			chunkSeq++
			replies = append(replies, protocol.ChatReplyPayload{
				Role:     "assistant",
				Text:     text,
				ChunkSeq: chunkSeq,
			})
		}

		if isTerminalChatEvent(payloadMap) {
			if len(replies) == 0 {
				chunkSeq++
				replies = append(replies, protocol.ChatReplyPayload{
					Role:     "assistant",
					Text:     "",
					ChunkSeq: chunkSeq,
				})
			}

			last := len(replies) - 1
			replies[last].IsFinal = true
			replies[last].IsEnd = true
			replies[last].FinishReason = finishReason(payloadMap)

			if code := firstString(payloadMap, "errorCode", "code"); code != "" && replies[last].FinishReason == "error" {
				replies[last].ErrorCode = code
			}
			if msg := firstString(payloadMap, "errorMessage", "message", "error"); msg != "" && replies[last].FinishReason == "error" {
				replies[last].ErrorMessage = msg
			}
			return replies, nil
		}
	}
}

func (a *Adapter) loadGatewayToken() (string, error) {
	openClawCfg := a.cfg.OpenClawConfig()
	envKey := openClawCfg.GatewayTokenEnvKey
	if envKey == "" {
		envKey = "OPENCLAW_GATEWAY_TOKEN"
	}

	if token := strings.TrimSpace(os.Getenv(envKey)); token != "" {
		return token, nil
	}

	envData, err := os.ReadFile(openClawCfg.EnvFile)
	if err != nil {
		return "", fmt.Errorf("load gateway token from %s: %w", openClawCfg.EnvFile, err)
	}

	for _, line := range strings.Split(string(envData), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != envKey {
			continue
		}
		return trimEnvValue(value), nil
	}
	return "", fmt.Errorf("gateway token %q is missing", envKey)
}

func (a *Adapter) writeJSON(ctx context.Context, conn *websocket.Conn, value interface{}) error {
	_ = ctx
	if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return conn.WriteJSON(value)
}

func (a *Adapter) readJSON(ctx context.Context, conn *websocket.Conn) ([]byte, error) {
	deadline := time.Now().Add(readTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return data, nil
}

func metadataString(metadata map[string]interface{}, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	return strings.TrimSpace(text)
}

func matchesChatEvent(payload map[string]interface{}, sessionID, runID string) bool {
	if len(payload) == 0 {
		return false
	}
	if runID != "" && anyStringEquals(payload, runID, "runId", "run_id") {
		return true
	}
	if sessionID != "" && anyStringEquals(payload, sessionID, "sessionKey", "sessionId", "session_id") {
		return true
	}
	return runID == "" && sessionID == ""
}

func anyStringEquals(values map[string]interface{}, target string, keys ...string) bool {
	for _, key := range keys {
		if text, ok := values[key].(string); ok && text == target {
			return true
		}
	}
	return false
}

func extractText(payload map[string]interface{}) string {
	for _, key := range []string{"text", "delta", "content", "message"} {
		if value := deepString(payload[key]); value != "" {
			return value
		}
	}
	if items, ok := payload["parts"].([]interface{}); ok {
		var builder strings.Builder
		for _, item := range items {
			if text := deepString(item); text != "" {
				builder.WriteString(text)
			}
		}
		return builder.String()
	}
	return ""
}

func deepString(value interface{}) string {
	switch v := value.(type) {
	case string:
		return v
	case map[string]interface{}:
		for _, key := range []string{"text", "delta", "content", "message", "value"} {
			if text := deepString(v[key]); text != "" {
				return text
			}
		}
	case []interface{}:
		var builder strings.Builder
		for _, item := range v {
			if text := deepString(item); text != "" {
				builder.WriteString(text)
			}
		}
		return builder.String()
	}
	return ""
}

func isTerminalChatEvent(payload map[string]interface{}) bool {
	status := strings.ToLower(firstString(payload, "status", "phase", "state"))
	switch status {
	case "done", "completed", "complete", "ok", "stopped", "aborted", "cancelled", "canceled", "error", "failed":
		return true
	}
	if value, ok := payload["isFinal"].(bool); ok && value {
		return true
	}
	if value, ok := payload["isEnd"].(bool); ok && value {
		return true
	}
	return false
}

func finishReason(payload map[string]interface{}) string {
	status := strings.ToLower(firstString(payload, "status", "phase", "state", "finishReason", "finish_reason"))
	switch status {
	case "error", "failed":
		return "error"
	case "aborted", "cancelled", "canceled", "stopped":
		return "cancelled"
	case "length":
		return "length"
	default:
		return "stop"
	}
}

func firstString(values map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if text, ok := values[key].(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func gatewayResponseError(prefix string, errPayload *gatewayError) error {
	if errPayload == nil {
		return errors.New(prefix)
	}
	if errPayload.Code != "" && errPayload.Message != "" {
		return fmt.Errorf("%s: %s (%s)", prefix, errPayload.Message, errPayload.Code)
	}
	if errPayload.Message != "" {
		return fmt.Errorf("%s: %s", prefix, errPayload.Message)
	}
	if errPayload.Code != "" {
		return fmt.Errorf("%s: %s", prefix, errPayload.Code)
	}
	return errors.New(prefix)
}

func trimEnvValue(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, `"'`)
	return value
}

func HTTPBaseURL(wsURL string) string {
	parsed, err := url.Parse(wsURL)
	if err != nil {
		return ""
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	}
	parsed.Path = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}
