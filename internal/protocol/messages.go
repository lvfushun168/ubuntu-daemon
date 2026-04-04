package protocol

import "encoding/json"

const (
	TypeAuthReq         = "auth_req"
	TypeAuthResp        = "auth_resp"
	TypePing            = "ping"
	TypePong            = "pong"
	TypeSysConfig       = "sys_config"
	TypeSysConfigAck    = "sys_config_ack"
	TypeChatMsg         = "chat_msg"
	TypeChatReply       = "chat_reply"
	TypeRemoteCmd       = "remote_cmd"
	TypeRemoteCmdResult = "remote_cmd_result"
)

type Envelope struct {
	MsgID     string          `json:"msg_id"`
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type PingMessage struct {
	Type string `json:"type"`
}

type AuthRequest struct {
	DeviceID string `json:"device_id"`
	Version  string `json:"version"`
}

type AuthResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

type SysConfigPayload struct {
	Action        string            `json:"action"`
	ConfigVersion int64             `json:"config_version"`
	Config        map[string]string `json:"config"`
}

type SysConfigAckPayload struct {
	ConfigVersion int64  `json:"config_version"`
	Status        string `json:"status"`
	Applied       bool   `json:"applied"`
	AppliedAt     int64  `json:"applied_at,omitempty"`
	DaemonVersion string `json:"daemon_version,omitempty"`
	Message       string `json:"message,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
	ErrorMessage  string `json:"error_message,omitempty"`
}

type ChatMessagePayload struct {
	SessionID string                 `json:"session_id"`
	UserID    string                 `json:"user_id,omitempty"`
	Role      string                 `json:"role"`
	Text      string                 `json:"text"`
	Stream    bool                   `json:"stream"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

type ChatReplyPayload struct {
	SessionID    string           `json:"session_id"`
	Role         string           `json:"role"`
	Text         string           `json:"text"`
	ChunkSeq     int              `json:"chunk_seq"`
	IsFinal      bool             `json:"is_final"`
	IsEnd        bool             `json:"is_end"`
	FinishReason string           `json:"finish_reason,omitempty"`
	ErrorCode    string           `json:"error_code,omitempty"`
	ErrorMessage string           `json:"error_message,omitempty"`
	Attachments  []ChatAttachment `json:"attachments,omitempty"`
}

type ChatAttachment struct {
	MediaID      string `json:"media_id,omitempty"`
	MediaType    string `json:"media_type,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
	PreviewURL   string `json:"preview_url,omitempty"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
	LocalPath    string `json:"local_path,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type RemoteCmdPayload struct {
	CommandID  string   `json:"command_id"`
	Command    string   `json:"command"`
	Args       []string `json:"args,omitempty"`
	TimeoutSec int      `json:"timeout_sec"`
	WorkDir    string   `json:"work_dir,omitempty"`
	Operator   string   `json:"operator,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

type RemoteCmdResultPayload struct {
	CommandID       string `json:"command_id"`
	Status          string `json:"status"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	Stdout          string `json:"stdout,omitempty"`
	Stderr          string `json:"stderr,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	StartedAt       int64  `json:"started_at,omitempty"`
	FinishedAt      int64  `json:"finished_at,omitempty"`
	DurationMs      int64  `json:"duration_ms,omitempty"`
	Message         string `json:"message,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
	ErrorMessage    string `json:"error_message,omitempty"`
}
