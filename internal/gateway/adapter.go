package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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
	"openclaw/dameon/internal/store"
)

const (
	defaultGatewayWSURL = "ws://127.0.0.1:18789"
	writeTimeout        = 5 * time.Second
	readTimeout         = 90 * time.Second
)

type Adapter struct {
	cfg        *config.Config
	logger     *log.Logger
	dialer     *websocket.Dialer
	stateStore *store.FileStore
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

type helloOKFrame struct {
	Type string `json:"type"`
	Auth *struct {
		DeviceToken string   `json:"deviceToken"`
		Role        string   `json:"role"`
		Scopes      []string `json:"scopes"`
		IssuedAtMs  int64    `json:"issuedAtMs"`
	} `json:"auth,omitempty"`
}

type connectAuth struct {
	headerToken    string
	connectToken   string
	connectAuthKey string
	signatureToken string
}

func NewAdapter(cfg *config.Config, logger *log.Logger, stateStore *store.FileStore) *Adapter {
	return &Adapter{
		cfg:        cfg,
		logger:     logger,
		dialer:     &websocket.Dialer{HandshakeTimeout: 10 * time.Second},
		stateStore: stateStore,
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

	if err := a.subscribeSessionMessages(ctx, conn, payload.SessionID); err != nil {
		return nil, err
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

	authState, err := a.resolveConnectAuth()
	if err != nil {
		return nil, err
	}
	identity, err := a.loadOrCreateGatewayIdentity()
	if err != nil {
		return nil, err
	}

	header := http.Header{}
	if authState.headerToken != "" {
		header.Set("Authorization", "Bearer "+authState.headerToken)
	}
	conn, _, err := a.dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		return nil, fmt.Errorf("dial openclaw gateway: %w", err)
	}

	if err := a.completeHandshake(ctx, conn, authState, identity); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (a *Adapter) subscribeSessionMessages(ctx context.Context, conn *websocket.Conn, sessionID string) error {
	reqID := fmt.Sprintf("session-sub-%d", time.Now().UnixMilli())
	if err := a.writeJSON(ctx, conn, gatewayRequest{
		Type:   "req",
		ID:     reqID,
		Method: "sessions.messages.subscribe",
		Params: map[string]interface{}{
			"key": sessionID,
		},
	}); err != nil {
		return fmt.Errorf("send sessions.messages.subscribe request: %w", err)
	}

	for {
		frameRaw, err := a.readJSON(ctx, conn)
		if err != nil {
			return fmt.Errorf("read sessions.messages.subscribe response: %w", err)
		}

		var envelope struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(frameRaw, &envelope); err != nil {
			continue
		}
		if a.handleControlFrame(frameRaw) {
			continue
		}
		if envelope.Type != "res" || envelope.ID != reqID {
			continue
		}

		var res gatewayResponse
		if err := json.Unmarshal(frameRaw, &res); err != nil {
			return fmt.Errorf("parse sessions.messages.subscribe response: %w", err)
		}
		if !res.OK {
			return gatewayResponseError("sessions.messages.subscribe rejected", res.Error)
		}
		return nil
	}
}

func (a *Adapter) completeHandshake(ctx context.Context, conn *websocket.Conn, authState connectAuth, identity *store.DeviceIdentity) error {
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

	signedAtMs := time.Now().UnixMilli()
	devicePayload := buildDeviceAuthPayloadV3(deviceAuthPayloadParams{
		deviceID:     identity.DeviceID,
		clientID:     "cli",
		clientMode:   "cli",
		role:         "operator",
		scopes:       []string{"operator.read", "operator.write"},
		signedAtMs:   signedAtMs,
		token:        authState.signatureToken,
		nonce:        challenge.Payload.Nonce,
		platform:     runtime.GOOS,
		deviceFamily: "headless",
	})
	signature, err := signDevicePayload(identity.PrivateKeyPEM, devicePayload)
	if err != nil {
		return fmt.Errorf("sign device payload: %w", err)
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
				"id":           "cli",
				"displayName":  "openclaw-daemon",
				"version":      a.cfg.DaemonVersion,
				"platform":     runtime.GOOS,
				"deviceFamily": "headless",
				"mode":         "cli",
			},
			"role":        "operator",
			"scopes":      []string{"operator.read", "operator.write"},
			"caps":        []string{},
			"commands":    []string{},
			"permissions": map[string]interface{}{},
			"auth": map[string]interface{}{
				authState.connectAuthKey: authState.connectToken,
			},
			"device": map[string]interface{}{
				"id":        identity.DeviceID,
				"publicKey": publicKeyRawBase64URLFromPEM(identity.PublicKeyPEM),
				"signature": signature,
				"signedAt":  signedAtMs,
				"nonce":     challenge.Payload.Nonce,
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

func (a *Adapter) handleControlFrame(frameRaw []byte) bool {
	var envelope struct {
		Type  string `json:"type"`
		Event string `json:"event"`
	}
	if err := json.Unmarshal(frameRaw, &envelope); err != nil {
		return false
	}

	switch envelope.Type {
	case "hello-ok":
		var hello helloOKFrame
		if err := json.Unmarshal(frameRaw, &hello); err != nil {
			a.logger.Printf("parse hello-ok failed: %v", err)
			return true
		}
		a.persistHelloOK(hello)
		return true
	case "event":
		if envelope.Event == "chat" || envelope.Event == "session.message" {
			return false
		}
		a.logger.Printf("ignoring control event after connect type=%s event=%s", envelope.Type, envelope.Event)
		return true
	default:
		return false
	}
}

func (a *Adapter) persistHelloOK(hello helloOKFrame) {
	if hello.Auth != nil && strings.TrimSpace(hello.Auth.DeviceToken) != "" {
		if err := a.persistGatewayDeviceToken(strings.TrimSpace(hello.Auth.DeviceToken)); err != nil {
			a.logger.Printf("persist gateway device token failed: %v", err)
		}
	}
}

func (a *Adapter) sendChat(ctx context.Context, conn *websocket.Conn, payload protocol.ChatMessagePayload, cloudMsgID string) (string, error) {
	reqID := fmt.Sprintf("chat-%d", time.Now().UnixMilli())
	params := map[string]interface{}{
		"sessionKey":     payload.SessionID,
		"idempotencyKey": cloudMsgID,
		"message":        payload.Text,
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
		if a.handleControlFrame(frameRaw) {
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
		if a.handleControlFrame(frameRaw) {
			continue
		}
		if envelope.Type != "event" {
			continue
		}

		var event gatewayEvent
		if err := json.Unmarshal(frameRaw, &event); err != nil {
			continue
		}
		if event.Event != "chat" && event.Event != "session.message" {
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

func (a *Adapter) resolveConnectAuth() (connectAuth, error) {
	deviceToken := strings.TrimSpace(a.loadStoredGatewayDeviceToken())
	if deviceToken != "" {
		return connectAuth{
			headerToken:    deviceToken,
			connectToken:   deviceToken,
			connectAuthKey: "deviceToken",
			signatureToken: deviceToken,
		}, nil
	}
	token, err := a.loadGatewayToken()
	if err != nil {
		return connectAuth{}, err
	}
	return connectAuth{
		headerToken:    token,
		connectToken:   token,
		connectAuthKey: "token",
		signatureToken: token,
	}, nil
}

func (a *Adapter) loadStoredGatewayDeviceToken() string {
	if a.stateStore == nil {
		return ""
	}
	state, err := a.stateStore.Load()
	if err != nil {
		a.logger.Printf("load state for gateway device token failed: %v", err)
		return ""
	}
	return state.GatewayDeviceToken
}

func (a *Adapter) persistGatewayDeviceToken(deviceToken string) error {
	if a.stateStore == nil || strings.TrimSpace(deviceToken) == "" {
		return nil
	}
	state, err := a.stateStore.Load()
	if err != nil {
		return err
	}
	state.GatewayDeviceToken = strings.TrimSpace(deviceToken)
	return a.stateStore.Save(state)
}

func (a *Adapter) loadOrCreateGatewayIdentity() (*store.DeviceIdentity, error) {
	if a.stateStore == nil {
		return createDeviceIdentity()
	}
	state, err := a.stateStore.Load()
	if err != nil {
		return nil, err
	}
	if state.GatewayDeviceIdentity != nil &&
		strings.TrimSpace(state.GatewayDeviceIdentity.DeviceID) != "" &&
		strings.TrimSpace(state.GatewayDeviceIdentity.PublicKeyPEM) != "" &&
		strings.TrimSpace(state.GatewayDeviceIdentity.PrivateKeyPEM) != "" {
		return state.GatewayDeviceIdentity, nil
	}
	identity, err := createDeviceIdentity()
	if err != nil {
		return nil, err
	}
	state.GatewayDeviceIdentity = identity
	if err := a.stateStore.Save(state); err != nil {
		return nil, err
	}
	return identity, nil
}

type deviceAuthPayloadParams struct {
	deviceID     string
	clientID     string
	clientMode   string
	role         string
	scopes       []string
	signedAtMs   int64
	token        string
	nonce        string
	platform     string
	deviceFamily string
}

func buildDeviceAuthPayloadV3(params deviceAuthPayloadParams) string {
	return strings.Join([]string{
		"v3",
		params.deviceID,
		params.clientID,
		params.clientMode,
		params.role,
		strings.Join(params.scopes, ","),
		fmt.Sprintf("%d", params.signedAtMs),
		params.token,
		params.nonce,
		normalizeDeviceMetadataForAuth(params.platform),
		normalizeDeviceMetadataForAuth(params.deviceFamily),
	}, "|")
}

func normalizeDeviceMetadataForAuth(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func createDeviceIdentity() (*store.DeviceIdentity, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return nil, err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	deviceID := sha256.Sum256(publicKey)
	return &store.DeviceIdentity{
		DeviceID:      fmt.Sprintf("%x", deviceID[:]),
		PublicKeyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})),
		PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})),
	}, nil
}

func signDevicePayload(privateKeyPEM, payload string) (string, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return "", fmt.Errorf("decode private key pem")
	}
	privateAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	privateKey, ok := privateAny.(ed25519.PrivateKey)
	if !ok {
		return "", fmt.Errorf("private key is not ed25519")
	}
	signature := ed25519.Sign(privateKey, []byte(payload))
	return base64.RawURLEncoding.EncodeToString(signature), nil
}

func publicKeyRawBase64URLFromPEM(publicKeyPEM string) string {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil {
		return ""
	}
	publicAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return ""
	}
	publicKey, ok := publicAny.(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(publicKey)
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
	case "done", "completed", "complete", "ok", "final", "stopped", "aborted", "cancelled", "canceled", "error", "failed":
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
