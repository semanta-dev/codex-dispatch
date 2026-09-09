package broker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func persistentTable(t *testing.T, dir string) *Table {
	t.Helper()
	table := NewTable(2, 4)
	if err := table.EnablePersistence(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = table.ClosePersistence() })
	return table
}

func TestDurableTaskSurvivesRestartAndEviction(t *testing.T) {
	dir := t.TempDir()
	table := persistentTable(t, dir)
	id, _, err := table.Admit("caller", TaskParams{Prompt: "secret prompt", LogPath: "/run/stdout.log"})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.MarkRunning(id); err != nil {
		t.Fatal(err)
	}
	if _, err := table.AppendEvent(id, "test", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := table.MarkDone(id, 64, "codex-session", true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret prompt") {
		t.Fatal("archive stores prompt")
	}
	// Simulate in-memory eviction; direct status still reads the archive.
	delete(table.tasks, id)
	status, err := table.Status(id)
	if err != nil || status.ExitCode != 64 {
		t.Fatalf("evicted status lost: %+v %v", status, err)
	}
	if err := table.ClosePersistence(); err != nil {
		t.Fatal(err)
	}
	restored := persistentTable(t, dir)
	status, err = restored.Status(id)
	if err != nil || status.State != StateDone || status.ExitCode != 64 || status.CodexSession != "codex-session" || !status.FellBackToFresh || status.EventCount != 1 {
		t.Fatalf("outcome changed after restart: %+v %v", status, err)
	}
	if _, err := restored.Events(id, 1); err == nil {
		t.Fatal("restart invented an event replay")
	}
}

func TestDurableTasksInterruptedByRestartFailExplicitly(t *testing.T) {
	dir := t.TempDir()
	table := persistentTable(t, dir)
	queued, _, err := table.Admit("", TaskParams{})
	if err != nil {
		t.Fatal(err)
	}
	running, _, err := table.Admit("", TaskParams{})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.MarkRunning(running); err != nil {
		t.Fatal(err)
	}
	if err := table.ClosePersistence(); err != nil {
		t.Fatal(err)
	}
	restored := persistentTable(t, dir)
	for _, id := range []string{queued, running} {
		status, err := restored.Status(id)
		if err != nil || status.State != StateErrored || status.ExitCode != 125 || !strings.Contains(status.ErrorMessage, "outcome unknown") {
			t.Fatalf("false completion: %+v %v", status, err)
		}
	}
	if restored.HasNonTerminal() {
		t.Fatal("restart silently requeued work")
	}
}

func TestTaskPersistenceFailureRejectsAdmissionAndCompletion(t *testing.T) {
	table := persistentTable(t, t.TempDir())
	id, _, err := table.Admit("", TaskParams{})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.MarkRunning(id); err != nil {
		t.Fatal(err)
	}
	// Close underlying handle without disabling persistence to inject I/O failure.
	if err := table.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := table.Admit("", TaskParams{}); err == nil {
		t.Fatal("acknowledged an unrecorded task")
	}
	if err := table.MarkDone(id, 0, "session", false); err == nil {
		t.Fatal("acknowledged an unrecorded completion")
	}
	status, err := table.Status(id)
	if err != nil || status.State != StateErrored || status.ExitCode == 0 || !strings.Contains(status.ErrorMessage, "persistence failed") {
		t.Fatalf("false success: %+v %v", status, err)
	}
}

func TestDurableTaskIDsCannotEscapeArchive(t *testing.T) {
	table := persistentTable(t, t.TempDir())
	for _, id := range []string{"../outside", "t_../../outside", "/absolute", `t_..\outside`} {
		if _, err := table.Status(id); err != ErrTaskNotFound {
			t.Fatalf("unsafe id %q: %v", id, err)
		}
	}
}

func TestFailedPersistenceCannotResurrectStaleArchive(t *testing.T) {
	dir := t.TempDir()
	table := persistentTable(t, dir)
	id, _, err := table.Admit("", TaskParams{})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.MarkRunning(id); err != nil {
		t.Fatal(err)
	}
	makeTaskStoreReadOnly(t, dir)
	if err := table.MarkDone(id, 0, "session", false); err == nil {
		t.Fatal("persistence failure not surfaced")
	}
	table.setClock(func() time.Time { return time.Now().Add(time.Hour) })
	table.SetEvictionPolicy(1, time.Nanosecond)
	table.mu.Lock()
	table.evictTerminalLocked()
	table.mu.Unlock()
	status, err := table.Status(id)
	if err != nil || status.State != StateErrored || !strings.Contains(status.ErrorMessage, "persistence failed") {
		t.Fatalf("stale active state resurrected: %+v %v", status, err)
	}
	if _, _, err := table.Admit("", TaskParams{}); err == nil {
		t.Fatal("storage failure did not stop admission")
	}
}
