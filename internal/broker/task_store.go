package broker

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type storedTask struct {
	Version int  `json:"version"`
	Task    Task `json:"task"`
}

// EnablePersistence must be called before serving. Tasks interrupted by the
// previous broker become explicit failures; no model turn is automatically replayed.
func (t *Table) EnablePersistence(dir string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.store != nil {
		return fmt.Errorf("task persistence already enabled")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	t.store = root
	entriesDir, err := root.Open(".")
	if err != nil {
		root.Close()
		t.store = nil
		return err
	}
	entries, err := entriesDir.ReadDir(-1)
	entriesDir.Close()
	if err != nil {
		root.Close()
		t.store = nil
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		task, err := t.readStoredLocked(id)
		if err != nil {
			root.Close()
			t.store = nil
			return fmt.Errorf("load durable task %s: %w", id, err)
		}
		rec := &taskRecord{task: task, totalEv: task.EventCount}
		if !task.State.IsTerminal() {
			rec.task.State = StateErrored
			rec.task.ExitCode = 125
			rec.task.ErrorMessage = "broker restarted before task completion; outcome unknown; inspect stdout.log before retrying"
			rec.task.FinishedAt = t.nowUTC()
			if err := t.persistLocked(rec); err != nil {
				root.Close()
				t.store = nil
				return err
			}
		}
		t.tasks[id] = rec
		t.order = append(t.order, id)
		t.evictTerminalLocked()
	}
	return nil
}

func (t *Table) ClosePersistence() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.store == nil {
		return nil
	}
	err := t.store.Close()
	t.store = nil
	return err
}

func validTaskID(id string) bool {
	if !strings.HasPrefix(id, "t_") && !strings.HasPrefix(id, "task-") {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return len(id) > 2 && len(id) <= 64
}

func (t *Table) readStoredLocked(id string) (Task, error) {
	if t.store == nil || !validTaskID(id) {
		return Task{}, ErrTaskNotFound
	}
	data, err := t.store.ReadFile(id + ".json")
	if os.IsNotExist(err) {
		return Task{}, ErrTaskNotFound
	}
	if err != nil {
		return Task{}, err
	}
	var stored storedTask
	if err := json.Unmarshal(data, &stored); err != nil {
		return Task{}, err
	}
	if stored.Version != 1 || stored.Task.ID != id || (stored.Task.State != StateQueued && stored.Task.State != StateRunning && !stored.Task.State.IsTerminal()) {
		return Task{}, fmt.Errorf("invalid persisted task %s", id)
	}
	return stored.Task, nil
}

// persistLocked writes before acknowledging admission or a transition. The file
// is synced before an atomic rename. This does not promise power-loss durability
// on filesystems that require directory syncing beyond the platform API contract.
func (t *Table) persistLocked(rec *taskRecord) error {
	if t.store == nil {
		return nil
	}
	task := rec.task
	task.EventCount = rec.totalEv
	task.Params.Prompt = "" // Prompts remain in run artifacts, not the status archive.
	data, err := json.Marshal(storedTask{Version: 1, Task: task})
	if err == nil {
		name := newTaskID() + ".tmp"
		var file *os.File
		file, err = t.store.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			defer t.store.Remove(name)
			_, err = file.Write(data)
			if err == nil {
				err = file.Sync()
			}
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
			if err == nil {
				err = t.store.Rename(name, task.ID+".json")
			}
		}
	}
	if err != nil {
		rec.persistenceFailed = true
		t.storeErr = fmt.Errorf("task storage failed; repair storage and restart broker: %w", err)
		if rec.task.State != StateCancelled {
			rec.task.State = StateErrored
		}
		rec.task.ExitCode = 1
		rec.task.FinishedAt = t.nowUTC()
		rec.task.ErrorMessage = "task status persistence failed: " + err.Error()
		return fmt.Errorf("persist task status: %w", err)
	}
	return nil
}
