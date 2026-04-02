package manager

import (
	"context"
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
