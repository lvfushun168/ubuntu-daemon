package gateway

import (
	"bytes"
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
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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

var markdownImagePattern = regexp.MustCompile(`!\[[^\]]*\]\((https?://[^)\s]+)\)`)

type Adapter struct {
	cfg        *config.Config
	logger     *log.Logger
	dialer     *websocket.Dialer
	stateStore *store.FileStore
	httpClient *http.Client
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

type mediaUploadInitRequest struct {
	DeviceUUID string `json:"deviceUuid"`
	SessionID  string `json:"sessionId,omitempty"`
	MsgID      string `json:"msgId,omitempty"`
	MediaType  string `json:"mediaType"`
	OriginName string `json:"originName,omitempty"`
	FileSize   int64  `json:"fileSize"`
}

type mediaUploadInitData struct {
	MediaID    string `json:"mediaId"`
	UploadURL  string `json:"uploadUrl"`
	PreviewURL string `json:"previewUrl"`
}

type mediaUploadCompleteRequest struct {
	MediaID  string `json:"mediaId"`
	MimeType string `json:"mimeType,omitempty"`
	FileSize int64  `json:"fileSize,omitempty"`
}

type mediaUploadCompleteData struct {
	MediaID    string `json:"mediaId"`
	PreviewURL string `json:"previewUrl"`
	Status     string `json:"status"`
}

type resultWrapper[T any] struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	ErrorCode string `json:"errorCode"`
	Data      T      `json:"data"`
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
		httpClient: &http.Client{Timeout: 30 * time.Second},
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

	replies, err := a.collectChatReplies(ctx, conn, payload.SessionID, runID, cloudMsgID)
	if err != nil {
		return nil, err
	}
	if len(replies) == 0 {
		return []protocol.ChatReplyPayload{{
			RequestMsgID: cloudMsgID,
			RunID:        runID,
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
		if envelope.Event == "chat" || envelope.Event == "session.message" || envelope.Event == "agent" {
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

func (a *Adapter) collectChatReplies(ctx context.Context, conn *websocket.Conn, sessionID, runID, cloudMsgID string) ([]protocol.ChatReplyPayload, error) {
	replies := make([]protocol.ChatReplyPayload, 0, 8)
	chunkSeq := 0
	assembledText := ""
	pendingSnapshotText := ""
	attachments := make([]protocol.ChatAttachment, 0, 2)
	lastMatchedEvent := ""
	openClawSessionID := ""
	transcriptMinAssistantAfterMs := int64(0)
	pendingSnapshotVerified := false

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
		if event.Event != "chat" && event.Event != "session.message" && event.Event != "agent" {
			continue
		}

		var payloadMap map[string]interface{}
		if len(event.Payload) > 0 {
			if err := json.Unmarshal(event.Payload, &payloadMap); err != nil {
				continue
			}
		}
		if event.Event == "agent" {
			if encoded, err := json.Marshal(payloadMap); err == nil {
				a.logger.Printf("raw agent event payload=%s", string(encoded))
			} else {
				a.logger.Printf("raw agent event payload_keys=%v", mapKeys(payloadMap))
			}
		}
		if !matchesChatEvent(payloadMap, sessionID, runID) {
			continue
		}
		lastMatchedEvent = event.Event
		if transcriptSessionID := extractTranscriptSessionID(payloadMap, sessionID); transcriptSessionID != "" {
			openClawSessionID = transcriptSessionID
		}
		if candidate := extractTranscriptMinAssistantAfterMs(payloadMap); candidate > transcriptMinAssistantAfterMs {
			transcriptMinAssistantAfterMs = candidate
		}

		text, textMode := extractChatText(payloadMap)
		if textMode == chatTextModeDelta {
			deltaText, nextAssembledText := incrementalReplyText(assembledText, text)
			assembledText = nextAssembledText
			if deltaText != "" {
				chunkSeq++
				replies = append(replies, protocol.ChatReplyPayload{
					RequestMsgID: cloudMsgID,
					RunID:        runID,
					Role:         "assistant",
					Text:         deltaText,
					ChunkSeq:     chunkSeq,
				})
			}
		} else if textMode == chatTextModeSnapshot {
			snapshotText := strings.Trim(text, "\n")
			if snapshotText != "" {
				if snapshotBelongsToCurrentTurn(payloadMap, runID, transcriptMinAssistantAfterMs) {
					pendingSnapshotText = snapshotText
					pendingSnapshotVerified = true
				} else {
					a.logger.Printf("ignore unverified snapshot event=%s sessionID=%s runID=%s keys=%v",
						event.Event, sessionID, runID, mapKeys(payloadMap))
				}
			}
		}

		attachmentText := assembledText
		if pendingSnapshotText != "" {
			attachmentText = pendingSnapshotText
		}
		attachments = append(attachments, extractMediaAttachments(payloadMap, attachmentText)...)
		attachments = deduplicateAttachments(attachments)

		if isTerminalChatEvent(event.Event, payloadMap) {
			needTranscriptLookup := strings.TrimSpace(openClawSessionID) != "" &&
				(transcriptMinAssistantAfterMs > 0 || strings.TrimSpace(assembledText) == "" ||
					strings.TrimSpace(pendingSnapshotText) == "" || !pendingSnapshotVerified)
			if needTranscriptLookup {
				transcriptText, transcriptAttachments, err := a.loadTranscriptAssistantReply(ctx, openClawSessionID, transcriptMinAssistantAfterMs)
				if err != nil {
					a.logger.Printf("load transcript assistant reply failed sessionID=%s openclawSessionID=%s err=%v",
						sessionID, openClawSessionID, err)
				} else if strings.TrimSpace(transcriptText) != "" {
					attachments = append(attachments, transcriptAttachments...)
					attachments = deduplicateAttachments(attachments)
					baseText := assembledText
					if strings.TrimSpace(baseText) == "" {
						baseText = pendingSnapshotText
					}
					if strings.TrimSpace(baseText) == "" {
						pendingSnapshotText = transcriptText
						pendingSnapshotVerified = true
					} else {
						deltaText, nextAssembledText := incrementalReplyText(baseText, transcriptText)
						assembledText = nextAssembledText
						pendingSnapshotText = transcriptText
						pendingSnapshotVerified = true
						if deltaText != "" {
							chunkSeq++
							replies = append(replies, protocol.ChatReplyPayload{
								RequestMsgID: cloudMsgID,
								RunID:        runID,
								Role:         "assistant",
								Text:         deltaText,
								ChunkSeq:     chunkSeq,
							})
						}
					}
					a.logger.Printf("recovered assistant reply from transcript sessionID=%s openclawSessionID=%s",
						sessionID, openClawSessionID)
				}
			}
			if strings.TrimSpace(assembledText) == "" && strings.TrimSpace(pendingSnapshotText) == "" {
				if encoded, err := json.Marshal(payloadMap); err == nil {
					a.logger.Printf("terminal chat event without extracted text event=%s sessionID=%s runID=%s payload=%s",
						lastMatchedEvent, sessionID, runID, string(encoded))
				} else {
					a.logger.Printf("terminal chat event without extracted text event=%s sessionID=%s runID=%s payload_keys=%v",
						lastMatchedEvent, sessionID, runID, mapKeys(payloadMap))
				}
			}
			if pendingSnapshotText != "" {
				if len(replies) == 0 {
					replies = []protocol.ChatReplyPayload{
						{
							RequestMsgID: cloudMsgID,
							RunID:        runID,
							Role:         "assistant",
							Text:         pendingSnapshotText,
							ChunkSeq:     1,
						},
					}
					chunkSeq = 1
					assembledText = pendingSnapshotText
				} else if strings.TrimSpace(assembledText) != "" {
					deltaText, nextAssembledText := incrementalReplyText(assembledText, pendingSnapshotText)
					assembledText = nextAssembledText
					if deltaText != "" {
						chunkSeq++
						replies = append(replies, protocol.ChatReplyPayload{
							RequestMsgID: cloudMsgID,
							RunID:        runID,
							Role:         "assistant",
							Text:         deltaText,
							ChunkSeq:     chunkSeq,
						})
					}
				}
			}
			if len(replies) == 0 {
				chunkSeq++
				replies = append(replies, protocol.ChatReplyPayload{
					RequestMsgID: cloudMsgID,
					RunID:        runID,
					Role:         "assistant",
					Text:         "",
					ChunkSeq:     chunkSeq,
				})
			}

			last := len(replies) - 1
			replies[last].IsFinal = true
			replies[last].IsEnd = true
			replies[last].FinishReason = finishReason(payloadMap)
			attachments = a.uploadLocalAttachments(ctx, sessionID, cloudMsgID, attachments)
			if len(attachments) > 0 {
				replies[last].Text = rewriteMarkdownImageURLs(replies[last].Text, attachments)
				replies[last].Attachments = attachments
				if errorCode, errorMessage, ok := summarizeAttachmentFailures(attachments); ok {
					replies[last].FinishReason = "error"
					if strings.TrimSpace(replies[last].ErrorCode) == "" {
						replies[last].ErrorCode = errorCode
					}
					if strings.TrimSpace(replies[last].ErrorMessage) == "" {
						replies[last].ErrorMessage = errorMessage
					}
					replies[last].Text = appendAttachmentFailureNotice(replies[last].Text, errorMessage)
				}
			}
			if strings.TrimSpace(replies[last].Text) == "" &&
				len(replies[last].Attachments) == 0 &&
				replies[last].FinishReason == "stop" {
				replies[last].FinishReason = "error"
				replies[last].ErrorCode = "ASSISTANT_REPLY_MISSING"
				replies[last].ErrorMessage = "assistant reply not available before resolve timeout"
			}

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

func snapshotBelongsToCurrentTurn(payload map[string]interface{}, runID string, userTimestampMs int64) bool {
	if len(payload) == 0 {
		return false
	}
	if runID != "" && anyStringEquals(payload, runID, "runId", "run_id") {
		return true
	}
	if userTimestampMs <= 0 {
		return false
	}
	message, ok := payload["message"].(map[string]interface{})
	if !ok {
		return false
	}
	role := strings.ToLower(strings.TrimSpace(firstString(message, "role")))
	if role != "" && role != "assistant" {
		return false
	}
	return timestampMillis(message["timestamp"]) > userTimestampMs
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
	if sessionID != "" && anySessionMatches(payload, sessionID, "sessionKey", "sessionId", "session_id") {
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

func anySessionMatches(values map[string]interface{}, target string, keys ...string) bool {
	for _, key := range keys {
		text, ok := values[key].(string)
		if !ok {
			continue
		}
		if sessionIdentifierMatches(strings.TrimSpace(text), target) {
			return true
		}
	}
	return false
}

func sessionIdentifierMatches(candidate, target string) bool {
	candidate = strings.TrimSpace(candidate)
	target = strings.TrimSpace(target)
	if candidate == "" || target == "" {
		return false
	}
	if candidate == target {
		return true
	}
	return strings.HasSuffix(candidate, ":"+target)
}

func extractTranscriptSessionID(payload map[string]interface{}, cloudSessionID string) string {
	if len(payload) == 0 {
		return ""
	}
	for _, key := range []string{"sessionId", "session_id"} {
		if candidate, ok := payload[key].(string); ok {
			candidate = strings.TrimSpace(candidate)
			if candidate != "" && !sessionIdentifierMatches(candidate, cloudSessionID) {
				return candidate
			}
		}
	}
	for _, key := range []string{"session", "payload", "data"} {
		nested, ok := payload[key].(map[string]interface{})
		if !ok {
			continue
		}
		if candidate := extractTranscriptSessionID(nested, cloudSessionID); candidate != "" {
			return candidate
		}
	}
	return ""
}

func extractTranscriptMinAssistantAfterMs(payload map[string]interface{}) int64 {
	if len(payload) == 0 {
		return 0
	}
	if message, ok := payload["message"].(map[string]interface{}); ok {
		role := strings.ToLower(strings.TrimSpace(firstString(message, "role")))
		if role == "user" {
			if ts := timestampMillis(message["timestamp"]); ts > 0 {
				return ts
			}
		}
	}
	for _, key := range []string{"payload", "data", "session"} {
		nested, ok := payload[key].(map[string]interface{})
		if !ok {
			continue
		}
		if ts := extractTranscriptMinAssistantAfterMs(nested); ts > 0 {
			return ts
		}
	}
	return 0
}

type chatTextMode string

const (
	chatTextModeNone     chatTextMode = ""
	chatTextModeDelta    chatTextMode = "delta"
	chatTextModeSnapshot chatTextMode = "snapshot"
)

func extractChatText(payload map[string]interface{}) (string, chatTextMode) {
	if len(payload) == 0 {
		return "", chatTextModeNone
	}
	for _, key := range []string{"payload", "data"} {
		if nested, ok := payload[key].(map[string]interface{}); ok {
			if text, mode := extractChatText(nested); mode != chatTextModeNone {
				return text, mode
			}
		}
	}
	if delta, ok := payload["delta"].(string); ok && delta != "" {
		return delta, chatTextModeDelta
	}
	if stream := strings.ToLower(strings.TrimSpace(firstString(payload, "stream"))); stream == "assistant" {
		if delta := deepString(payload["delta"]); delta != "" {
			return delta, chatTextModeDelta
		}
		if text := deepString(payload["text"]); text != "" {
			return text, chatTextModeSnapshot
		}
		if text := extractMarkdownBlocks(payload["content"]); text != "" {
			return text, chatTextModeSnapshot
		}
	}
	if text := extractAssistantMessageText(payload); text != "" {
		return text, chatTextModeSnapshot
	}
	for _, key := range []string{"content", "parts"} {
		if text := extractMarkdownBlocks(payload[key]); text != "" {
			return text, chatTextModeSnapshot
		}
	}
	if text, ok := payload["text"].(string); ok && text != "" {
		return text, chatTextModeSnapshot
	}
	return "", chatTextModeNone
}

func extractAssistantMessageText(payload map[string]interface{}) string {
	rawMessage, ok := payload["message"]
	if !ok {
		return ""
	}
	message, ok := rawMessage.(map[string]interface{})
	if !ok {
		return ""
	}
	role := strings.ToLower(strings.TrimSpace(firstString(message, "role")))
	if role != "" && role != "assistant" {
		return ""
	}
	if text := extractMarkdownBlocks(message["content"]); text != "" {
		return text
	}
	for _, key := range []string{"text", "value"} {
		if text, ok := message[key].(string); ok && text != "" {
			return text
		}
	}
	return ""
}

func extractMarkdownBlocks(value interface{}) string {
	switch v := value.(type) {
	case map[string]interface{}:
		if content, ok := v["content"]; ok {
			if text := extractMarkdownBlocks(content); text != "" {
				return text
			}
		}
		if block := markdownFromContentBlock(v); block != "" {
			return block
		}
		return ""
	case []interface{}:
		blocks := make([]string, 0, len(v))
		for _, item := range v {
			block := extractMarkdownBlocks(item)
			if strings.TrimSpace(block) == "" {
				continue
			}
			blocks = append(blocks, block)
		}
		return joinMarkdownBlocks(blocks)
	default:
		return ""
	}
}

func markdownFromContentBlock(block map[string]interface{}) string {
	blockType := strings.ToLower(strings.TrimSpace(firstString(block, "type", "kind")))
	switch blockType {
	case "", "text", "markdown", "md":
		return deepString(block["text"])
	case "input_text", "output_text":
		if text := deepString(block["text"]); text != "" {
			return text
		}
		return deepString(block["value"])
	case "code", "input_code", "output_code":
		code := firstNonEmptyString(block, "code", "text", "value", "content")
		if strings.TrimSpace(code) == "" {
			return ""
		}
		language := firstNonEmptyString(block, "language", "lang")
		return buildMarkdownCodeFence(language, code)
	default:
		if text := deepString(block["text"]); text != "" {
			return text
		}
		if text := deepString(block["value"]); text != "" {
			return text
		}
		return ""
	}
}

func joinMarkdownBlocks(blocks []string) string {
	if len(blocks) == 0 {
		return ""
	}
	var builder strings.Builder
	for i, block := range blocks {
		if i > 0 {
			prev := blocks[i-1]
			if !strings.HasSuffix(prev, "\n") {
				builder.WriteString("\n")
			}
			if !strings.HasSuffix(prev, "\n\n") && !strings.HasPrefix(block, "\n") {
				builder.WriteString("\n")
			}
		}
		builder.WriteString(block)
	}
	return builder.String()
}

func buildMarkdownCodeFence(language, code string) string {
	code = strings.Trim(code, "\n")
	if code == "" {
		return ""
	}
	language = strings.TrimSpace(language)
	if language != "" {
		return fmt.Sprintf("```%s\n%s\n```", language, code)
	}
	return fmt.Sprintf("```\n%s\n```", code)
}

func incrementalReplyText(assembledText, currentText string) (string, string) {
	currentText = strings.Trim(currentText, "\n")
	if currentText == "" {
		return "", assembledText
	}
	if assembledText == "" {
		return currentText, currentText
	}
	if currentText == assembledText {
		return "", assembledText
	}
	if strings.HasPrefix(currentText, assembledText) {
		return currentText[len(assembledText):], currentText
	}
	return currentText, assembledText + currentText
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

func isTerminalChatEvent(eventName string, payload map[string]interface{}) bool {
	eventName = strings.ToLower(strings.TrimSpace(eventName))
	status := strings.ToLower(firstString(payload, "status", "phase", "state"))
	if eventName == "agent" {
		if stream := strings.ToLower(strings.TrimSpace(firstString(payload, "stream"))); stream == "lifecycle" {
			switch strings.ToLower(strings.TrimSpace(firstString(payload, "event", "action"))) {
			case "end", "error":
				return true
			}
		}
		return false
	}
	switch status {
	case "done", "completed", "complete", "ok", "final", "stopped", "aborted", "cancelled", "canceled", "error", "failed":
		return true
	}
	if stream := strings.ToLower(strings.TrimSpace(firstString(payload, "stream"))); stream == "lifecycle" {
		if event := strings.ToLower(strings.TrimSpace(firstString(payload, "event", "action"))); event == "end" || event == "error" {
			return true
		}
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
	if stream := strings.ToLower(strings.TrimSpace(firstString(payload, "stream"))); stream == "lifecycle" {
		switch strings.ToLower(strings.TrimSpace(firstString(payload, "event", "action"))) {
		case "error":
			return "error"
		case "end":
			return "stop"
		}
	}
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

func firstNonEmptyString(values map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if text := deepString(values[key]); strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func mapKeys(values map[string]interface{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
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

func (a *Adapter) loadTranscriptAssistantReply(ctx context.Context, openClawSessionID string, minAssistantAfterMs int64) (string, []protocol.ChatAttachment, error) {
	openClawSessionID = strings.TrimSpace(openClawSessionID)
	if openClawSessionID == "" {
		return "", nil, nil
	}
	resolveTimeout := time.Duration(a.cfg.OpenClawConfig().ReplyResolveTimeoutSec) * time.Second
	if resolveTimeout <= 0 {
		resolveTimeout = 120 * time.Second
	}
	deadline := time.Now().Add(resolveTimeout)
	bestText := ""
	var bestAttachments []protocol.ChatAttachment
	for {
		text, attachments, assistantTimestamp, complete, err := a.readTranscriptAssistantReply(openClawSessionID)
		if err != nil {
			return "", nil, err
		}
		if strings.TrimSpace(text) != "" && (minAssistantAfterMs <= 0 || assistantTimestamp > minAssistantAfterMs) {
			bestText = text
			bestAttachments = attachments
		}
		if strings.TrimSpace(bestText) != "" && complete && (minAssistantAfterMs <= 0 || assistantTimestamp > minAssistantAfterMs) {
			return text, attachments, nil
		}
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
		if time.Now().After(deadline) {
			return bestText, bestAttachments, nil
		}
		select {
		case <-ctx.Done():
			return bestText, bestAttachments, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (a *Adapter) readTranscriptAssistantReply(openClawSessionID string) (string, []protocol.ChatAttachment, int64, bool, error) {
	path := a.transcriptSessionPath(openClawSessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, 0, false, nil
		}
		return "", nil, 0, false, fmt.Errorf("read transcript %s: %w", path, err)
	}
	var (
		currentAssistantTexts       []string
		currentAssistantAttachments []protocol.ChatAttachment
		currentToolAttachments      []protocol.ChatAttachment
		lastAssistantTimestamp      int64
		lastAssistantStopReason     string
		currentTurnStarted          bool
	)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if strings.ToLower(strings.TrimSpace(firstString(entry, "type"))) != "message" {
			continue
		}
		message, ok := entry["message"].(map[string]interface{})
		if !ok {
			continue
		}
		entryTimestamp := transcriptEntryTimestampMillis(entry, message)

		switch role := strings.ToLower(strings.TrimSpace(firstString(message, "role"))); role {
		case "user":
			currentTurnStarted = true
			currentAssistantTexts = nil
			currentAssistantAttachments = nil
			currentToolAttachments = nil
			lastAssistantTimestamp = 0
			lastAssistantStopReason = ""
		case "assistant":
			if !currentTurnStarted {
				continue
			}
			text := strings.Trim(extractMarkdownBlocks(message["content"]), "\n")
			if text == "" {
				text = strings.Trim(firstNonEmptyString(message, "text", "value"), "\n")
			}
			if text != "" {
				currentAssistantTexts = append(currentAssistantTexts, text)
			}
			currentAssistantAttachments = append(currentAssistantAttachments, extractMediaAttachments(message, text)...)
			currentAssistantAttachments = deduplicateAttachments(currentAssistantAttachments)
			lastAssistantTimestamp = entryTimestamp
			lastAssistantStopReason = strings.ToLower(strings.TrimSpace(firstString(message, "stopReason", "stop_reason")))
		case "toolresult":
			if !currentTurnStarted {
				continue
			}
			currentToolAttachments = append(currentToolAttachments, extractTranscriptToolAttachments(message)...)
			currentToolAttachments = deduplicateAttachments(currentToolAttachments)
		}
	}

	complete := lastAssistantTimestamp > 0 && lastAssistantStopReason != "tooluse" && lastAssistantStopReason != "tool_use"
	return joinMarkdownBlocks(currentAssistantTexts),
		preferredTranscriptAttachments(currentToolAttachments, currentAssistantAttachments),
		lastAssistantTimestamp,
		complete,
		nil
}

func extractTranscriptToolAttachments(message map[string]interface{}) []protocol.ChatAttachment {
	if len(message) == 0 {
		return nil
	}
	results := make([]protocol.ChatAttachment, 0, 2)
	if details, ok := message["details"].(map[string]interface{}); ok {
		results = append(results, extractMediaAttachments(details, "")...)
		if media, ok := details["media"].(map[string]interface{}); ok {
			results = append(results, extractMediaAttachments(media, "")...)
		}
	}
	results = append(results, extractMediaAttachments(message, "")...)
	return deduplicateAttachments(results)
}

func preferredTranscriptAttachments(toolAttachments, assistantAttachments []protocol.ChatAttachment) []protocol.ChatAttachment {
	toolAttachments = deduplicateAttachments(toolAttachments)
	assistantAttachments = deduplicateAttachments(assistantAttachments)
	if hasLocalAttachment(toolAttachments) {
		return toolAttachments
	}
	return deduplicateAttachments(append(toolAttachments, assistantAttachments...))
}

func hasLocalAttachment(items []protocol.ChatAttachment) bool {
	for _, item := range items {
		if strings.TrimSpace(item.LocalPath) != "" {
			return true
		}
	}
	return false
}

func transcriptEntryTimestampMillis(entry map[string]interface{}, message map[string]interface{}) int64 {
	if ts := timestampMillis(message["timestamp"]); ts > 0 {
		return ts
	}
	if ts := timestampMillis(entry["timestamp"]); ts > 0 {
		return ts
	}
	return 0
}

func timestampMillis(value interface{}) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return 0
		}
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed.UnixMilli()
		}
	}
	return 0
}

func (a *Adapter) transcriptSessionPath(openClawSessionID string) string {
	openClawCfg := a.cfg.OpenClawConfig()
	workDir := strings.TrimSpace(openClawCfg.WorkDir)
	if workDir == "" {
		switch {
		case strings.TrimSpace(openClawCfg.JSONConfigFile) != "":
			workDir = filepath.Dir(openClawCfg.JSONConfigFile)
		case strings.TrimSpace(openClawCfg.EnvFile) != "":
			workDir = filepath.Dir(openClawCfg.EnvFile)
		default:
			workDir = "."
		}
	}
	return filepath.Join(workDir, "agents", "main", "sessions", openClawSessionID+".jsonl")
}

func extractMediaAttachments(payload map[string]interface{}, assembledText string) []protocol.ChatAttachment {
	attachments := make([]protocol.ChatAttachment, 0, 4)
	for _, line := range extractMediaLines(assembledText) {
		attachments = append(attachments, mediaAttachmentFromRef(line))
	}
	for _, ref := range extractMarkdownImageRefs(assembledText) {
		attachments = append(attachments, mediaAttachmentFromRef(ref))
	}
	for _, ref := range extractMediaRefsFromPayload(payload) {
		attachments = append(attachments, mediaAttachmentFromRef(ref))
	}
	return attachments
}

func extractMediaLines(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	results := make([]string, 0, 2)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(trimmed), "MEDIA:") {
			ref := strings.TrimSpace(trimmed[len("MEDIA:"):])
			if ref != "" {
				results = append(results, ref)
			}
		}
	}
	return results
}

func extractMarkdownImageRefs(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	matches := markdownImagePattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	results := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		ref := strings.TrimSpace(match[1])
		if ref != "" {
			results = append(results, ref)
		}
	}
	return results
}

func rewriteMarkdownImageURLs(text string, attachments []protocol.ChatAttachment) string {
	if strings.TrimSpace(text) == "" || len(attachments) == 0 {
		return text
	}
	replacementURLs := make([]string, 0, len(attachments))
	for _, item := range attachments {
		if strings.TrimSpace(item.PreviewURL) == "" {
			continue
		}
		if normalizeMediaType(item.MediaType) != "image" {
			continue
		}
		replacementURLs = append(replacementURLs, strings.TrimSpace(item.PreviewURL))
	}
	if len(replacementURLs) == 0 {
		return text
	}
	index := 0
	return markdownImagePattern.ReplaceAllStringFunc(text, func(match string) string {
		if index >= len(replacementURLs) {
			return match
		}
		groups := markdownImagePattern.FindStringSubmatch(match)
		if len(groups) < 2 {
			return match
		}
		replaced := strings.Replace(match, groups[1], replacementURLs[index], 1)
		index++
		return replaced
	})
}

func extractMediaRefsFromPayload(payload map[string]interface{}) []string {
	if len(payload) == 0 {
		return nil
	}
	results := make([]string, 0, 2)
	for _, key := range []string{"mediaUrl", "media_url", "url"} {
		if text := strings.TrimSpace(deepString(payload[key])); text != "" {
			results = append(results, text)
		}
	}
	for _, key := range []string{"mediaUrls", "media_urls", "urls"} {
		items, ok := payload[key].([]interface{})
		if !ok {
			continue
		}
		for _, item := range items {
			if text := strings.TrimSpace(deepString(item)); text != "" {
				results = append(results, text)
			}
		}
	}
	return results
}

func mediaAttachmentFromRef(ref string) protocol.ChatAttachment {
	ref = strings.TrimSpace(ref)
	attachment := protocol.ChatAttachment{
		MediaType: detectMediaType(ref),
		MimeType:  detectMimeType(ref),
	}
	if isHTTPURL(ref) {
		attachment.PreviewURL = ref
	} else {
		attachment.LocalPath = ref
		attachment.ErrorCode = "MEDIA_LOCAL_PATH_PENDING_UPLOAD"
		attachment.ErrorMessage = "local media path requires daemon upload workflow"
	}
	return attachment
}

func (a *Adapter) uploadLocalAttachments(ctx context.Context, sessionID, cloudMsgID string, attachments []protocol.ChatAttachment) []protocol.ChatAttachment {
	for i := range attachments {
		item := &attachments[i]
		if strings.TrimSpace(item.LocalPath) == "" {
			continue
		}
		if err := a.uploadAttachmentFile(ctx, sessionID, cloudMsgID, item); err != nil {
			item.ErrorCode = "MEDIA_UPLOAD_FAILED"
			item.ErrorMessage = err.Error()
			a.logger.Printf("upload local media failed path=%s err=%v", item.LocalPath, err)
		}
	}
	return attachments
}

func (a *Adapter) UploadLocalAttachment(ctx context.Context, sessionID, cloudMsgID string, attachment protocol.ChatAttachment) (protocol.ChatAttachment, error) {
	item := attachment
	if strings.TrimSpace(item.LocalPath) == "" {
		return item, fmt.Errorf("local_path is required")
	}
	if err := a.uploadAttachmentFile(ctx, sessionID, cloudMsgID, &item); err != nil {
		return attachment, err
	}
	return item, nil
}

func (a *Adapter) uploadAttachmentFile(ctx context.Context, sessionID, cloudMsgID string, item *protocol.ChatAttachment) error {
	mediaCfg := a.cfg.MediaConfig()
	backendBase := strings.TrimSpace(mediaCfg.BackendBaseURL)
	if backendBase == "" {
		return fmt.Errorf("media.backend_base_url is empty")
	}
	backendBase = strings.TrimRight(backendBase, "/")
	stat, err := os.Stat(item.LocalPath)
	if err != nil {
		return fmt.Errorf("stat media file: %w", err)
	}
	if stat.IsDir() {
		return fmt.Errorf("media path is directory: %s", item.LocalPath)
	}
	initReq := mediaUploadInitRequest{
		DeviceUUID: a.cfg.DeviceID,
		SessionID:  sessionID,
		MsgID:      cloudMsgID,
		MediaType:  normalizeMediaType(item.MediaType),
		OriginName: filepath.Base(item.LocalPath),
		FileSize:   stat.Size(),
	}
	initData, err := a.callUploadInit(ctx, backendBase, initReq)
	if err != nil {
		return err
	}
	if err := a.putFileToPresignedURL(ctx, initData.UploadURL, item.LocalPath, item.MimeType); err != nil {
		return err
	}
	completeData, err := a.callUploadComplete(ctx, backendBase, mediaUploadCompleteRequest{
		MediaID:  initData.MediaID,
		MimeType: item.MimeType,
		FileSize: stat.Size(),
	})
	if err != nil {
		return err
	}
	item.MediaID = initData.MediaID
	item.FileSize = stat.Size()
	item.LocalPath = ""
	item.ErrorCode = ""
	item.ErrorMessage = ""
	if strings.TrimSpace(completeData.PreviewURL) != "" {
		item.PreviewURL = completeData.PreviewURL
	} else if strings.TrimSpace(initData.PreviewURL) != "" {
		item.PreviewURL = initData.PreviewURL
	}
	return nil
}

func normalizeMediaType(raw string) string {
	value := strings.TrimSpace(strings.ToLower(raw))
	if value == "image" || value == "video" {
		return value
	}
	return "image"
}

func (a *Adapter) callUploadInit(ctx context.Context, backendBase string, payload mediaUploadInitRequest) (mediaUploadInitData, error) {
	endpoint := backendBase + "/internal/media/upload-init"
	body, err := json.Marshal(payload)
	if err != nil {
		return mediaUploadInitData{}, fmt.Errorf("marshal upload-init payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return mediaUploadInitData{}, fmt.Errorf("build upload-init request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(a.cfg.MediaConfig().UploadToken); token != "" {
		req.Header.Set("X-Device-Upload-Token", token)
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return mediaUploadInitData{}, fmt.Errorf("request upload-init: %w", err)
	}
	defer resp.Body.Close()
	var result resultWrapper[mediaUploadInitData]
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return mediaUploadInitData{}, fmt.Errorf("decode upload-init response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || result.Code != 200 {
		return mediaUploadInitData{}, fmt.Errorf("upload-init failed http=%d code=%d errorCode=%s message=%s",
			resp.StatusCode, result.Code, result.ErrorCode, result.Message)
	}
	if strings.TrimSpace(result.Data.UploadURL) == "" || strings.TrimSpace(result.Data.MediaID) == "" {
		return mediaUploadInitData{}, fmt.Errorf("upload-init response missing uploadUrl/mediaId")
	}
	return result.Data, nil
}

func (a *Adapter) putFileToPresignedURL(ctx context.Context, uploadURL, localPath, mimeType string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open media file: %w", err)
	}
	defer file.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, file)
	if err != nil {
		return fmt.Errorf("build presigned put request: %w", err)
	}
	if strings.TrimSpace(mimeType) != "" {
		req.Header.Set("Content-Type", mimeType)
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("put media file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("put media file failed http=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (a *Adapter) callUploadComplete(ctx context.Context, backendBase string, payload mediaUploadCompleteRequest) (mediaUploadCompleteData, error) {
	endpoint := backendBase + "/internal/media/upload-complete"
	body, err := json.Marshal(payload)
	if err != nil {
		return mediaUploadCompleteData{}, fmt.Errorf("marshal upload-complete payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return mediaUploadCompleteData{}, fmt.Errorf("build upload-complete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(a.cfg.MediaConfig().UploadToken); token != "" {
		req.Header.Set("X-Device-Upload-Token", token)
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return mediaUploadCompleteData{}, fmt.Errorf("request upload-complete: %w", err)
	}
	defer resp.Body.Close()
	var result resultWrapper[mediaUploadCompleteData]
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return mediaUploadCompleteData{}, fmt.Errorf("decode upload-complete response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || result.Code != 200 {
		return mediaUploadCompleteData{}, fmt.Errorf("upload-complete failed http=%d code=%d errorCode=%s message=%s",
			resp.StatusCode, result.Code, result.ErrorCode, result.Message)
	}
	return result.Data, nil
}

func deduplicateAttachments(items []protocol.ChatAttachment) []protocol.ChatAttachment {
	if len(items) <= 1 {
		return items
	}
	seen := make(map[string]struct{}, len(items))
	result := make([]protocol.ChatAttachment, 0, len(items))
	for _, item := range items {
		key := item.PreviewURL + "|" + item.LocalPath
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, item)
	}
	return result
}

func summarizeAttachmentFailures(items []protocol.ChatAttachment) (string, string, bool) {
	if len(items) == 0 {
		return "", "", false
	}
	hasFailure := false
	for _, item := range items {
		if strings.TrimSpace(item.PreviewURL) != "" || strings.TrimSpace(item.MediaID) != "" {
			return "", "", false
		}
		if strings.TrimSpace(item.ErrorCode) == "" {
			return "", "", false
		}
		hasFailure = true
	}
	if !hasFailure {
		return "", "", false
	}
	for _, item := range items {
		if strings.TrimSpace(item.ErrorMessage) != "" {
			return "MEDIA_UPLOAD_FAILED", item.ErrorMessage, true
		}
	}
	return "MEDIA_UPLOAD_FAILED", "media upload failed", true
}

func appendAttachmentFailureNotice(text, errorMessage string) string {
	notice := "媒体文件上传失败"
	if msg := strings.TrimSpace(errorMessage); msg != "" {
		notice = notice + "：" + msg
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return notice
	}
	if strings.Contains(trimmed, notice) {
		return text
	}
	return strings.TrimRight(text, "\n") + "\n\n" + notice
}

func isHTTPURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

func detectMediaType(ref string) string {
	lower := strings.ToLower(filepath.Ext(ref))
	switch lower {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".bmp":
		return "image"
	case ".mp4", ".webm", ".mov", ".m4v":
		return "video"
	default:
		return "unknown"
	}
}

func detectMimeType(ref string) string {
	switch strings.ToLower(filepath.Ext(ref)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".bmp":
		return "image/bmp"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".m4v":
		return "video/x-m4v"
	default:
		return ""
	}
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
