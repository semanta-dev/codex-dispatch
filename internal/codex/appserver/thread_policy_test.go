package appserver

import (
	"context"
	"encoding/json"
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
