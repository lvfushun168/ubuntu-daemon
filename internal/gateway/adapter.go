package gateway

import (
	"context"

	"openclaw/dameon/internal/protocol"
)

type Adapter struct{}

func NewAdapter() *Adapter {
	return &Adapter{}
}

func (a *Adapter) Chat(ctx context.Context, _ protocol.ChatMessagePayload) ([]protocol.ChatReplyPayload, error) {
	_ = ctx
	// TODO: 按 OpenClaw 官方 Gateway Protocol 实现 connect.challenge / req-res / event 适配。
	return []protocol.ChatReplyPayload{
		{
			Role:         "assistant",
			ChunkSeq:     1,
			IsFinal:      true,
			IsEnd:        true,
			FinishReason: "error",
			ErrorCode:    "GATEWAY_UNAVAILABLE",
			ErrorMessage: "gateway adapter is not implemented yet",
		},
	}, nil
}
