package runner

import (
	"bytes"
	"context"
	"os/exec"
)

type ExecRunner struct{}

func NewExecRunner() *ExecRunner {
	return &ExecRunner{}
}

func (r *ExecRunner) Run(ctx context.Context, command string, args []string, workDir string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workDir

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	return stdout.String(), stderr.String(), exitCode, err
}
