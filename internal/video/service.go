package video

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"openclaw/dameon/internal/config"
	"openclaw/dameon/internal/protocol"
)

const (
	videoMimeType = "video/mp4"
	videoExt      = ".mp4"
)

type Service struct {
	cfg        *config.Config
	logger     *log.Logger
	uploader   MediaUploader
	httpClient *http.Client

	mu      sync.Mutex
	results map[string]*requestResult
}

type requestResult struct {
	done    chan struct{}
	replies []protocol.ChatReplyPayload
	err     error
}

func NewService(cfg *config.Config, logger *log.Logger, uploader MediaUploader) *Service {
	return &Service{
		cfg:        cfg,
		logger:     logger,
		uploader:   uploader,
		httpClient: &http.Client{},
		results:    make(map[string]*requestResult),
	}
}

func (s *Service) Enabled() bool {
	videoCfg := s.cfg.VideoConfig()
	if !strings.EqualFold(strings.TrimSpace(videoCfg.Provider), "minimax") {
		return false
	}
	if videoCfg.Enabled {
		return true
	}
	envValues, err := loadEnvFile(s.cfg.OpenClawConfig().EnvFile)
	if err == nil {
		if parseBool(envValues["VIDEO_GENERATION_ENABLED"]) {
			return true
		}
	}
	return parseBool(os.Getenv("VIDEO_GENERATION_ENABLED"))
}

func (s *Service) IsVideoRequest(text string) bool {
	return IsVideoRequest(text)
}

func (s *Service) Generate(ctx context.Context, requestMsgID string, payload protocol.ChatMessagePayload) ([]protocol.ChatReplyPayload, error) {
	s.logger.Printf("video generate start request_msg_id=%s session_id=%s text=%q", requestMsgID, payload.SessionID, payload.Text)
	if !s.Enabled() {
		s.logger.Printf("video generate rejected request_msg_id=%s reason=disabled", requestMsgID)
		return s.errorReplies(payload.SessionID, "VIDEO_REQUEST_UNSUPPORTED", "video generation is disabled"), nil
	}

	s.mu.Lock()
	if existing, ok := s.results[requestMsgID]; ok {
		s.mu.Unlock()
		<-existing.done
		return existing.replies, existing.err
	}
	entry := &requestResult{done: make(chan struct{})}
	s.results[requestMsgID] = entry
	s.mu.Unlock()

	entry.replies, entry.err = s.generate(ctx, requestMsgID, payload)
	close(entry.done)
	return entry.replies, entry.err
}

func (s *Service) generate(ctx context.Context, requestMsgID string, payload protocol.ChatMessagePayload) ([]protocol.ChatReplyPayload, error) {
	apiKey, model, duration, resolution, promptOptimizer, err := s.resolveRuntimeConfig()
	if err != nil {
		s.logger.Printf("video runtime config invalid request_msg_id=%s err=%v", requestMsgID, err)
		return s.errorReplies(payload.SessionID, "VIDEO_REQUEST_UNSUPPORTED", err.Error()), nil
	}

	videoCfg := s.cfg.VideoConfig()
	client := NewMiniMaxClient(videoCfg.APIBaseURL, apiKey)
	prompt := NormalizePrompt(payload.Text)
	if prompt == "" {
		return s.errorReplies(payload.SessionID, "VIDEO_REQUEST_UNSUPPORTED", "video prompt is empty"), nil
	}

	progress := protocol.ChatReplyPayload{
		SessionID: payload.SessionID,
		Role:      "assistant",
		Text:      "正在生成视频，预计需要几十秒，请稍候",
		ChunkSeq:  1,
	}

	createCtx, cancelCreate := context.WithTimeout(ctx, time.Duration(videoCfg.CreateTimeoutSec)*time.Second)
	taskID, err := client.CreateTextToVideo(createCtx, GenerationRequest{
		Model:           model,
		Prompt:          prompt,
		Duration:        duration,
		Resolution:      resolution,
		PromptOptimizer: promptOptimizer,
	})
	cancelCreate()
	if err != nil {
		s.logger.Printf("video task create failed request_msg_id=%s err=%v", requestMsgID, err)
		return append([]protocol.ChatReplyPayload{progress},
			s.finalErrorReply(payload.SessionID, 2, "VIDEO_TASK_CREATE_FAILED", err.Error())), nil
	}
	s.logger.Printf("video task created request_msg_id=%s task_id=%s", requestMsgID, taskID)

	status, err := s.pollTask(ctx, client, taskID, videoCfg)
	if err != nil {
		s.logger.Printf("video task timeout request_msg_id=%s task_id=%s err=%v", requestMsgID, taskID, err)
		return append([]protocol.ChatReplyPayload{progress},
			s.finalErrorReply(payload.SessionID, 2, "VIDEO_TASK_TIMEOUT", err.Error())), nil
	}
	if isFailedStatus(status.Status) {
		s.logger.Printf("video task failed request_msg_id=%s task_id=%s status=%s", requestMsgID, status.TaskID, status.Status)
		msg := "video task failed"
		if status.TaskID != "" {
			msg = fmt.Sprintf("video task %s failed", status.TaskID)
		}
		return append([]protocol.ChatReplyPayload{progress},
			s.finalErrorReply(payload.SessionID, 2, "VIDEO_TASK_FAILED", msg)), nil
	}
	if strings.TrimSpace(status.FileID) == "" {
		s.logger.Printf("video task missing file_id request_msg_id=%s task_id=%s", requestMsgID, status.TaskID)
		return append([]protocol.ChatReplyPayload{progress},
			s.finalErrorReply(payload.SessionID, 2, "VIDEO_FILE_RETRIEVE_FAILED", "video task succeeded but file_id is empty")), nil
	}

	queryCtx, cancelRetrieve := context.WithTimeout(ctx, time.Duration(videoCfg.QueryTimeoutSec)*time.Second)
	downloadURL, err := client.RetrieveFile(queryCtx, status.FileID)
	cancelRetrieve()
	if err != nil {
		s.logger.Printf("video file retrieve failed request_msg_id=%s file_id=%s err=%v", requestMsgID, status.FileID, err)
		return append([]protocol.ChatReplyPayload{progress},
			s.finalErrorReply(payload.SessionID, 2, "VIDEO_FILE_RETRIEVE_FAILED", err.Error())), nil
	}

	localPath, err := s.downloadVideo(ctx, requestMsgID, downloadURL, videoCfg)
	if err != nil {
		s.logger.Printf("video download failed request_msg_id=%s url=%s err=%v", requestMsgID, downloadURL, err)
		return append([]protocol.ChatReplyPayload{progress},
			s.finalErrorReply(payload.SessionID, 2, "VIDEO_DOWNLOAD_FAILED", err.Error())), nil
	}

	uploaded, err := s.uploader.UploadLocalAttachment(ctx, payload.SessionID, requestMsgID, protocol.ChatAttachment{
		MediaType: "video",
		MimeType:  videoMimeType,
		LocalPath: localPath,
	})
	if err != nil {
		s.logger.Printf("video upload failed request_msg_id=%s local_path=%s err=%v", requestMsgID, localPath, err)
		return append([]protocol.ChatReplyPayload{progress},
			s.finalErrorReply(payload.SessionID, 2, "VIDEO_UPLOAD_FAILED", err.Error())), nil
	}
	if strings.TrimSpace(uploaded.PreviewURL) == "" || strings.TrimSpace(uploaded.MediaID) == "" {
		s.logger.Printf("video upload missing fields request_msg_id=%s media_id=%s preview_url=%s", requestMsgID, uploaded.MediaID, uploaded.PreviewURL)
		return append([]protocol.ChatReplyPayload{progress},
			s.finalErrorReply(payload.SessionID, 2, "VIDEO_UPLOAD_FAILED", "uploaded video missing media_id or preview_url")), nil
	}

	final := protocol.ChatReplyPayload{
		SessionID:    payload.SessionID,
		Role:         "assistant",
		Text:         "视频已生成，请查看",
		ChunkSeq:     2,
		IsFinal:      true,
		IsEnd:        true,
		FinishReason: "stop",
		Attachments: []protocol.ChatAttachment{
			uploaded,
		},
	}
	s.logger.Printf("video generate success request_msg_id=%s media_id=%s preview_url=%s", requestMsgID, uploaded.MediaID, uploaded.PreviewURL)
	return []protocol.ChatReplyPayload{progress, final}, nil
}

func (s *Service) pollTask(ctx context.Context, client VideoClient, taskID string, videoCfg config.VideoConfig) (TaskStatus, error) {
	pollInterval := time.Duration(videoCfg.PollIntervalSec) * time.Second
	deadline := time.Now().Add(time.Duration(videoCfg.TaskTimeoutSec) * time.Second)
	lastStatus := TaskStatus{TaskID: taskID}
	for {
		if time.Now().After(deadline) {
			return TaskStatus{}, fmt.Errorf("video task timed out task_id=%s last_status=%s", taskID, lastStatus.Status)
		}

		queryCtx, cancelQuery := context.WithTimeout(ctx, time.Duration(videoCfg.QueryTimeoutSec)*time.Second)
		status, err := client.QueryTask(queryCtx, taskID)
		cancelQuery()
		if err != nil {
			lastStatus = TaskStatus{TaskID: taskID}
		} else {
			lastStatus = status
			if isSuccessStatus(status.Status) {
				return status, nil
			}
			if isFailedStatus(status.Status) {
				return status, nil
			}
		}

		select {
		case <-ctx.Done():
			return TaskStatus{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (s *Service) downloadVideo(ctx context.Context, requestMsgID, downloadURL string, videoCfg config.VideoConfig) (string, error) {
	if err := os.MkdirAll(videoCfg.TempDir, 0o755); err != nil {
		return "", fmt.Errorf("create video temp dir: %w", err)
	}

	localPath := filepath.Join(videoCfg.TempDir, requestMsgID+videoExt)
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(videoCfg.DownloadTimeoutSec)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", fmt.Errorf("build video download request: %w", err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download video file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("download video file failed http=%d", resp.StatusCode)
	}

	file, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("create local video file: %w", err)
	}
	defer file.Close()
	if _, err := io.Copy(file, resp.Body); err != nil {
		return "", fmt.Errorf("write local video file: %w", err)
	}
	return localPath, nil
}

func (s *Service) resolveRuntimeConfig() (apiKey, model string, duration int, resolution string, promptOptimizer bool, err error) {
	videoCfg := s.cfg.VideoConfig()
	envValues, readErr := loadEnvFile(s.cfg.OpenClawConfig().EnvFile)
	if readErr != nil {
		s.logger.Printf("video env read failed path=%s err=%v", s.cfg.OpenClawConfig().EnvFile, readErr)
	}

	apiKey = resolveEnvReference(firstNonEmpty(os.Getenv("VIDEO_PROVIDER_API_KEY"), envValues["VIDEO_PROVIDER_API_KEY"]), envValues)
	if strings.TrimSpace(apiKey) == "" {
		apiKey = strings.TrimSpace(firstNonEmpty(os.Getenv("MINIMAX_API_KEY"), envValues["MINIMAX_API_KEY"]))
	}
	if strings.TrimSpace(apiKey) == "" {
		err = fmt.Errorf("missing VIDEO_PROVIDER_API_KEY/MINIMAX_API_KEY in daemon env")
		return
	}

	model = strings.TrimSpace(firstNonEmpty(os.Getenv("VIDEO_GENERATION_MODEL"), envValues["VIDEO_GENERATION_MODEL"], videoCfg.DefaultModel))
	model = normalizeModelName(model)
	if model == "" {
		model = "MiniMax-Hailuo-2.3"
	}
	duration = parseIntOrDefault(firstNonEmpty(os.Getenv("VIDEO_GENERATION_DURATION"), envValues["VIDEO_GENERATION_DURATION"]), videoCfg.DefaultDuration)
	resolution = strings.TrimSpace(firstNonEmpty(os.Getenv("VIDEO_GENERATION_RESOLUTION"), envValues["VIDEO_GENERATION_RESOLUTION"], videoCfg.DefaultResolution))
	if resolution == "" {
		resolution = "768P"
	}
	promptOptimizer = videoCfg.PromptOptimizer
	return
}

func parseIntOrDefault(raw string, defaultValue int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultValue
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return defaultValue
	}
	return value
}

func normalizeModelName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if provider, model, ok := strings.Cut(value, "/"); ok {
		if strings.TrimSpace(provider) != "" && strings.TrimSpace(model) != "" {
			return strings.TrimSpace(model)
		}
	}
	return value
}

func parseBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func isSuccessStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "succeeded":
		return true
	default:
		return false
	}
}

func isFailedStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "fail", "failed":
		return true
	default:
		return false
	}
}

func (s *Service) errorReplies(sessionID, code, msg string) []protocol.ChatReplyPayload {
	return []protocol.ChatReplyPayload{s.finalErrorReply(sessionID, 1, code, msg)}
}

func (s *Service) finalErrorReply(sessionID string, chunkSeq int, code, msg string) protocol.ChatReplyPayload {
	return protocol.ChatReplyPayload{
		SessionID:    sessionID,
		Role:         "assistant",
		Text:         "",
		ChunkSeq:     chunkSeq,
		IsFinal:      true,
		IsEnd:        true,
		FinishReason: "error",
		ErrorCode:    code,
		ErrorMessage: msg,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func resolveEnvReference(value string, envValues map[string]string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 4 && strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		key := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}"))
		if key == "" {
			return ""
		}
		if fromOS := strings.TrimSpace(os.Getenv(key)); fromOS != "" {
			return fromOS
		}
		return strings.TrimSpace(envValues[key])
	}
	return value
}

func loadEnvFile(path string) (map[string]string, error) {
	result := map[string]string{}
	content, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			continue
		}
		result[key] = strings.Trim(value, `"'`)
	}
	return result, nil
}
