package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ExecutionPolicy is immutable broker configuration, never supplied by an RPC.
// Authenticated same-user clients may opt into danger-full-access explicitly;
// this guard validates dispatch destinations, not subsequent model tool calls.
type ExecutionPolicy struct {
	Root        string
	ResultRoots []string
}

type executionPolicyKey struct{}

// Bound expensive Git preflights independently of queued/running task slots.
var policyPreflights = make(chan struct{}, 16)

func canonicalDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("absolute directory required")
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("directory required")
	}
	return p, nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (p ExecutionPolicy) workRoots(ctx context.Context) ([]string, error) {
	root, err := canonicalDirectory(p.Root)
	if err != nil {
		return nil, err
	}
	roots := []string{root}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "worktree", "list", "--porcelain", "-z")
	for _, v := range os.Environ() {
		// Repository selection is server-owned, regardless of the launcher's
		// git plumbing environment (e.g. a temporary index/worktree override).
		if !strings.HasPrefix(v, "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list registered worktrees: %w", err)
	}
	for _, field := range strings.Split(string(out), "\x00") {
		if path, ok := strings.CutPrefix(field, "worktree "); ok {
			if path, err := canonicalDirectory(path); err == nil {
				roots = append(roots, path)
			}
		}
	}
	return roots, nil
}

// Wrap guards both production execution RPCs before task creation and log open.
func (p ExecutionPolicy) Wrap(next Handler) Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req DispatchRunParams
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, &RPCError{Code: -32602, Message: "invalid dispatch params"}
		}
		select {
		case policyPreflights <- struct{}{}:
		default:
			return nil, &RPCError{Code: -32008, Message: "broker policy checks busy; retry later"}
		}
		err := p.validate(ctx, &req)
		<-policyPreflights
		if err != nil {
			return nil, &RPCError{Code: -32602, Message: "dispatch policy: " + err.Error()}
		}
		validated, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		return next(context.WithValue(ctx, executionPolicyKey{}, &p), validated)
	}
}

func (p ExecutionPolicy) validate(ctx context.Context, req *DispatchRunParams) error {
	if req.Mode != "fresh" && req.Mode != "resume" {
		return fmt.Errorf("mode must be fresh or resume")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return fmt.Errorf("prompt required")
	}
	if req.Mode == "resume" && req.PrevSessionID == "" {
		return fmt.Errorf("resume requires a previous session")
	}
	if req.Sandbox == "" {
		req.Sandbox = "workspace-write"
	}
	switch req.Sandbox {
	case "read-only", "workspace-write", "danger-full-access":
	default:
		return fmt.Errorf("invalid sandbox")
	}
	roots, err := p.workRoots(ctx)
	if err != nil {
		return err
	}
	if req.CWD == "" {
		req.CWD = p.Root
	}
	cwd, err := canonicalDirectory(req.CWD)
	if err != nil {
		return fmt.Errorf("invalid cwd: %w", err)
	}
	allowed := false
	for _, root := range roots {
		allowed = allowed || within(root, cwd)
	}
	if !allowed {
		return fmt.Errorf("cwd must be inside the broker repository or a registered worktree")
	}
	req.CWD = cwd
	if !filepath.IsAbs(req.LogPath) || filepath.Base(req.LogPath) != "stdout.log" {
		return fmt.Errorf("log_path must be an absolute stdout.log path")
	}
	if req.ResultDir == "" {
		req.ResultDir = filepath.Dir(req.LogPath)
	}
	result, err := canonicalDirectory(req.ResultDir)
	if err != nil {
		return fmt.Errorf("invalid result directory: %w", err)
	}
	logDir, err := canonicalDirectory(filepath.Dir(req.LogPath))
	if err != nil || logDir != result {
		return fmt.Errorf("log_path must be inside result_dir")
	}
	allowed = false
	for _, root := range append(roots, p.ResultRoots...) {
		if root, err := canonicalDirectory(root); err == nil {
			allowed = allowed || within(root, result)
		}
	}
	if !allowed {
		return fmt.Errorf("result_dir is outside configured result roots; configure CODEX_BROKER_RESULT_ROOTS and restart the broker")
	}
	req.ResultDir = result
	req.LogPath = filepath.Join(result, "stdout.log")
	if fi, err := os.Lstat(req.LogPath); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("log_path must be a regular file, not a symlink or device")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// openLog replaces the artifact with a newly owned file through a confined
// directory handle. It never appends through an existing symlink or hard link,
// and moving/replacing a parent after opening cannot redirect the descriptor.
func (p ExecutionPolicy) openLog(ctx context.Context, req *DispatchRunParams) (*LogWriter, error) {
	if err := p.validate(ctx, req); err != nil {
		return nil, err
	}
	roots, err := p.workRoots(ctx)
	if err != nil {
		return nil, err
	}
	for _, rootPath := range append(roots, p.ResultRoots...) {
		rootPath, err = canonicalDirectory(rootPath)
		if err != nil || !within(rootPath, req.ResultDir) {
			continue
		}
		root, err := os.OpenRoot(rootPath)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(rootPath, req.ResultDir)
		if err != nil {
			root.Close()
			return nil, err
		}
		dir, err := root.OpenRoot(rel)
		root.Close()
		if err != nil {
			return nil, err
		}
		defer dir.Close()
		// Remove only the directory entry, never truncate its target. A new
		// entry appearing before exclusive creation causes a safe failure.
		if err = dir.Remove("stdout.log"); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		f, err := dir.OpenFile("stdout.log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		return &LogWriter{file: f}, nil
	}
	return nil, fmt.Errorf("no permitted log root")
}

func openTaskLog(ctx context.Context, p *DispatchRunParams) (*LogWriter, error) {
	if p.policy != nil {
		return p.policy.openLog(ctx, p)
	}
	return OpenLogWriter(p.LogPath)
}
