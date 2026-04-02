package runner

import (
	"context"
	"testing"

	"openclaw/dameon/internal/config"
	"openclaw/dameon/internal/protocol"
)

func TestRemoteCommandRejectsNonWhitelistedCommand(t *testing.T) {
	runner := NewRemoteCommandRunner(config.RemoteCmdConfig{
		DefaultTimeoutSec: 1,
		MaxTimeoutSec:     5,
		MaxOutputBytes:    1024,
		Whitelist:         []string{"bash"},
	}, NewExecRunner())

	result := runner.Execute(context.Background(), protocol.RemoteCmdPayload{
		CommandID: "cmd-1",
		Command:   "python3",
	})

	if result.Status != "rejected" {
		t.Fatalf("expected rejected, got %+v", result)
	}
	if result.ErrorCode != "COMMAND_REJECTED" {
		t.Fatalf("expected COMMAND_REJECTED, got %+v", result)
	}
}
