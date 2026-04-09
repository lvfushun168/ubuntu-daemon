package manager

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openclaw/dameon/internal/config"
	"openclaw/dameon/internal/protocol"
	"openclaw/dameon/internal/store"
)

type stubExecutor struct{}

func (s stubExecutor) Run(ctx context.Context, command string, args []string, workDir string) (string, string, int, error) {
	return "ok", "", 0, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestApplyWritesEnvAndState(t *testing.T) {
	t.Helper()

	tmpDir := t.TempDir()
	cfg := &config.Config{
		DeviceID:      "device-1",
		DaemonVersion: "0.1.0",
		Cloud: config.CloudConfig{
			WSURL: "ws://43.156.161.7:18080/ws/device",
		},
		OpenClaw: config.OpenClawConfig{
			WorkDir:               tmpDir,
			EnvFile:               filepath.Join(tmpDir, ".env"),
			JSONConfigFile:        filepath.Join(tmpDir, "openclaw.json"),
			GatewayHealthURL:      "",
			GatewayTokenEnvKey:    "OPENCLAW_GATEWAY_TOKEN",
			RestartTimeoutSec:     1,
			HealthCheckTimeoutSec: 1,
		},
		Store: config.StoreConfig{
			StateFile: filepath.Join(tmpDir, "state.json"),
		},
	}
	manager := NewConfigManager(cfg, store.NewFileStore(filepath.Join(tmpDir, "state.json")), stubExecutor{})

	reply := manager.Apply(context.Background(), protocol.SysConfigPayload{
		ConfigVersion: 2,
		Config: map[string]string{
			"API_KEY":                "sk-test",
			"OPENCLAW_GATEWAY_TOKEN": "token-1",
			"CHAT_MODEL":             "minimax/MiniMax-M2.7",
			"IMAGE_MODEL":            "minimax/MiniMax-VL-01",
			"IMAGE_GENERATION_MODEL": "minimax/image-01",
			"VIDEO_GENERATION_MODEL": "minimax/video-01",
			"PROVIDER_API_KEY_ENV":   "MINIMAX_API_KEY",
			"PROVIDER_BASE_URL":      "https://api.minimaxi.com/anthropic",
			"PROVIDER_API_TYPE":      "anthropic-messages",
			"MINIMAX_API_KEY":        "provider-key",
		},
	}, "0.1.0")

	if reply.Status != "success" || !reply.Applied {
		t.Fatalf("expected success reply, got %+v", reply)
	}

	envData, err := os.ReadFile(filepath.Join(tmpDir, ".env"))
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	content := string(envData)
	if !strings.Contains(content, "API_KEY='sk-test'") {
		t.Fatalf("env file missing api key: %s", content)
	}
	if !strings.Contains(content, "OPENCLAW_GATEWAY_TOKEN='token-1'") {
		t.Fatalf("env file missing gateway token: %s", content)
	}
	if strings.Contains(content, "PROVIDER_API_KEY_ENV") {
		t.Fatalf("daemon-only provider api key env should not be written to .env: %s", content)
	}
	if strings.Contains(content, "PROVIDER_BASE_URL") {
		t.Fatalf("daemon-only provider base url should not be written to .env: %s", content)
	}
	if !strings.Contains(content, "MINIMAX_API_KEY='provider-key'") {
		t.Fatalf("env file missing provider api key: %s", content)
	}

	jsonData, err := os.ReadFile(filepath.Join(tmpDir, "openclaw.json"))
	if err != nil {
		t.Fatalf("read openclaw json: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(jsonData, &parsed); err != nil {
		t.Fatalf("parse openclaw json: %v", err)
	}
	models := parsed["models"].(map[string]interface{})
	providers := models["providers"].(map[string]interface{})
	minimax := providers["minimax"].(map[string]interface{})
	if minimax["baseUrl"] != "https://api.minimaxi.com/anthropic" {
		t.Fatalf("unexpected provider baseUrl: %+v", minimax)
	}
	if minimax["apiKey"] != "${MINIMAX_API_KEY}" {
		t.Fatalf("unexpected provider apiKey placeholder: %+v", minimax)
	}
	if minimax["api"] != "anthropic-messages" {
		t.Fatalf("unexpected provider api type: %+v", minimax)
	}
	agents := parsed["agents"].(map[string]interface{})
	defaults := agents["defaults"].(map[string]interface{})
	if defaults["imageModel"].(map[string]interface{})["primary"] != "minimax/MiniMax-VL-01" {
		t.Fatalf("unexpected imageModel defaults: %+v", defaults)
	}
	if defaults["imageGenerationModel"].(map[string]interface{})["primary"] != "minimax/image-01" {
		t.Fatalf("unexpected imageGenerationModel defaults: %+v", defaults)
	}
	if _, exists := defaults["videoGenerationModel"]; exists {
		t.Fatalf("videoGenerationModel must not be written into openclaw defaults: %+v", defaults)
	}

	state, err := store.NewFileStore(filepath.Join(tmpDir, "state.json")).Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.LastConfigVersion != 2 {
		t.Fatalf("expected state version 2, got %d", state.LastConfigVersion)
	}
}

func TestApplyUpdatesCloudWSURLWithoutWritingToEnv(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "daemon.json")
	configContent := "{\n" +
		"  \"device_id\": \"device-1\",\n" +
		"  \"daemon_version\": \"0.1.0\",\n" +
		"  \"cloud\": {\"ws_url\": \"ws://old:18080/ws/device\"},\n" +
		"  \"openclaw\": {\n" +
		"    \"work_dir\": \"" + tmpDir + "\",\n" +
		"    \"env_file\": \"" + filepath.Join(tmpDir, ".env") + "\",\n" +
		"    \"json_config_file\": \"" + filepath.Join(tmpDir, "openclaw.json") + "\",\n" +
		"    \"gateway_health_url\": \"\"\n" +
		"  },\n" +
		"  \"store\": {\"state_file\": \"" + filepath.Join(tmpDir, "state.json") + "\"}\n" +
		"}\n"
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("write daemon config: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	manager := NewConfigManager(cfg, store.NewFileStore(filepath.Join(tmpDir, "state.json")), stubExecutor{})

	reply := manager.Apply(context.Background(), protocol.SysConfigPayload{
		ConfigVersion: 1,
		Config: map[string]string{
			"CLOUD_WS_URL": "ws://43.156.161.7:18080/ws/device",
			"API_KEY":      "sk-test",
		},
	}, "0.1.0")
	if reply.Status != "success" {
		t.Fatalf("expected success, got %+v", reply)
	}
	if got := cfg.CloudWSURL(); got != "ws://43.156.161.7:18080/ws/device" {
		t.Fatalf("unexpected cloud ws url: %s", got)
	}

	envData, err := os.ReadFile(filepath.Join(tmpDir, ".env"))
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if strings.Contains(string(envData), "CLOUD_WS_URL") {
		t.Fatalf("daemon-only cloud url should not be written to .env: %s", string(envData))
	}
}

func TestLoadUsesDeviceIDFromEnv(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "daemon.json")
	configContent := "{\n" +
		"  \"device_id\": \"\",\n" +
		"  \"daemon_version\": \"0.1.0\",\n" +
		"  \"cloud\": {\"ws_url\": \"ws://43.156.161.7:18080/ws/device\"}\n" +
		"}\n"
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("write daemon config: %v", err)
	}

	t.Setenv("OPENCLAW_DEVICE_ID", "aa:bb:cc:dd:ee:ff")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.DeviceID != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("unexpected device_id: %s", cfg.DeviceID)
	}
}

func TestApplyBackfillsMinimaxKeyAndClearsStaleCapabilityModels(t *testing.T) {
	tmpDir := t.TempDir()
	jsonPath := filepath.Join(tmpDir, "openclaw.json")
	initialJSON := `{
  "agents": {
    "defaults": {
      "imageModel": {"primary": "minimax/MiniMax-VL-01"},
      "imageGenerationModel": {"primary": "minimax/image-01"},
      "videoGenerationModel": {"primary": "minimax/video-01"}
    }
  },
  "models": {
    "providers": {
      "minimax": {
        "apiKey": "${MINIMAX_API_KEY}"
      }
    }
  }
}`
	if err := os.WriteFile(jsonPath, []byte(initialJSON), 0o644); err != nil {
		t.Fatalf("write initial json: %v", err)
	}

	cfg := &config.Config{
		DeviceID:      "device-1",
		DaemonVersion: "0.1.0",
		Cloud: config.CloudConfig{
			WSURL: "ws://43.156.161.7:18080/ws/device",
		},
		OpenClaw: config.OpenClawConfig{
			WorkDir:               tmpDir,
			EnvFile:               filepath.Join(tmpDir, ".env"),
			JSONConfigFile:        jsonPath,
			GatewayHealthURL:      "",
			GatewayTokenEnvKey:    "OPENCLAW_GATEWAY_TOKEN",
			RestartTimeoutSec:     1,
			HealthCheckTimeoutSec: 1,
		},
		Store: config.StoreConfig{
			StateFile: filepath.Join(tmpDir, "state.json"),
		},
	}
	manager := NewConfigManager(cfg, store.NewFileStore(filepath.Join(tmpDir, "state.json")), stubExecutor{})

	reply := manager.Apply(context.Background(), protocol.SysConfigPayload{
		ConfigVersion: 1,
		Config: map[string]string{
			"API_KEY":                "sk-test",
			"OPENCLAW_GATEWAY_TOKEN": "token-1",
			"CHAT_MODEL":             "moonshot/kimi-k2.5",
		},
	}, "0.1.0")
	if reply.Status != "success" {
		t.Fatalf("expected success, got %+v", reply)
	}

	envData, err := os.ReadFile(filepath.Join(tmpDir, ".env"))
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	content := string(envData)
	if !strings.Contains(content, "MINIMAX_API_KEY='sk-test'") {
		t.Fatalf("env file missing minimax fallback: %s", content)
	}

	jsonData, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read openclaw json: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(jsonData, &parsed); err != nil {
		t.Fatalf("parse openclaw json: %v", err)
	}
	defaults := parsed["agents"].(map[string]interface{})["defaults"].(map[string]interface{})
	if _, exists := defaults["imageModel"]; exists {
		t.Fatalf("stale imageModel should be removed: %+v", defaults)
	}
	if _, exists := defaults["imageGenerationModel"]; exists {
		t.Fatalf("stale imageGenerationModel should be removed: %+v", defaults)
	}
	if _, exists := defaults["videoGenerationModel"]; exists {
		t.Fatalf("stale videoGenerationModel should be removed: %+v", defaults)
	}
}

func TestCheckGatewayHealthIncludesBearerToken(t *testing.T) {
	tmpDir := t.TempDir()
	var authHeader string

	cfg := &config.Config{
		DeviceID:      "device-1",
		DaemonVersion: "0.1.0",
		Cloud: config.CloudConfig{
			WSURL: "ws://43.156.161.7:18080/ws/device",
		},
		OpenClaw: config.OpenClawConfig{
			WorkDir:               tmpDir,
			EnvFile:               filepath.Join(tmpDir, ".env"),
			JSONConfigFile:        filepath.Join(tmpDir, "openclaw.json"),
			GatewayHealthURL:      "http://gateway-health.local/__openclaw__/canvas/",
			GatewayTokenEnvKey:    "OPENCLAW_GATEWAY_TOKEN",
			RestartTimeoutSec:     1,
			HealthCheckTimeoutSec: 1,
		},
		Store: config.StoreConfig{
			StateFile: filepath.Join(tmpDir, "state.json"),
		},
	}
	if err := os.WriteFile(cfg.OpenClaw.EnvFile, []byte("OPENCLAW_GATEWAY_TOKEN='token-1'\n"), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	manager := NewConfigManager(cfg, store.NewFileStore(filepath.Join(tmpDir, "state.json")), stubExecutor{})
	manager.httpClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authHeader = r.Header.Get("Authorization")
			statusCode := http.StatusOK
			if authHeader != "Bearer token-1" {
				statusCode = http.StatusUnauthorized
			}
			return &http.Response{
				StatusCode: statusCode,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Header:     make(http.Header),
			}, nil
		}),
	}
	if err := manager.checkGatewayHealth(context.Background()); err != nil {
		t.Fatalf("expected healthcheck success, got %v", err)
	}
	if authHeader != "Bearer token-1" {
		t.Fatalf("unexpected auth header: %q", authHeader)
	}
}
