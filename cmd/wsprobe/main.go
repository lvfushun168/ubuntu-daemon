package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/gorilla/websocket"
)

type request struct {
	Type   string      `json:"type"`
	ID     string      `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params,omitempty"`
}

type response struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   any             `json:"error,omitempty"`
}

type challenge struct {
	Type    string `json:"type"`
	Event   string `json:"event"`
	Payload struct {
		Nonce string `json:"nonce"`
	} `json:"payload"`
}

type daemonState struct {
	GatewayDeviceIdentity *struct {
		DeviceID      string `json:"device_id"`
		PublicKeyPEM  string `json:"public_key_pem"`
		PrivateKeyPEM string `json:"private_key_pem"`
	} `json:"gateway_device_identity"`
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	token := os.Getenv("OPENCLAW_GATEWAY_TOKEN")
	if token == "" {
		token = "123456"
	}
	wsURL := os.Getenv("OPENCLAW_GATEWAY_WS")
	if wsURL == "" {
		wsURL = "ws://127.0.0.1:18789"
	}
	sessionKey := os.Getenv("OPENCLAW_SESSION_KEY")
	if sessionKey == "" {
		sessionKey = "wsprobe-session"
	}
	message := os.Getenv("OPENCLAW_MESSAGE")
	if message == "" {
		message = "请只回复OK"
	}

	pub, priv, deviceID := loadOrCreateIdentity()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, header)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	raw := readFrame(ctx, conn)
	fmt.Printf("FRAME %s\n", raw)

	var ch challenge
	must(json.Unmarshal([]byte(raw), &ch))

	signedAt := time.Now().UnixMilli()
	devicePayload := fmt.Sprintf("%s|%s|%s|%s|%s|%d|%s|%s|%s|%s",
		deviceID, "cli", "cli", "operator", "operator.read,operator.write", signedAt, token, ch.Payload.Nonce, runtime.GOOS, "headless")
	signature := sign(priv, devicePayload)

	connectID := fmt.Sprintf("connect-%d", time.Now().UnixMilli())
	writeJSON(ctx, conn, request{
		Type:   "req",
		ID:     connectID,
		Method: "connect",
		Params: map[string]any{
			"minProtocol": 3,
			"maxProtocol": 3,
			"client": map[string]any{
				"id":           "cli",
				"displayName":  "wsprobe",
				"version":      "0.0.1",
				"platform":     runtime.GOOS,
				"deviceFamily": "headless",
				"mode":         "cli",
			},
			"role":        "operator",
			"scopes":      []string{"operator.read", "operator.write"},
			"caps":        []string{},
			"commands":    []string{},
			"permissions": map[string]any{},
			"auth": map[string]any{
				"token": token,
			},
			"device": map[string]any{
				"id":        deviceID,
				"publicKey": pub,
				"signature": signature,
				"signedAt":  signedAt,
				"nonce":     ch.Payload.Nonce,
			},
			"locale":    "zh-CN",
			"userAgent": "wsprobe/0.0.1",
		},
	})

	for {
		raw = readFrame(ctx, conn)
		fmt.Printf("FRAME %s\n", raw)
		var env struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal([]byte(raw), &env) == nil && env.Type == "res" && env.ID == connectID {
			break
		}
	}

	chatID := fmt.Sprintf("chat-%d", time.Now().UnixMilli())
	writeJSON(ctx, conn, request{
		Type:   "req",
		ID:     chatID,
		Method: "chat.send",
		Params: map[string]any{
			"sessionKey":     sessionKey,
			"idempotencyKey": chatID,
			"message":        message,
		},
	})

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		raw = readFrame(ctx, conn)
		fmt.Printf("FRAME %s\n", raw)
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) {
	must(conn.SetWriteDeadline(time.Now().Add(5 * time.Second)))
	must(conn.WriteJSON(v))
}

func readFrame(ctx context.Context, conn *websocket.Conn) string {
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	_, data, err := conn.ReadMessage()
	must(err)
	return string(data)
}

func loadOrCreateIdentity() (string, string, string) {
	stateFile := os.Getenv("OPENCLAW_DAEMON_STATE")
	if stateFile != "" {
		data, err := os.ReadFile(stateFile)
		must(err)
		var state daemonState
		must(json.Unmarshal(data, &state))
		if state.GatewayDeviceIdentity != nil && state.GatewayDeviceIdentity.DeviceID != "" &&
			state.GatewayDeviceIdentity.PublicKeyPEM != "" && state.GatewayDeviceIdentity.PrivateKeyPEM != "" {
			return publicKeyRawBase64URLFromPEM(state.GatewayDeviceIdentity.PublicKeyPEM), state.GatewayDeviceIdentity.PrivateKeyPEM, state.GatewayDeviceIdentity.DeviceID
		}
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	must(err)
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	must(err)
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	must(err)
	sum := sha256.Sum256(pub)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	return publicKeyRawBase64URLFromPEM(string(pubPEM)), string(privPEM), fmt.Sprintf("%x", sum[:])
}

func sign(privateKeyPEM, payload string) string {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	privateAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	must(err)
	privateKey := privateAny.(ed25519.PrivateKey)
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(payload)))
}

func publicKeyRawBase64URLFromPEM(publicKeyPEM string) string {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	publicAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	must(err)
	publicKey := publicAny.(ed25519.PublicKey)
	return base64.RawURLEncoding.EncodeToString(publicKey)
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
