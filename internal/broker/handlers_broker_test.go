package broker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHandlerBrokerPing(t *testing.T) {
	table := NewTable(8, 2048)
	state := &BrokerState{Table: table, StartedAt: "2026-05-12T00:00:00Z"}
	out, err := HandleBrokerPing(state)(context.Background(), nil)
	if err != nil {
		t.Fatalf("HandleBrokerPing: %v", err)
	}
	b, _ := json.Marshal(out)
	for _, want := range []string{`"version":"1.0.0"`, `"started_at":"2026-05-12T00:00:00Z"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q: %s", want, b)
		}
	}
}

func TestTableRunningCount(t *testing.T) {
	table := NewTable(8, 2048)
	if got := table.RunningCount(); got != 0 {
		t.Fatalf("RunningCount on empty table = %d, want 0", got)
	}
	r1, _ := table.Start("s", TaskParams{Mode: "fresh"})
	r2, _ := table.Start("s", TaskParams{Mode: "fresh"})
	q := mustStart(t, table, "s") // stays queued
	_ = q
	if err := table.MarkRunning(r1); err != nil {
		t.Fatalf("MarkRunning r1: %v", err)
	}
	if err := table.MarkRunning(r2); err != nil {
		t.Fatalf("MarkRunning r2: %v", err)
	}
	if got := table.RunningCount(); got != 2 {
		t.Fatalf("RunningCount with two running (one queued) = %d, want 2", got)
	}
	if err := table.MarkDone(r1, 0, "sess", false); err != nil {
		t.Fatalf("MarkDone r1: %v", err)
	}
	if got := table.RunningCount(); got != 1 {
		t.Fatalf("RunningCount after one done = %d, want 1", got)
	}
}

// mustStart starts a task and returns its id, failing the test on a queued
// flag only when the caller did not expect a queued task; here it just returns
// the id (queued status is asserted separately when relevant).
func mustStart(t *testing.T, table *Table, session string) string {
	t.Helper()
	id, _ := table.Start(session, TaskParams{Mode: "fresh"})
	return id
}

// TestHandlerBrokerPingRunningCount asserts broker.ping reports the same
// running tally as Table.RunningCount (the field the handler now delegates to).
func TestHandlerBrokerPingRunningCount(t *testing.T) {
	table := NewTable(8, 2048)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})
	if err := table.MarkRunning(id); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	state := &BrokerState{Table: table, StartedAt: "2026-06-02T00:00:00Z"}
	out, err := HandleBrokerPing(state)(context.Background(), nil)
	if err != nil {
		t.Fatalf("HandleBrokerPing: %v", err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"running_count":1`) {
		t.Fatalf("ping running_count != 1: %s", b)
	}
	if !strings.Contains(string(b), `"task_count":1`) {
		t.Fatalf("ping task_count != 1: %s", b)
	}
}

func TestHandlerBrokerShutdownRefusesWhenRunning(t *testing.T) {
	table := NewTable(8, 2048)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})
	_ = table.MarkRunning(id)
	state := &BrokerState{Table: table, Shutdown: func(bool) {}}
	_, err := HandleBrokerShutdown(state)(context.Background(), []byte(`{"force":false}`))
	if err == nil {
		t.Fatalf("shutdown should refuse with running tasks")
	}
	rpcErr := ToRPCError(err)
	if rpcErr == nil {
		t.Fatalf("err = %v, want *RPCError", err)
	}
	if rpcErr.Code != -32001 {
		t.Fatalf("code = %d, want -32001", rpcErr.Code)
	}
}

func TestHandlerBrokerShutdownForceProceedsAndSignals(t *testing.T) {
	table := NewTable(8, 2048)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})
	_ = table.MarkRunning(id)
	calledCh := make(chan bool, 1)
	state := &BrokerState{Table: table, Shutdown: func(force bool) { calledCh <- force }}
	out, err := HandleBrokerShutdown(state)(context.Background(), []byte(`{"force":true}`))
	if err != nil {
		t.Fatalf("HandleBrokerShutdown: %v", err)
	}
	// Shutdown is deferred via time.AfterFunc; wait up to 500ms for it.
	select {
	case force := <-calledCh:
		if !force {
			t.Fatalf("Shutdown invoked with force=false, want force=true")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Shutdown(force=true) not invoked within 500ms")
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"ok":true`) {
		t.Fatalf("response missing ok=true: %s", b)
	}
}
