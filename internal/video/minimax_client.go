package video

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type MiniMaxClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func NewMiniMaxClient(baseURL, apiKey string) *MiniMaxClient {
	return &MiniMaxClient{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:     strings.TrimSpace(apiKey),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *MiniMaxClient) CreateTextToVideo(ctx context.Context, req GenerationRequest) (string, error) {
	endpoint := c.baseURL + "/v1/video_generation"
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal create video request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build create video request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.applyAuth(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request create video task: %w", err)
	}
	defer resp.Body.Close()

	var parsed struct {
		TaskID   interface{} `json:"task_id"`
		BaseResp struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("decode create video response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("create video task failed http=%d", resp.StatusCode)
	}
	if parsed.BaseResp.StatusCode != 0 {
		return "", fmt.Errorf("create video task failed code=%d message=%s", parsed.BaseResp.StatusCode, parsed.BaseResp.StatusMsg)
	}
	taskID := normalizeString(parsed.TaskID)
	if taskID == "" {
		return "", fmt.Errorf("create video task response missing task_id")
	}
	return taskID, nil
}

func (c *MiniMaxClient) QueryTask(ctx context.Context, taskID string) (TaskStatus, error) {
	endpoint := c.baseURL + "/v1/query/video_generation"
	queryValues := url.Values{}
	queryValues.Set("task_id", taskID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+queryValues.Encode(), nil)
	if err != nil {
		return TaskStatus{}, fmt.Errorf("build query video request: %w", err)
	}
	c.applyAuth(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return TaskStatus{}, fmt.Errorf("request query video task: %w", err)
	}
	defer resp.Body.Close()

	var parsed struct {
		TaskID   interface{} `json:"task_id"`
		Status   interface{} `json:"status"`
		FileID   interface{} `json:"file_id"`
		BaseResp struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return TaskStatus{}, fmt.Errorf("decode query video response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TaskStatus{}, fmt.Errorf("query video task failed http=%d", resp.StatusCode)
	}
	if parsed.BaseResp.StatusCode != 0 {
		return TaskStatus{}, fmt.Errorf("query video task failed code=%d message=%s", parsed.BaseResp.StatusCode, parsed.BaseResp.StatusMsg)
	}
	return TaskStatus{
		TaskID: normalizeString(parsed.TaskID),
		Status: strings.TrimSpace(normalizeString(parsed.Status)),
		FileID: normalizeString(parsed.FileID),
	}, nil
}

func (c *MiniMaxClient) RetrieveFile(ctx context.Context, fileID string) (string, error) {
	endpoint := c.baseURL + "/v1/files/retrieve"
	queryValues := url.Values{}
	queryValues.Set("file_id", fileID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+queryValues.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("build retrieve file request: %w", err)
	}
	c.applyAuth(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request retrieve file: %w", err)
	}
	defer resp.Body.Close()

	var parsed struct {
		File struct {
			DownloadURL string `json:"download_url"`
		} `json:"file"`
		BaseResp struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("decode retrieve file response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("retrieve file failed http=%d", resp.StatusCode)
	}
	if parsed.BaseResp.StatusCode != 0 {
		return "", fmt.Errorf("retrieve file failed code=%d message=%s", parsed.BaseResp.StatusCode, parsed.BaseResp.StatusMsg)
	}
	if strings.TrimSpace(parsed.File.DownloadURL) == "" {
		return "", fmt.Errorf("retrieve file response missing download_url")
	}
	return strings.TrimSpace(parsed.File.DownloadURL), nil
}

func (c *MiniMaxClient) applyAuth(req *http.Request) {
	if c.apiKey == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
}

func normalizeString(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case int:
		return strconv.Itoa(typed)
	default:
		return ""
	}
}
