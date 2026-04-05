package router

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"openclaw/dameon/internal/gateway"
	"openclaw/dameon/internal/manager"
	"openclaw/dameon/internal/protocol"
	"openclaw/dameon/internal/runner"
)

type Sender interface {
	SendEnvelope(ctx context.Context, msgID, msgType string, payload interface{}) error
}

type MessageRouter struct {
	logger         *log.Logger
	sender         Sender
	configManager  *manager.ConfigManager
	remoteRunner   *runner.RemoteCommandRunner
	gatewayAdapter *gateway.Adapter
	daemonVersion  string
}

func New(logger *log.Logger, sender Sender, configManager *manager.ConfigManager, remoteRunner *runner.RemoteCommandRunner, gatewayAdapter *gateway.Adapter, daemonVersion string) *MessageRouter {
	return &MessageRouter{
		logger:         logger,
		sender:         sender,
		configManager:  configManager,
		remoteRunner:   remoteRunner,
		gatewayAdapter: gatewayAdapter,
		daemonVersion:  daemonVersion,
	}
}

func (r *MessageRouter) SetSender(sender Sender) {
	r.sender = sender
}

func (r *MessageRouter) Handle(ctx context.Context, envelope protocol.Envelope) {
	switch envelope.Type {
	case protocol.TypeSysConfig:
		r.handleSysConfig(ctx, envelope)
	case protocol.TypeRemoteCmd:
		r.handleRemoteCmd(ctx, envelope)
	case protocol.TypeChatMsg:
		r.handleChat(ctx, envelope)
	case protocol.TypeAuthResp, protocol.TypePong:
		return
	default:
		r.logger.Printf("ignore unsupported message type=%s msg_id=%s", envelope.Type, envelope.MsgID)
	}
}

func (r *MessageRouter) handleSysConfig(ctx context.Context, envelope protocol.Envelope) {
	var payload protocol.SysConfigPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		r.reply(ctx, envelope.MsgID, protocol.TypeSysConfigAck, protocol.SysConfigAckPayload{
			ConfigVersion: 0,
			Status:        "failed",
			Applied:       false,
			DaemonVersion: r.daemonVersion,
			ErrorCode:     "INVALID_PAYLOAD",
			ErrorMessage:  err.Error(),
			Message:       "invalid sys_config payload",
		})
		return
	}
	reply := r.configManager.Apply(ctx, payload, r.daemonVersion)
	r.reply(ctx, envelope.MsgID, protocol.TypeSysConfigAck, reply)
}

func (r *MessageRouter) handleRemoteCmd(ctx context.Context, envelope protocol.Envelope) {
	var payload protocol.RemoteCmdPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		r.reply(ctx, envelope.MsgID, protocol.TypeRemoteCmdResult, protocol.RemoteCmdResultPayload{
			Status:          "rejected",
			StdoutTruncated: false,
			StderrTruncated: false,
			Message:         "invalid remote_cmd payload",
			ErrorCode:       "INVALID_PAYLOAD",
			ErrorMessage:    err.Error(),
		})
		return
	}
	reply := r.remoteRunner.Execute(ctx, payload)
	r.reply(ctx, envelope.MsgID, protocol.TypeRemoteCmdResult, reply)
}

func (r *MessageRouter) handleChat(ctx context.Context, envelope protocol.Envelope) {
	var payload protocol.ChatMessagePayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		r.reply(ctx, envelope.MsgID, protocol.TypeChatReply, protocol.ChatReplyPayload{
			RequestMsgID: envelope.MsgID,
			Role:         "assistant",
			ChunkSeq:     1,
			IsFinal:      true,
			IsEnd:        true,
			FinishReason: "error",
			ErrorCode:    "INVALID_PAYLOAD",
			ErrorMessage: err.Error(),
		})
		return
	}
	if payload.Metadata == nil {
		payload.Metadata = map[string]interface{}{}
	}
	payload.Metadata["cloud_msg_id"] = envelope.MsgID
	replies, err := r.gatewayAdapter.Chat(ctx, payload)
	if err != nil {
		r.reply(ctx, envelope.MsgID, protocol.TypeChatReply, protocol.ChatReplyPayload{
			RequestMsgID: envelope.MsgID,
			SessionID:    payload.SessionID,
			Role:         "assistant",
			ChunkSeq:     1,
			IsFinal:      true,
			IsEnd:        true,
			FinishReason: "error",
			ErrorCode:    "CHAT_EXECUTION_FAILED",
			ErrorMessage: err.Error(),
		})
		return
	}
	for _, reply := range replies {
		reply.RequestMsgID = envelope.MsgID
		reply.SessionID = payload.SessionID
		if reply.Role == "" {
			reply.Role = "assistant"
		}
		r.reply(ctx, envelope.MsgID, protocol.TypeChatReply, reply)
	}
}

func (r *MessageRouter) reply(ctx context.Context, msgID, msgType string, payload interface{}) {
	if err := r.sender.SendEnvelope(ctx, msgID, msgType, payload); err != nil {
		r.logger.Printf("send reply failed type=%s msg_id=%s error=%v", msgType, msgID, err)
	}
}

func NowMillis() int64 {
	return time.Now().UnixMilli()
}
