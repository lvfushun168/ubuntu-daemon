package video

import (
	"context"

	"openclaw/dameon/internal/protocol"
)

type GenerationRequest struct {
	Model           string `json:"model"`
	Prompt          string `json:"prompt"`
	FirstFrameImage string `json:"first_frame_image,omitempty"`
	Duration        int    `json:"duration"`
	Resolution      string `json:"resolution"`
	PromptOptimizer bool   `json:"prompt_optimizer"`
}

type TaskStatus struct {
	TaskID string
	Status string
	FileID string
}

type VideoClient interface {
	CreateTextToVideo(ctx context.Context, req GenerationRequest) (string, error)
	QueryTask(ctx context.Context, taskID string) (TaskStatus, error)
	RetrieveFile(ctx context.Context, fileID string) (string, error)
}

type MediaUploader interface {
	UploadLocalAttachment(ctx context.Context, sessionID, cloudMsgID string, attachment protocol.ChatAttachment) (protocol.ChatAttachment, error)
}
