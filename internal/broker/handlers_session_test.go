package broker

import (
	"context"
	"strings"
	"testing"
)

func TestSessionRegisterIdempotent(t *testing.T) {
	table := NewTable(8, 2048)
	h := HandleSessionRegister(&BrokerState{Table: table})
	for i := 0; i < 3; i++ {
		out, err := h(context.Background(), []byte(`{"session_id":"sess-a","cwd":"/repo"}`))
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if m, _ := out.(map[string]any); m["ok"] != true {
			t.Fatalf("attempt %d ok != true: %v", i, out)
		}
	}
}

func TestSessionDeregisterCancelsQueued(t *testing.T) {
	table := NewTable(2, 2048)
	a, _ := table.Start("sess-a", TaskParams{Mode: "fresh"})
	_ = table.MarkRunning(a) // a is running
	b, _ := table.Start("sess-a", TaskParams{Mode: "fresh"})
	_ = table.MarkRunning(b) // b is running, table at cap=2
	c, queued := table.Start("sess-a", TaskParams{Mode: "fresh"})
	if !queued {
		t.Fatalf("c should be queued")
	}

	h := HandleSessionDeregister(&BrokerState{Table: table})
	out, err := h(context.Background(), []byte(`{"session_id":"sess-a","cancel_queued":true}`))
	if err != nil {
		t.Fatalf("deregister: %v", err)
	}
	m := out.(map[string]any)
	ids := m["cancelled_task_ids"].([]string)
	if len(ids) != 1 || ids[0] != c {
		t.Fatalf("cancelled = %v, want [%s]", ids, c)
	}
	for _, id := range []string{a, b} {
		st, _ := table.Status(id)
		if st.State != StateRunning {
			t.Fatalf("task %s state = %s, want still running", id, st.State)
		}
	}
}

func TestSessionInvalidParams(t *testing.T) {
	h := HandleSessionRegister(&BrokerState{Table: NewTable(8, 2048)})
	_, err := h(context.Background(), []byte(`{"cwd":"/r"}`))
	if err == nil || !strings.Contains(err.Error(), "session_id") {
		t.Fatalf("err = %v, want session_id-required", err)
	}
}
