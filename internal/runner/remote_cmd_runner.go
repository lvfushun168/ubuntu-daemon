package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"openclaw/dameon/internal/config"
	"openclaw/dameon/internal/protocol"
)

type RemoteCommandRunner struct {
	cfg      config.RemoteCmdConfig
	executor *ExecRunner
}

func NewRemoteCommandRunner(cfg config.RemoteCmdConfig, executor *ExecRunner) *RemoteCommandRunner {
	return &RemoteCommandRunner{cfg: cfg, executor: executor}
}

func (r *RemoteCommandRunner) Execute(ctx context.Context, payload protocol.RemoteCmdPayload) protocol.RemoteCmdResultPayload {
	startedAt := time.Now()
	timeout := payload.TimeoutSec
	if timeout <= 0 {
		timeout = r.cfg.DefaultTimeoutSec
	}
	if timeout > r.cfg.MaxTimeoutSec {
		timeout = r.cfg.MaxTimeoutSec
	}

	if err := r.validate(payload.Command); err != nil {
		return protocol.RemoteCmdResultPayload{
			CommandID:       payload.CommandID,
			Status:          "rejected",
			StdoutTruncated: false,
			StderrTruncated: false,
			StartedAt:       startedAt.UnixMilli(),
			FinishedAt:      time.Now().UnixMilli(),
			DurationMs:      time.Since(startedAt).Milliseconds(),
			Message:         "command rejected by whitelist",
			ErrorCode:       "COMMAND_REJECTED",
			ErrorMessage:    err.Error(),
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	stdout, stderr, exitCode, err := r.executor.Run(runCtx, payload.Command, payload.Args, payload.WorkDir)
	finishedAt := time.Now()

	stdout, stdoutTruncated := truncate(stdout, r.cfg.MaxOutputBytes)
	stderr, stderrTruncated := truncate(stderr, r.cfg.MaxOutputBytes)
	result := protocol.RemoteCmdResultPayload{
		CommandID:       payload.CommandID,
		Stdout:          stdout,
		Stderr:          stderr,
		StdoutTruncated: stdoutTruncated,
		StderrTruncated: stderrTruncated,
		StartedAt:       startedAt.UnixMilli(),
		FinishedAt:      finishedAt.UnixMilli(),
		DurationMs:      finishedAt.Sub(startedAt).Milliseconds(),
	}

	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		result.Status = "timeout"
		result.Message = "command execution timeout"
		result.ErrorCode = "COMMAND_TIMEOUT"
		result.ErrorMessage = "command execution exceeded timeout"
		return result
	}

	result.ExitCode = &exitCode
	if err != nil || exitCode != 0 {
		result.Status = "failed"
		result.Message = "command execution failed"
		result.ErrorCode = "INTERNAL_ERROR"
		if err != nil {
			result.ErrorMessage = err.Error()
		}
		return result
	}

	result.Status = "success"
	result.Message = "command executed successfully"
	return result
}

func (r *RemoteCommandRunner) validate(command string) error {
	if len(r.cfg.Whitelist) == 0 {
		return nil
	}
	base := filepath.Base(command)
	if slices.Contains(r.cfg.Whitelist, command) || slices.Contains(r.cfg.Whitelist, base) {
		return nil
	}
	return fmt.Errorf("command %q is not in whitelist", command)
}

func truncate(value string, limit int) (string, bool) {
	if limit <= 0 || len(value) <= limit {
		return value, false
	}
	return value[:limit], true
}
