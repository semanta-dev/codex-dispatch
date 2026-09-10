//go:build darwin

// Fixed native feasibility controls. This is not a production sandbox backend.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func write(path, value string) {
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		panic(err)
	}
}

func child(args []string) int {
	switch args[0] {
	case "--boundary":
		checks := map[string]bool{}
		data, err := os.ReadFile("input")
		checks["positive_read"] = err == nil && string(data) == "expected"
		checks["positive_write"] = os.WriteFile("output", []byte("allowed"), 0600) == nil
		_, err = os.ReadFile(args[1])
		checks["host_secret_denied"] = err != nil
		rootFD, rootErr := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY, 0)
		checks["root_directory_open"] = rootErr == nil
		checks["root_openat_host_secret_denied"] = false
		if rootErr == nil {
			secretFD, openErr := unix.Openat(rootFD, strings.TrimPrefix(args[1], "/"), unix.O_RDONLY, 0)
			checks["root_openat_host_secret_denied"] = openErr != nil
			if secretFD >= 0 {
				_ = unix.Close(secretFD)
			}
			_ = unix.Close(rootFD)
		}
		checks["symlink_positive"] = os.Symlink(args[1], "outside-link") == nil
		_, err = os.ReadFile("outside-link")
		checks["symlink_host_secret_denied"] = err != nil
		checks["host_write_denied"] = os.WriteFile(args[1]+"-write", []byte("escape"), 0600) != nil
		connection, err := net.DialTimeout("tcp", args[2], 300*time.Millisecond)
		checks["host_network_denied"] = err != nil
		if connection != nil {
			_ = connection.Close()
		}
		checks["environment_clean"] = os.Getenv("FAKE_REVIEW_KEY") == ""
		output, err := exec.Command("/bin/sh", "-c", "cat input").CombinedOutput()
		checks["shell_positive"] = err == nil && string(output) == "expected"
		_ = json.NewEncoder(os.Stdout).Encode(checks)
		for _, pass := range checks {
			if !pass {
				return 1
			}
		}
		return 0
	case "--fork":
		executable, _ := os.Executable()
		command := exec.Command(executable, "--delayed")
		command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := command.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() { _ = command.Wait() }()
		time.Sleep(3 * time.Second)
		return 0
	case "--delayed":
		write("ready", strconv.Itoa(os.Getpid()))
		time.Sleep(time.Second)
		write("late-write", "descendant survived process-group cancellation")
		return 0
	}
	return 2
}

func run(out string) (err error) {
	root, err := os.MkdirTemp("", "codex-macos-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	work := filepath.Join(root, "workspace")
	if err = os.Mkdir(work, 0700); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		return err
	}
	worker := filepath.Join(work, "probe")
	if err = os.WriteFile(worker, binary, 0700); err != nil {
		return err
	}
	write(filepath.Join(work, "input"), "expected")
	secret := filepath.Join(root, "fake-host-secret")
	write(secret, "FAKE_SECRET")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		return err
	}
	_ = connection.Close()
	profile := `(version 1)
(deny default)
(allow process-exec process-fork sysctl-read file-read-metadata)
; dyld/libignition opens the root directory during startup. This allows only
; immediate root enumeration, never recursive reads of otherwise denied files.
(allow file-read-data (literal "/"))
(allow file-read* (subpath "/System") (subpath "/usr") (subpath "/bin") (subpath "/Library/Apple/System") (subpath "/private/var/db/dyld") (literal "/dev/null") (literal "/dev/random") (literal "/dev/urandom"))
(allow file-write-data (literal "/dev/null"))
(allow file-read* file-write* (subpath ` + strconv.Quote(work) + `))
`
	profilePath := filepath.Join(root, "sandbox.sb")
	write(profilePath, profile)
	report := map[string]any{"kind": "feasibility-only", "goarch": runtime.GOARCH, "profile": profile, "host_listener_reachable": true}
	defer func() {
		if err != nil {
			report["error"] = err.Error()
		}
		data, _ := json.MarshalIndent(report, "", "  ")
		if saveErr := os.WriteFile(out, append(data, '\n'), 0600); saveErr != nil && err == nil {
			err = saveErr
		}
	}()
	if err = os.Setenv("FAKE_REVIEW_KEY", "FAKE_PARENT_ONLY"); err != nil {
		return err
	}
	report["parent_fake_environment_present"] = os.Getenv("FAKE_REVIEW_KEY") == "FAKE_PARENT_ONLY"
	command := func(ctx context.Context, target string, arguments ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", append([]string{"-f", profilePath, target}, arguments...)...)
		cmd.Dir = work
		cmd.Env = []string{"HOME=" + work, "TMPDIR=" + work, "PATH=/usr/bin:/bin", "LANG=C"}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return cmd
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	startup := map[string]any{}
	for _, target := range []string{"/usr/bin/true", "/bin/sh"} {
		arguments := []string{}
		if target == "/bin/sh" {
			arguments = []string{"-c", "printf shell-started"}
		}
		data, startErr := command(ctx, target, arguments...).CombinedOutput()
		result := map[string]any{"stdout_stderr": string(data), "pass": startErr == nil}
		if startErr != nil {
			result["error"] = startErr.Error()
		}
		startup[target] = result
	}
	report["runtime_startup"] = startup
	output, boundaryErr := command(ctx, worker, "--boundary", secret, listener.Addr().String()).CombinedOutput()
	report["boundary_output"], report["boundary_pass"] = string(output), boundaryErr == nil
	if boundaryErr != nil {
		return fmt.Errorf("native boundary positive/negative controls: %w", boundaryErr)
	}
	owner := command(ctx, worker, "--fork")
	var ownerErrors bytes.Buffer
	owner.Stderr = &ownerErrors
	if err = owner.Start(); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		if data, readErr := os.ReadFile(filepath.Join(work, "ready")); readErr == nil {
			report["detached_pid"] = string(data)
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, beforeErr := os.Stat(filepath.Join(work, "late-write"))
	report["sentinel_absent_before_cancel"] = os.IsNotExist(beforeErr)
	killErr := syscall.Kill(-owner.Process.Pid, syscall.SIGKILL)
	report["group_kill_succeeded"] = killErr == nil
	if killErr != nil {
		report["group_kill_error"] = killErr.Error()
	}
	if waitErr := owner.Wait(); waitErr != nil {
		report["owner_exit"] = waitErr.Error()
	}
	report["owner_stderr"] = ownerErrors.String()
	// Fixed descendants exit themselves within one second; never leave a daemon.
	time.Sleep(1500 * time.Millisecond)
	_, sentinelErr := os.Stat(filepath.Join(work, "late-write"))
	report["detached_ready"], report["late_write_after_group_kill"] = ready, sentinelErr == nil
	if killErr != nil || !os.IsNotExist(beforeErr) || (sentinelErr != nil && !os.IsNotExist(sentinelErr)) {
		return fmt.Errorf("cancellation or sentinel control failed; lifecycle result inconclusive")
	}
	if !ready {
		return fmt.Errorf("detached positive control did not start; lifecycle result inconclusive")
	}
	if sentinelErr == nil {
		return fmt.Errorf("process-group cancellation does not own detached descendants")
	}
	return fmt.Errorf("no survival observed in bounded window; reliable ownership remains unproven")
}

func main() {
	if len(os.Args) > 1 && os.Args[1] != "--out" {
		os.Exit(child(os.Args[1:]))
	}
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: macos-sandbox-probe --out evidence.json")
		os.Exit(2)
	}
	if err := run(os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
