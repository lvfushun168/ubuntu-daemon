package manager

import (
	"context"
	"encoding/json"
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
	if defaults["videoGenerationModel"].(map[string]interface{})["primary"] != "minimax/video-01" {
		t.Fatalf("unexpected videoGenerationModel defaults: %+v", defaults)
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
