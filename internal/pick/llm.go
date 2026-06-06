package pick

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ClaudeRunner shells out to the `claude` CLI. If claude is not on PATH or
// the call fails, Run returns an error and Pick will fall back deterministically.
type ClaudeRunner struct {
	// HasAPIKey, when true, causes the runner to pass --bare to claude.
	HasAPIKey bool
	// Timeout for the claude call (default 30s).
	Timeout time.Duration
}

func (c ClaudeRunner) Run(ctx context.Context, in Inputs) (string, error) {
	if _, err := exec.LookPath("claude"); err != nil {
		return "", fmt.Errorf("claude not on PATH: %w", err)
	}
	prompt := fmt.Sprintf(
		"Choose the maximum number of agentic iterations needed for this task. "+
			"Reply with ONLY a single integer between %d and %d, no explanation, no markdown.\n\n"+
			"TASK:\n%s\n\nACCEPTANCE CRITERIA:\n%s",
		in.Floor, in.Ceiling, in.Task, in.Acceptance,
	)

	timeout := c.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	subCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{}
	if c.HasAPIKey {
		args = append(args, "--bare")
	}
	args = append(args, "--print", "--model", in.Model, prompt)

	cmd := exec.CommandContext(subCtx, "claude", args...)
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}
