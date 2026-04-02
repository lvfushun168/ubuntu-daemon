package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultConfigPath = "config/daemon.json"

type Config struct {
	mu                     sync.RWMutex    `json:"-"`
	path                   string          `json:"-"`
	DeviceID               string          `json:"device_id"`
	DaemonVersion          string          `json:"daemon_version"`
	Cloud                  CloudConfig     `json:"cloud"`
	OpenClaw               OpenClawConfig  `json:"openclaw"`
	RemoteCommand          RemoteCmdConfig `json:"remote_command"`
	Store                  StoreConfig     `json:"store"`
	ChatReplyOnUnsupported bool            `json:"chat_reply_on_unsupported"`
}

type CloudConfig struct {
	WSURL             string `json:"ws_url"`
	ConnectTimeoutSec int    `json:"connect_timeout_sec"`
	PingIntervalSec   int    `json:"ping_interval_sec"`
	ReconnectMinSec   int    `json:"reconnect_min_sec"`
	ReconnectMaxSec   int    `json:"reconnect_max_sec"`
}

type OpenClawConfig struct {
	WorkDir               string   `json:"work_dir"`
	EnvFile               string   `json:"env_file"`
	JSONConfigFile        string   `json:"json_config_file"`
	GatewayHealthURL      string   `json:"gateway_health_url"`
	GatewayTokenEnvKey    string   `json:"gateway_token_env_key"`
	RestartCommand        string   `json:"restart_command"`
	RestartArgs           []string `json:"restart_args"`
	RestartTimeoutSec     int      `json:"restart_timeout_sec"`
	HealthCheckTimeoutSec int      `json:"health_check_timeout_sec"`
}

type RemoteCmdConfig struct {
	DefaultTimeoutSec int      `json:"default_timeout_sec"`
	MaxTimeoutSec     int      `json:"max_timeout_sec"`
	MaxOutputBytes    int      `json:"max_output_bytes"`
	Whitelist         []string `json:"whitelist"`
}

type StoreConfig struct {
	StateFile string `json:"state_file"`
}

func ConfigPathFromEnv() string {
	if value := os.Getenv("OPENCLAW_DAEMON_CONFIG"); value != "" {
		return value
	}
	return defaultConfigPath
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := defaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.path = path
	cfg.applyEnvOverrides()
	if err := cfg.normalize(path); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaultConfig() *Config {
	return &Config{
		DaemonVersion:          "0.1.0",
		ChatReplyOnUnsupported: true,
		Cloud: CloudConfig{
			WSURL:             "ws://43.156.161.7:18080/ws/device",
			ConnectTimeoutSec: 10,
			PingIntervalSec:   30,
			ReconnectMinSec:   1,
			ReconnectMaxSec:   30,
		},
		OpenClaw: OpenClawConfig{
			WorkDir:               "/root/.openclaw",
			EnvFile:               "/root/.openclaw/.env",
			JSONConfigFile:        "/root/.openclaw/openclaw.json",
			GatewayHealthURL:      "http://127.0.0.1:18789/__openclaw__/canvas/",
			GatewayTokenEnvKey:    "OPENCLAW_GATEWAY_TOKEN",
			RestartCommand:        "pm2",
			RestartArgs:           []string{"restart", "openclaw"},
			RestartTimeoutSec:     20,
			HealthCheckTimeoutSec: 5,
		},
		RemoteCommand: RemoteCmdConfig{
			DefaultTimeoutSec: 30,
			MaxTimeoutSec:     300,
			MaxOutputBytes:    64 * 1024,
		},
		Store: StoreConfig{
			StateFile: "/root/.openclaw/daemon-state.json",
		},
	}
}

func (c *Config) normalize(path string) error {
	if c.DeviceID == "" {
		return errors.New("device_id is required")
	}
	if c.Cloud.WSURL == "" {
		return errors.New("cloud.ws_url is required")
	}

	baseDir := filepath.Dir(path)
	c.OpenClaw.WorkDir = absPath(baseDir, c.OpenClaw.WorkDir)
	c.OpenClaw.EnvFile = absPath(baseDir, c.OpenClaw.EnvFile)
	c.OpenClaw.JSONConfigFile = absPath(baseDir, c.OpenClaw.JSONConfigFile)
	c.Store.StateFile = absPath(baseDir, c.Store.StateFile)

	if c.Cloud.ConnectTimeoutSec <= 0 {
		c.Cloud.ConnectTimeoutSec = 10
	}
	if c.Cloud.PingIntervalSec <= 0 {
		c.Cloud.PingIntervalSec = 30
	}
	if c.Cloud.ReconnectMinSec <= 0 {
		c.Cloud.ReconnectMinSec = 1
	}
	if c.Cloud.ReconnectMaxSec < c.Cloud.ReconnectMinSec {
		c.Cloud.ReconnectMaxSec = c.Cloud.ReconnectMinSec
	}
	if c.OpenClaw.RestartTimeoutSec <= 0 {
		c.OpenClaw.RestartTimeoutSec = 20
	}
	if c.OpenClaw.HealthCheckTimeoutSec <= 0 {
		c.OpenClaw.HealthCheckTimeoutSec = 5
	}
	if c.RemoteCommand.DefaultTimeoutSec <= 0 {
		c.RemoteCommand.DefaultTimeoutSec = 30
	}
	if c.RemoteCommand.MaxTimeoutSec < c.RemoteCommand.DefaultTimeoutSec {
		c.RemoteCommand.MaxTimeoutSec = c.RemoteCommand.DefaultTimeoutSec
	}
	if c.RemoteCommand.MaxOutputBytes <= 0 {
		c.RemoteCommand.MaxOutputBytes = 64 * 1024
	}
	return nil
}

func (c *Config) CloudWSURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Cloud.WSURL
}

func (c *Config) OpenClawConfig() OpenClawConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.OpenClaw
}

func (c *Config) UpdateCloudWSURL(wsURL string) error {
	wsURL = strings.TrimSpace(wsURL)
	if wsURL == "" {
		return nil
	}
	c.mu.Lock()
	c.Cloud.WSURL = wsURL
	c.mu.Unlock()
	return c.persist()
}

func (c *Config) applyEnvOverrides() {
	if value := strings.TrimSpace(os.Getenv("OPENCLAW_CLOUD_WS_URL")); value != "" {
		c.Cloud.WSURL = value
		return
	}
	if value := strings.TrimSpace(os.Getenv("EDGE_GATEWAY_WS_URL")); value != "" {
		c.Cloud.WSURL = value
		return
	}

	scheme := strings.TrimSpace(os.Getenv("OPENCLAW_CLOUD_SCHEME"))
	host := strings.TrimSpace(os.Getenv("OPENCLAW_CLOUD_HOST"))
	port := strings.TrimSpace(os.Getenv("OPENCLAW_CLOUD_PORT"))
	wsPath := strings.TrimSpace(os.Getenv("OPENCLAW_CLOUD_WS_PATH"))
	if host == "" {
		return
	}
	if scheme == "" {
		scheme = "ws"
	}
	if port == "" {
		port = "18080"
	}
	if wsPath == "" {
		wsPath = "/ws/device"
	}
	if !strings.HasPrefix(wsPath, "/") {
		wsPath = "/" + wsPath
	}
	c.Cloud.WSURL = fmt.Sprintf("%s://%s:%s%s", scheme, host, port, wsPath)
}

func (c *Config) persist() error {
	c.mu.RLock()
	path := c.path
	payload := struct {
		DeviceID               string          `json:"device_id"`
		DaemonVersion          string          `json:"daemon_version"`
		Cloud                  CloudConfig     `json:"cloud"`
		OpenClaw               OpenClawConfig  `json:"openclaw"`
		RemoteCommand          RemoteCmdConfig `json:"remote_command"`
		Store                  StoreConfig     `json:"store"`
		ChatReplyOnUnsupported bool            `json:"chat_reply_on_unsupported"`
	}{
		DeviceID:               c.DeviceID,
		DaemonVersion:          c.DaemonVersion,
		Cloud:                  c.Cloud,
		OpenClaw:               c.OpenClaw,
		RemoteCommand:          c.RemoteCommand,
		Store:                  c.Store,
		ChatReplyOnUnsupported: c.ChatReplyOnUnsupported,
	}
	c.mu.RUnlock()

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal daemon config: %w", err)
	}
	data = append(data, '\n')
	return writeAtomically(path, data, 0o644)
}

func absPath(baseDir, value string) string {
	if value == "" || filepath.IsAbs(value) {
		return value
	}
	return filepath.Clean(filepath.Join(baseDir, value))
}

func (c CloudConfig) ConnectTimeout() time.Duration {
	return time.Duration(c.ConnectTimeoutSec) * time.Second
}

func (c CloudConfig) PingInterval() time.Duration {
	return time.Duration(c.PingIntervalSec) * time.Second
}

func (c CloudConfig) ReconnectMin() time.Duration {
	return time.Duration(c.ReconnectMinSec) * time.Second
}

func (c CloudConfig) ReconnectMax() time.Duration {
	return time.Duration(c.ReconnectMaxSec) * time.Second
}

func writeAtomically(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName)

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Chmod(mode); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
