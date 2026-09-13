// Package authority stores baseline and capture records in an owner-controlled
// directory. Callers must also exclude this directory from child sandboxes.
package authority

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Binding struct {
	Version            int    `json:"version"`
	RunID              string `json:"run_id"`
	Head               string `json:"head"`
	BaselineTree       string `json:"baseline_tree"`
	IndexDigest        string `json:"index_digest"`
	IndexPresent       bool   `json:"index_present"`
	IndexObjectsDigest string `json:"index_objects_digest"`
	SharedIndex        string `json:"shared_index,omitempty"`
	SharedIndexDigest  string `json:"shared_index_digest,omitempty"`
	PatchDigest        string `json:"patch_digest,omitempty"`
	CandidateHash      string `json:"candidate_hash"`
	Repository         string `json:"repository"`
	Workdir            string `json:"workdir"`
	ExportDir          string `json:"export_dir"`
	TerminalState      string `json:"terminal_state"`
}

func BaseDir() (string, error) {
	home, err := accountHome()
	if err != nil {
		return "", err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex-dispatch-authority"), nil
}

// Store creates an exclusive random directory. Existing roots must already be
// private; never fix permissions on an arbitrary pre-existing directory.
func Store(root string) (string, error) {
	var err error
	accountStore := root == ""
	if root == "" {
		root, err = BaseDir()
		if err != nil {
			return "", err
		}
	}
	if err := mkdirPrivate(root); err != nil && !os.IsExist(err) {
		return "", err
	}
	pinned, err := openPrivate(root)
	if err != nil {
		return "", err
	}
	defer pinned.Close()
	if accountStore {
		if err := rejectTemporaryOverlap(root, os.TempDir()); err != nil {
			return "", err
		}
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	name := hex.EncodeToString(id[:])
	if err := pinned.Mkdir(name, 0700); err != nil {
		return "", err
	}
	return filepath.Join(root, name), nil
}

// Codex may grant writes to its temporary root more specifically than a parent
// deny rule. Refuse that overlap before publishing any private recovery data.
// This is defense in depth; the app-server still needs an enforced read deny.
//
// Both sides must be resolved in the same namespace before comparison. On macOS
// the temporary root is reached through /var -> /private/var, so resolving only
// the temporary side compares unrelated namespaces and accepts a real overlap.
func rejectTemporaryOverlap(root, temporary string) error {
	temporary, err := resolvePath(temporary)
	if err != nil {
		return fmt.Errorf("cannot validate temporary directory: %w", err)
	}
	root, err = resolvePath(root)
	if err != nil {
		return fmt.Errorf("cannot validate private authority root: %w", err)
	}
	for _, paths := range [][2]string{{root, temporary}, {temporary, root}} {
		rel, err := filepath.Rel(paths[0], paths[1])
		if err != nil { // Different Windows volumes cannot overlap.
			continue
		}
		if rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("temporary directory overlaps private authority; select a disjoint TMPDIR/TEMP")
		}
	}
	return nil
}

// resolvePath returns an absolute path with every existing symlink resolved.
// Trailing components that do not exist yet are preserved so a caller can
// validate a directory before creating it.
func resolvePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var pending []string
	current := path
	for {
		resolved, evalErr := filepath.EvalSymlinks(current)
		if evalErr == nil {
			for i := len(pending) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, pending[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(evalErr) {
			return "", evalErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("cannot resolve %s", path)
		}
		pending = append(pending, filepath.Base(current))
		current = parent
	}
}

func validHex(s string, size int) bool {
	if len(s) != size {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (b Binding) Validate() error {
	if b.Version != 2 || !validHex(b.RunID, 32) || !validHex(b.Head, 40) && !validHex(b.Head, 64) || len(b.Head) != len(b.BaselineTree) || !validHex(b.BaselineTree, 40) && !validHex(b.BaselineTree, 64) || !validHex(b.IndexDigest, 64) || !validHex(b.IndexObjectsDigest, 64) || !validHex(b.CandidateHash, 64) || !filepath.IsAbs(b.Repository) || !filepath.IsAbs(b.Workdir) || !filepath.IsAbs(b.ExportDir) {
		return fmt.Errorf("invalid authority binding")
	}
	if b.SharedIndex != "" {
		oid := strings.TrimPrefix(b.SharedIndex, "sharedindex.")
		if oid == b.SharedIndex || !validHex(oid, 40) && !validHex(oid, 64) || !validHex(b.SharedIndexDigest, 64) || !b.IndexPresent {
			return fmt.Errorf("invalid split index binding")
		}
	} else if b.SharedIndexDigest != "" {
		return fmt.Errorf("missing shared index name")
	}
	switch b.TerminalState {
	case "BASELINE_CAPTURED":
		if b.PatchDigest != "" {
			return fmt.Errorf("baseline cannot seal a task patch")
		}
	case "DIFF_CAPTURED":
		if !validHex(b.PatchDigest, 64) {
			return fmt.Errorf("missing captured patch digest")
		}
	default:
		return fmt.Errorf("invalid authority state")
	}
	return nil
}

// Open resolves an opaque ID through the controller's fixed root. Workspace
// supplied paths never select authority or Git write destinations.
func Open(id string) (string, error) {
	if !validHex(id, 32) {
		return "", fmt.Errorf("invalid authority run id")
	}
	base, err := BaseDir()
	if err != nil {
		return "", err
	}
	root, err := openPrivate(base)
	if err != nil {
		return "", err
	}
	defer root.Close()
	info, err := root.Lstat(id)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("invalid authority run directory")
	}
	child, err := root.OpenRoot(id)
	if err != nil {
		return "", err
	}
	defer child.Close()
	if err := checkPrivateRoot(child); err != nil {
		return "", err
	}
	return filepath.Join(base, id), nil
}

func openPrivate(dir string) (*os.Root, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("authority directory is not regular: %s", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	pinned, err := root.Stat(".")
	if err != nil || !os.SameFile(info, pinned) {
		root.Close()
		return nil, fmt.Errorf("authority directory changed")
	}
	if err := checkPrivateRoot(root); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

func Publish(dir string, b Binding) error        { return publish(dir, "authority.json", b) }
func PublishCapture(dir string, b Binding) error { return publish(dir, "capture.json", b) }

// A hard-link publication exposes only a complete, synced file and fails if
// the create-only identity already exists. The directory handle stays pinned.
func publish(dir, name string, b Binding) error {
	if err := b.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return PublishFile(dir, name, data)
}

// PublishFile publishes one complete create-only leaf in the private store.
func PublishFile(dir, name string, data []byte) error {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return fmt.Errorf("invalid authority filename")
	}
	root, err := openPrivate(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tmp := ".authority-" + rand.Text()
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := root.Link(tmp, name); err != nil {
		return err
	}
	return syncRoot(root)
}

func Read(dir string) (Binding, error)        { return read(dir, "authority.json") }
func ReadCapture(dir string) (Binding, error) { return read(dir, "capture.json") }
func read(dir, name string) (Binding, error) {
	data, err := ReadFile(dir, name, 8192)
	if err != nil {
		return Binding{}, err
	}
	var b Binding
	if err := json.Unmarshal(data, &b); err != nil {
		return Binding{}, err
	}
	if err := b.Validate(); err != nil {
		return Binding{}, err
	}
	return b, nil
}

func ReadFile(dir, name string, limit int64) ([]byte, error) {
	if filepath.Base(name) != name {
		return nil, fmt.Errorf("invalid authority filename")
	}
	root, err := openPrivate(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("authority input is not regular")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|nonblock, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pinned, err := f.Stat()
	if err != nil || !os.SameFile(info, pinned) || !pinned.Mode().IsRegular() {
		return nil, fmt.Errorf("authority input changed")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("authority input exceeds quota")
	}
	return data, nil
}
