package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"openclaw/dameon/internal/config"
	"openclaw/dameon/internal/protocol"
	"openclaw/dameon/internal/store"
)

type ConfigManager struct {
	cfg        *config.Config
	stateStore *store.FileStore
	httpClient *http.Client
	executor   CommandExecutor
}

type CommandExecutor interface {
	Run(ctx context.Context, command string, args []string, workDir string) (stdout string, stderr string, exitCode int, err error)
}

func NewConfigManager(cfg *config.Config, stateStore *store.FileStore, executor CommandExecutor) *ConfigManager {
	return &ConfigManager{
		cfg:        cfg,
		stateStore: stateStore,
		executor:   executor,
		httpClient: &http.Client{Timeout: time.Duration(cfg.OpenClaw.HealthCheckTimeoutSec) * time.Second},
	}
}

func (m *ConfigManager) Apply(ctx context.Context, payload protocol.SysConfigPayload, daemonVersion string) protocol.SysConfigAckPayload {
	state, err := m.stateStore.Load()
	if err != nil {
		return failedAck(payload.ConfigVersion, daemonVersion, "INTERNAL_ERROR", err)
	}
	if payload.ConfigVersion <= state.LastConfigVersion {
		return protocol.SysConfigAckPayload{
			ConfigVersion: payload.ConfigVersion,
			Status:        "ignored",
			Applied:       false,
			DaemonVersion: daemonVersion,
			Message:       fmt.Sprintf("config version %d already applied", payload.ConfigVersion),
		}
	}

	openClawCfg := m.cfg.OpenClawConfig()
	if err := os.MkdirAll(openClawCfg.WorkDir, 0o755); err != nil {
		return failedAck(payload.ConfigVersion, daemonVersion, "CONFIG_APPLY_FAILED", err)
	}
	if err := m.updateDaemonCloudWSURL(payload.Config); err != nil {
		return failedAck(payload.ConfigVersion, daemonVersion, "CONFIG_APPLY_FAILED", err)
	}
	if err := m.writeEnv(openClawCfg, payload.Config); err != nil {
		return failedAck(payload.ConfigVersion, daemonVersion, "CONFIG_APPLY_FAILED", err)
	}
	if err := m.ensureJSONConfig(openClawCfg, payload.Config); err != nil {
		return failedAck(payload.ConfigVersion, daemonVersion, "CONFIG_APPLY_FAILED", err)
	}
	if err := m.restartOpenClaw(ctx); err != nil {
		return failedAck(payload.ConfigVersion, daemonVersion, "CONFIG_APPLY_FAILED", err)
	}
	if err := m.checkGatewayHealth(ctx); err != nil {
		return failedAck(payload.ConfigVersion, daemonVersion, "GATEWAY_UNAVAILABLE", err)
	}

	now := time.Now().UnixMilli()
	state.LastConfigVersion = payload.ConfigVersion
	if err := m.stateStore.Save(state); err != nil {
		return failedAck(payload.ConfigVersion, daemonVersion, "INTERNAL_ERROR", err)
	}

	return protocol.SysConfigAckPayload{
		ConfigVersion: payload.ConfigVersion,
		Status:        "success",
		Applied:       true,
		AppliedAt:     now,
		DaemonVersion: daemonVersion,
		Message:       "configuration applied and gateway is healthy",
	}
}

func (m *ConfigManager) writeEnv(openClawCfg config.OpenClawConfig, values map[string]string) error {
	effective := make(map[string]string, len(values)+1)
	for key, value := range values {
		effective[key] = value
	}
	// 兼容现有 OpenClaw 配置模板：若未显式下发 VORTEX_OPENAI_API_KEY，则用 API_KEY 兜底。
	// 这样可以避免每次 sys_config 覆盖 .env 后触发网关启动失败。
	if _, ok := effective["VORTEX_OPENAI_API_KEY"]; !ok {
		if apiKey, has := effective["API_KEY"]; has && apiKey != "" {
			effective["VORTEX_OPENAI_API_KEY"] = apiKey
		}
	}

	keys := make([]string, 0, len(effective))
	for key := range effective {
		if isDaemonOnlyConfigKey(key) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("%s=%s", key, shellEscape(effective[key])))
	}
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	return storeWriteFile(openClawCfg.EnvFile, []byte(content), 0o600)
}

func (m *ConfigManager) ensureJSONConfig(openClawCfg config.OpenClawConfig, values map[string]string) error {
	current := map[string]interface{}{}
	if data, err := os.ReadFile(openClawCfg.JSONConfigFile); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &current); err != nil {
			return fmt.Errorf("parse openclaw json config: %w", err)
		}
	}

	gateway := ensureMap(current, "gateway")
	if _, ok := gateway["mode"]; !ok {
		gateway["mode"] = "local"
	}
	if _, ok := gateway["bind"]; !ok {
		gateway["bind"] = "127.0.0.1:18789"
	}
	auth := ensureMap(gateway, "auth")
	if _, ok := auth["token"]; !ok {
		envKey := openClawCfg.GatewayTokenEnvKey
		if envKey == "" {
			envKey = "OPENCLAW_GATEWAY_TOKEN"
		}
		auth["token"] = "${" + envKey + "}"
	}
	current["gateway"] = gateway
	delete(current, "default_model")

	// OpenClaw 2026.4.1 已不支持根级 default_model，默认模型需写入 agents.defaults.model.primary。
	// 这里保持与现有配置结构兼容，避免写入未知字段导致 OpenClaw 启动失败。
	chatModel := firstNonEmptyValue(values, "CHAT_MODEL", "LLM_MODEL")
	if chatModel != "" {
		agents := ensureMap(current, "agents")
		defaults := ensureMap(agents, "defaults")
		model := ensureMap(defaults, "model")
		model["primary"] = chatModel
		if err := applyCapabilityModels(defaults, values); err != nil {
			return err
		}
		if err := applyProviderConfig(current, values); err != nil {
			return err
		}
	}

	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal openclaw json config: %w", err)
	}
	data = append(data, '\n')
	return storeWriteFile(openClawCfg.JSONConfigFile, data, 0o644)
}

func (m *ConfigManager) restartOpenClaw(ctx context.Context) error {
	openClawCfg := m.cfg.OpenClawConfig()
	if openClawCfg.RestartCommand == "" {
		return nil
	}
	restartCtx, cancel := context.WithTimeout(ctx, time.Duration(openClawCfg.RestartTimeoutSec)*time.Second)
	defer cancel()

	stdout, stderr, exitCode, err := m.executor.Run(restartCtx, openClawCfg.RestartCommand, openClawCfg.RestartArgs, openClawCfg.WorkDir)
	if err != nil {
		return fmt.Errorf("restart openclaw: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("restart openclaw exit code=%d stdout=%s stderr=%s", exitCode, stdout, stderr)
	}
	return nil
}

func (m *ConfigManager) checkGatewayHealth(ctx context.Context) error {
	openClawCfg := m.cfg.OpenClawConfig()
	if openClawCfg.GatewayHealthURL == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openClawCfg.GatewayHealthURL, nil)
	if err != nil {
		return err
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("gateway healthcheck status=%d", resp.StatusCode)
	}
	return nil
}

func (m *ConfigManager) updateDaemonCloudWSURL(values map[string]string) error {
	wsURL := strings.TrimSpace(values["CLOUD_WS_URL"])
	if wsURL == "" {
		wsURL = strings.TrimSpace(values["EDGE_GATEWAY_WS_URL"])
	}
	if wsURL == "" {
		return nil
	}
	return m.cfg.UpdateCloudWSURL(wsURL)
}

func isDaemonOnlyConfigKey(key string) bool {
	switch key {
	case "CLOUD_WS_URL", "EDGE_GATEWAY_WS_URL", "PROVIDER_API_KEY_ENV", "PROVIDER_BASE_URL", "PROVIDER_API_TYPE":
		return true
	default:
		return false
	}
}

func applyCapabilityModels(defaults map[string]interface{}, values map[string]string) error {
	if imageModel := strings.TrimSpace(values["IMAGE_MODEL"]); imageModel != "" {
		ensureMap(defaults, "imageModel")["primary"] = imageModel
	}
	if imageGenerationModel := strings.TrimSpace(values["IMAGE_GENERATION_MODEL"]); imageGenerationModel != "" {
		ensureMap(defaults, "imageGenerationModel")["primary"] = imageGenerationModel
	}
	if videoGenerationModel := strings.TrimSpace(values["VIDEO_GENERATION_MODEL"]); videoGenerationModel != "" {
		ensureMap(defaults, "videoGenerationModel")["primary"] = videoGenerationModel
	}
	return nil
}

func applyProviderConfig(current map[string]interface{}, values map[string]string) error {
	baseURL := strings.TrimSpace(values["PROVIDER_BASE_URL"])
	apiKeyEnv := strings.TrimSpace(values["PROVIDER_API_KEY_ENV"])
	apiType := strings.TrimSpace(values["PROVIDER_API_TYPE"])
	if apiType == "" {
		apiType = "openai-completions"
	}
	modelRefs := collectConfiguredModelRefs(values)
	if len(modelRefs) == 0 || (baseURL == "" && apiKeyEnv == "") {
		return nil
	}

	models := ensureMap(current, "models")
	if _, ok := models["mode"]; !ok {
		models["mode"] = "merge"
	}
	providers := ensureMap(models, "providers")
	modelsByProvider := make(map[string][]string)
	for _, ref := range modelRefs {
		providerID, modelID, ok := splitModelRef(ref)
		if !ok {
			continue
		}
		if !containsString(modelsByProvider[providerID], modelID) {
			modelsByProvider[providerID] = append(modelsByProvider[providerID], modelID)
		}
	}
	for providerID, modelIDs := range modelsByProvider {
		providerCfg := ensureMap(providers, providerID)
		providerCfg["api"] = apiType
		if baseURL != "" {
			providerCfg["baseUrl"] = baseURL
		}
		if apiKeyEnv != "" {
			providerCfg["apiKey"] = "${" + apiKeyEnv + "}"
		}
		definitions := make([]interface{}, 0, len(modelIDs))
		for _, modelID := range modelIDs {
			definition := map[string]interface{}{
				"id":   modelID,
				"name": modelID,
			}
			if modelID == "MiniMax-M2.7" || modelID == "MiniMax-M2.7-highspeed" {
				definition["input"] = []interface{}{"text", "image"}
			}
			definitions = append(definitions, definition)
		}
		providerCfg["models"] = definitions
	}
	return nil
}

func splitModelRef(modelRef string) (string, string, bool) {
	parts := strings.SplitN(strings.TrimSpace(modelRef), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func collectConfiguredModelRefs(values map[string]string) []string {
	keys := []string{"CHAT_MODEL", "LLM_MODEL", "IMAGE_MODEL", "IMAGE_GENERATION_MODEL", "VIDEO_GENERATION_MODEL"}
	refs := make([]string, 0, len(keys))
	for _, key := range keys {
		value := strings.TrimSpace(values[key])
		if value == "" || containsString(refs, value) {
			continue
		}
		refs = append(refs, value)
	}
	return refs
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func firstNonEmptyValue(values map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func failedAck(version int64, daemonVersion, code string, err error) protocol.SysConfigAckPayload {
	return protocol.SysConfigAckPayload{
		ConfigVersion: version,
		Status:        "failed",
		Applied:       false,
		DaemonVersion: daemonVersion,
		Message:       "failed to apply configuration",
		ErrorCode:     code,
		ErrorMessage:  err.Error(),
	}
}

func ensureMap(root map[string]interface{}, key string) map[string]interface{} {
	value, ok := root[key]
	if !ok {
		child := map[string]interface{}{}
		root[key] = child
		return child
	}
	child, ok := value.(map[string]interface{})
	if !ok {
		child = map[string]interface{}{}
		root[key] = child
	}
	return child
}

func shellEscape(value string) string {
	quoted := strings.ReplaceAll(value, "'", "'\"'\"'")
	return "'" + quoted + "'"
}

func storeWriteFile(path string, data []byte, mode os.FileMode) error {
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
