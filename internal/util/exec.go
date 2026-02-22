package util

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

type CmdResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func RunCmd(ctx context.Context, dir string, env []string, name string, args ...string) (CmdResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := CmdResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
	}

	if err == nil {
		return result, nil
	}

	if exitErr, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}

	return result, fmt.Errorf("run %s: %w", name, err)
}
