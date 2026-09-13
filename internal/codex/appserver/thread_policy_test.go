package appserver

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

func TestFreshAndResumeBindAndVerifyExecutionSettings(t *testing.T) {
	for _, resume := range []bool{false, true} {
		for _, badField := range []string{"", "cwd", "model", "sandbox", "approvalPolicy", "missing"} {
			name := "fresh/" + badField
			if resume {
				name = "resume/" + badField
			}
			t.Run(name, func(t *testing.T) {
				a, srv := newPipedAppServer(t)
				srv.handle("initialize", func(json.RawMessage) (interface{}, *rpcError) { return map[string]string{}, nil })
				cwd := t.TempDir()
				method := "thread/start"
				if resume {
					method = "thread/resume"
				}
				srv.handle(method, func(raw json.RawMessage) (interface{}, *rpcError) {
					var p map[string]any
					if err := json.Unmarshal(raw, &p); err != nil {
						t.Error(err)
					}
					for k, want := range map[string]string{"cwd": cwd, "model": "pinned-model", "sandbox": "read-only", "approvalPolicy": "never"} {
						if p[k] != want {
							t.Errorf("%s = %v, want %s", k, p[k], want)
						}
					}
					if resume && p["threadId"] != "existing" {
						t.Error("resume lost thread ID")
					}
					response := map[string]any{"thread": map[string]string{"id": "existing", "cwd": cwd}, "cwd": cwd, "model": "pinned-model", "sandbox": map[string]string{"type": "readOnly"}, "approvalPolicy": "never"}
					switch badField {
					case "":
					case "missing":
						delete(response, "sandbox")
					case "sandbox":
						response["sandbox"] = map[string]string{"type": "dangerFullAccess"}
					default:
						response[badField] = "incorrect"
					}
					return response, nil
				})
				ctx := context.Background()
				if err := a.initialize(ctx); err != nil {
					t.Fatal(err)
				}
				opts := ThreadStartOptions{CWD: cwd, Model: "pinned-model", Sandbox: "read-only", VerifySettings: true}
				var err error
				if resume {
					_, err = a.ResumeThread(ctx, "existing", opts)
				} else {
					_, err = a.StartThread(ctx, opts)
				}
				if (err != nil) != (badField != "") {
					t.Fatalf("settings mismatch=%s, err=%v", badField, err)
				}
			})
		}
	}
}

func TestFreshAndResumeRejectExpandedWorkspacePermissions(t *testing.T) {
	for _, resume := range []bool{false, true} {
		for _, mutation := range []string{"valid", "network", "extra root", "relative root", "tmpdir", "slash tmp", "missing temp policy", "wrong network type"} {
			t.Run(fmt.Sprintf("resume=%t/%s", resume, mutation), func(t *testing.T) {
				a, srv := newPipedAppServer(t)
				srv.handle("initialize", func(json.RawMessage) (interface{}, *rpcError) { return map[string]string{}, nil })
				cwd := t.TempDir()
				method := "thread/start"
				if resume {
					method = "thread/resume"
				}
				srv.handle(method, func(raw json.RawMessage) (interface{}, *rpcError) {
					var request struct {
						Config map[string]json.RawMessage `json:"config"`
					}
					if err := json.Unmarshal(raw, &request); err != nil {
						t.Error(err)
					}
					for key, want := range confinedWorkspaceConfig() {
						encoded, _ := json.Marshal(want)
						if string(request.Config[key]) != string(encoded) {
							t.Errorf("%s = %s, want %s", key, request.Config[key], encoded)
						}
					}
					policy := map[string]any{"type": "workspaceWrite", "writableRoots": []string{}, "networkAccess": false, "excludeTmpdirEnvVar": true, "excludeSlashTmp": true}
					switch mutation {
					case "network":
						policy["networkAccess"] = true
					case "extra root":
						policy["writableRoots"] = []string{filepath.Dir(cwd)}
					case "relative root":
						policy["writableRoots"] = []string{"."}
					case "tmpdir":
						policy["excludeTmpdirEnvVar"] = false
					case "slash tmp":
						policy["excludeSlashTmp"] = false
					case "missing temp policy":
						delete(policy, "excludeTmpdirEnvVar")
					case "wrong network type":
						policy["networkAccess"] = "false"
					}
					return map[string]any{"thread": map[string]string{"id": "thread"}, "cwd": cwd, "model": "pinned-model", "approvalPolicy": "never", "sandbox": policy}, nil
				})
				if err := a.initialize(context.Background()); err != nil {
					t.Fatal(err)
				}
				opts := ThreadStartOptions{VerifySettings: true, CWD: cwd, Sandbox: "workspace-write", Model: "pinned-model"}
				var err error
				if resume {
					_, err = a.ResumeThread(context.Background(), "thread", opts)
				} else {
					_, err = a.StartThread(context.Background(), opts)
				}
				if (err != nil) != (mutation != "valid") {
					t.Fatalf("mutation %s: %v", mutation, err)
				}
			})
		}
	}
}

func TestReadOnlyRejectsNetworkAndWritableRoots(t *testing.T) {
	cwd := t.TempDir()
	for _, policy := range []map[string]any{{"type": "readOnly", "networkAccess": true}, {"type": "readOnly", "writableRoots": []string{cwd}}} {
		raw, _ := json.Marshal(map[string]any{"cwd": cwd, "approvalPolicy": "never", "sandbox": policy})
		if err := verifyThreadSettings(raw, ThreadStartOptions{CWD: cwd, Sandbox: "read-only"}); err == nil {
			t.Fatalf("accepted expanded policy: %v", policy)
		}
	}
}

func TestInvalidResumeParametersDoNotSilentlyStartFresh(t *testing.T) {
	for _, message := range []string{"invalid sandbox override", "model not found", "cwd not found"} {
		if isStaleError(&rpcError{Code: -32602, Message: message}) {
			t.Fatalf("invalid settings treated as missing session: %s", message)
		}
	}
	if !isStaleError(&rpcError{Code: -32600, Message: "no rollout found for thread id missing"}) {
		t.Fatal("observed missing-rollout response not recognized")
	}
}
