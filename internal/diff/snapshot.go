package diff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/semanta-dev/codex-dispatch/internal/artifact"
	"github.com/semanta-dev/codex-dispatch/internal/authority"
	"github.com/semanta-dev/codex-dispatch/internal/result"
)

type snapshot struct {
	Version      int    `json:"version"`
	Head         string `json:"head"`
	Tree         string `json:"tree"`
	RunID        string `json:"run_id"`
	AuthorityDir string `json:"authority_dir,omitempty"`
	ObjectDir    string `json:"object_dir,omitempty"`
}

// snapshotTree stages only into a temporary index. Both trees exclude runtime
// artifacts; untracked non-ignored files, deletions, symlinks and modes survive.
func snapshotTree(repo, resultDir, objectDir string) (string, error) {
	if objectDir == "" {
		return "", fmt.Errorf("private snapshot object directory required")
	}
	scratch, err := os.MkdirTemp(filepath.Dir(objectDir), "capture-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch)
	index, cleanup, err := setupTempIndex(repo, scratch)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if _, err := runGitIndex(repo, index, "-c", "core.splitIndex=false", "update-index", "--no-split-index"); err != nil {
		return "", err
	}
	rel, err := relIfUnder(repo, resultDir)
	if err != nil {
		return "", err
	}
	excluded := []string{".codex-dispatch"}
	if rel != "" {
		excluded = append(excluded, rel)
	}
	listed, err := runGitIndexObjects(repo, index, objectDir, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	var paths []string
	for _, p := range dedup(splitZ(listed)) {
		skip := false
		for _, x := range excluded {
			if p == x || strings.HasPrefix(p, x+"/") {
				skip = true
				break
			}
		}
		if !skip {
			paths = append(paths, p)
		}
	}
	// Build a raw-content tree: git add would run clean filters and lose bytes.
	if _, err := runGitIndexObjects(repo, index, objectDir, "read-tree", "--empty"); err != nil {
		return "", err
	}
	if len(paths) > 100000 {
		return "", fmt.Errorf("snapshot file count exceeds quota")
	}
	source, err := os.OpenRoot(repo)
	if err != nil {
		return "", err
	}
	defer source.Close()
	var totalBytes int64
	var entries, batch strings.Builder
	var regularPaths, regularModes []string
	for _, p := range paths {
		info, err := source.Lstat(filepath.FromSlash(p))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		mode := "100644"
		var hash string
		switch {
		case info.Mode().IsRegular():
			if info.Mode().Perm()&0111 != 0 {
				mode = "100755"
			}
			raw, err := artifact.ReadRegularAt(source, filepath.FromSlash(p), 8*1024*1024)
			if err != nil {
				return "", fmt.Errorf("snapshot %q: %w", p, err)
			}
			totalBytes += int64(len(raw))
			if totalBytes > 512*1024*1024 {
				return "", fmt.Errorf("snapshot bytes exceed quota")
			}
			privatePath := filepath.Join(scratch, fmt.Sprintf("blob-%d", len(regularPaths)))
			if err := os.WriteFile(privatePath, raw, 0600); err != nil {
				return "", err
			}
			regularPaths = append(regularPaths, p)
			regularModes = append(regularModes, mode)
			batch.WriteString(quoteGitPath(privatePath))
			batch.WriteByte('\n')
			continue
		case info.Mode()&os.ModeSymlink != 0:
			mode = "120000"
			var target string
			target, err = source.Readlink(filepath.FromSlash(p))
			if err == nil {
				hash, err = gitInputObjects(repo, "", objectDir, target, "hash-object", "--no-filters", "-w", "--stdin")
			}
		default:
			return "", fmt.Errorf("cannot preserve special file or submodule %q", p)
		}
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&entries, "%s %s\t%s\x00", mode, strings.TrimSpace(hash), p)
	}
	if len(regularPaths) > 0 {
		out, err := gitInputObjects(repo, "", objectDir, batch.String(), "hash-object", "--no-filters", "-w", "--stdin-paths")
		if err != nil {
			return "", err
		}
		hashes := strings.Fields(out)
		if len(hashes) != len(regularPaths) {
			return "", fmt.Errorf("incomplete snapshot hash output")
		}
		for i, p := range regularPaths {
			if !objectID(hashes[i]) {
				return "", fmt.Errorf("invalid snapshot blob hash")
			}
			fmt.Fprintf(&entries, "%s %s\t%s\x00", regularModes[i], hashes[i], p)
		}
	}
	if _, err := gitInputObjects(repo, index, objectDir, entries.String(), "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	tree, err := runGitIndexObjects(repo, index, objectDir, "write-tree")
	return strings.TrimSpace(tree), err
}

func readSnapshotExport(resultDir string) ([]byte, error) {
	root, err := os.OpenRoot(resultDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return artifact.ReadRegularAt(root, "baseline-snapshot.json", 16*1024)
}

// Baseline carries identity retained by dispatch across the untrusted turn.
type Baseline struct {
	manifest snapshot
	binding  authority.Binding
}

func (b *Baseline) Capture(repo, head, resultDir string) (Stats, error) {
	raw, err := readSnapshotExport(resultDir)
	if err != nil {
		return Stats{}, err
	}
	var exported snapshot
	if err := json.Unmarshal(raw, &exported); err != nil || exported != b.manifest {
		return Stats{}, fmt.Errorf("baseline export changed during dispatch")
	}
	root, err := gitTopLevel(repo)
	if err != nil {
		return Stats{}, err
	}
	current, err := authority.Read(b.manifest.AuthorityDir)
	if err != nil || current != b.binding {
		return Stats{}, fmt.Errorf("baseline authority changed during dispatch")
	}
	return captureSnapshot(root, head, resultDir, b.manifest)
}

// PreexistingPatch compares two Git trees, never the live working tree. Live
// git diff would invoke repository clean filters while reading working files.
func (b *Baseline) PreexistingPatch() ([]byte, error) {
	repo := b.binding.Repository
	original, err := runGit(repo, "rev-parse", "--git-path", "objects")
	if err != nil {
		return nil, err
	}
	original = strings.TrimSpace(original)
	if !filepath.IsAbs(original) {
		original = filepath.Join(repo, original)
	}
	cmd := gitCommand(repo, "", b.manifest.ObjectDir, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", b.binding.Head, b.binding.BaselineTree)
	// Read the preexisting HEAD closure only for this read-only comparison. Raw
	// snapshot creation still has no alternates and writes no repository objects.
	cmd.Env = append(cmd.Env, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+quoteGitPath(original))
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	if len(output) > 8*1024*1024 {
		return nil, fmt.Errorf("baseline patch exceeds quota")
	}
	return output, nil
}

// SealResult records controller-produced result bytes separately from baseline
// and diff capture. It never reads a model-written public result as authority.
func (b *Baseline) SealResult(res result.Result) error {
	current, err := authority.Read(b.manifest.AuthorityDir)
	if err != nil || current != b.binding {
		return fmt.Errorf("baseline authority changed before terminal seal")
	}
	if (res.ExitCode == 0 || res.ExitCode == 4) && res.DiffPath == "" {
		return fmt.Errorf("completed result requires captured diff")
	}
	patchDigest := ""
	if res.DiffPath != "" {
		capture, err := authority.ReadCapture(b.manifest.AuthorityDir)
		if err != nil {
			return err
		}
		patchDigest = capture.PatchDigest
	}
	encoded, err := result.Encode(res)
	if err != nil {
		return err
	}
	if err := authority.PublishFile(b.manifest.AuthorityDir, "result.json", encoded); err != nil {
		return err
	}
	state := "FAILED"
	switch res.ExitCode {
	case 0:
		state = "SUCCEEDED"
	case 4:
		state = "NO_CHANGES"
	case 124:
		state = "CANCELED"
	}
	terminal := struct {
		Version      int               `json:"version"`
		Binding      authority.Binding `json:"baseline"`
		ResultDigest string            `json:"result_digest"`
		PatchDigest  string            `json:"patch_digest,omitempty"`
		SessionID    string            `json:"session_id"`
		ExitCode     int               `json:"exit_code"`
		State        string            `json:"state"`
	}{1, b.binding, fmt.Sprintf("%x", sha256.Sum256(encoded)), patchDigest, res.SessionID, res.ExitCode, state}
	data, err := json.Marshal(terminal)
	if err != nil {
		return err
	}
	return authority.PublishFile(b.manifest.AuthorityDir, "terminal.json", data)
}

type indexObjects struct {
	Version int      `json:"version"`
	Objects []string `json:"objects"`
}

// copyIndexObjectClosure preserves blobs and sparse trees referenced by the original index,
// including staged-only content that is absent from the working tree snapshot.
func copyIndexObjectClosure(repo, index, privateObjects, authorityDir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	return copyIndexObjectClosureContext(ctx, repo, index, privateObjects, authorityDir)
}

func copyIndexObjectClosureContext(ctx context.Context, repo, index, privateObjects, authorityDir string) (string, error) {
	read := func(args ...string) (string, error) {
		return runGitBounded(ctx, repo, index, "", "", 8*1024*1024, args...)
	}
	out, err := read("ls-files", "--sparse", "--stage", "-z")
	if err != nil {
		return "", err
	}
	records := splitZ(out)
	if len(records) > 100000 {
		return "", fmt.Errorf("index object count exceeds quota")
	}
	seen := map[string]struct{}{}
	objects := make([]string, 0)
	var total int64
	var copyObject func(string) error
	copyObject = func(oid string) error {
		if ctx.Err() != nil {
			return fmt.Errorf("index object closure exceeded capture budget: %w", ctx.Err())
		}
		if _, ok := seen[oid]; ok {
			return nil
		}
		if len(seen) >= 100000 {
			return fmt.Errorf("index object count exceeds quota")
		}
		kind, err := read("cat-file", "-t", oid)
		if err != nil {
			return err
		}
		kind = strings.TrimSpace(kind)
		if kind != "blob" && kind != "tree" {
			return fmt.Errorf("index object %s has unsupported type %s", oid, kind)
		}
		declared, err := read("cat-file", "-s", oid)
		if err != nil {
			return err
		}
		size, err := strconv.ParseInt(strings.TrimSpace(declared), 10, 64)
		if err != nil || size < 0 || size > 8*1024*1024 || total+size > 512*1024*1024 {
			return fmt.Errorf("index object %s exceeds capture limits", oid)
		}
		data, err := read("cat-file", kind, oid)
		if err != nil || int64(len(data)) != size {
			return fmt.Errorf("could not read indexed object %s", oid)
		}
		wrote, err := runGitBounded(ctx, repo, "", privateObjects, data, 1024, "hash-object", "-t", kind, "-w", "--stdin")
		if err != nil || strings.TrimSpace(wrote) != oid {
			return fmt.Errorf("could not preserve indexed object %s", oid)
		}
		// Mark before walking a tree to handle recursive/shared structures safely.
		seen[oid] = struct{}{}
		objects = append(objects, oid)
		total += size
		if kind != "tree" {
			return nil
		}
		// Traverse the exact tree bytes just hashed into private storage. The
		// source object path may change after that copy has been validated.
		entries, err := runGitBounded(ctx, authorityDir, "", privateObjects, "", 8*1024*1024, "ls-tree", "-z", oid)
		if err != nil {
			return err
		}
		for _, entry := range splitZ(entries) {
			header, _, ok := strings.Cut(entry, "\t")
			if !ok {
				return fmt.Errorf("invalid indexed tree entry")
			}
			fields := strings.Fields(header)
			if len(fields) != 3 || !objectID(fields[2]) {
				return fmt.Errorf("invalid indexed tree object")
			}
			if fields[1] != "blob" && fields[1] != "tree" {
				return fmt.Errorf("unsupported indexed tree entry %s", fields[1])
			}
			if err := copyObject(fields[2]); err != nil {
				return err
			}
		}
		return nil
	}
	for _, record := range records {
		header, _, ok := strings.Cut(record, "\t")
		if !ok {
			return "", fmt.Errorf("invalid index stage entry")
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || !objectID(fields[1]) {
			return "", fmt.Errorf("invalid index object entry")
		}
		oid := fields[1]
		if oid == strings.Repeat("0", len(oid)) {
			continue
		}
		if err := copyObject(oid); err != nil {
			return "", err
		}
	}
	sort.Strings(objects)
	manifest, err := json.Marshal(indexObjects{Version: 1, Objects: objects})
	if err != nil {
		return "", err
	}
	if err := authority.PublishFile(authorityDir, "index-objects.json", manifest); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(manifest)), nil
}

func saveSnapshot(repo, workdir, resultDir string) (*Baseline, error) {
	head, err := runGit(repo, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil, err
	}
	gitDir, err := runGit(repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	authDir, err := authority.Store("")
	if err != nil {
		return nil, err
	}
	for _, p := range []string{repo, workdir, resultDir} {
		rel, err := filepath.Rel(p, authDir)
		if err != nil {
			return nil, err
		}
		if rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("authority store must be outside repository and run paths")
		}
	}
	// Keep recovery data even on later failure. Never prune or rewrite user Git.
	indexPresent := true
	if err := copyFile(filepath.Join(strings.TrimSpace(gitDir), "index"), filepath.Join(authDir, "baseline-index")); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		indexPresent = false
		if err := artifact.WriteAtomic(authDir, "baseline-index", nil, 0600); err != nil {
			return nil, err
		}
	}
	sharedName, sharedDigest := "", ""
	if indexPresent {
		shared, err := runGitIndex(repo, filepath.Join(authDir, "baseline-index"), "rev-parse", "--shared-index-path")
		if err != nil {
			return nil, err
		}
		if shared = strings.TrimSpace(shared); shared != "" {
			sharedName = filepath.Base(shared)
			if !strings.HasPrefix(sharedName, "sharedindex.") || !objectID(strings.TrimPrefix(sharedName, "sharedindex.")) {
				return nil, fmt.Errorf("invalid shared index path")
			}
			if err := copyFile(filepath.Join(strings.TrimSpace(gitDir), sharedName), filepath.Join(authDir, sharedName)); err != nil {
				return nil, err
			}
			data, err := authority.ReadFile(authDir, sharedName, 512*1024*1024)
			if err != nil {
				return nil, err
			}
			sharedDigest = fmt.Sprintf("%x", sha256.Sum256(data))
		}
	}
	objectDir := filepath.Join(authDir, "objects")
	format, err := runGit(repo, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	if _, err := runGit(authDir, "init", "--bare", "--quiet", "--template=", "--object-format="+strings.TrimSpace(format)); err != nil {
		return nil, err
	}
	indexObjectsDigest := ""
	if indexPresent {
		indexObjectsDigest, err = copyIndexObjectClosure(repo, filepath.Join(authDir, "baseline-index"), objectDir, authDir)
		if err != nil {
			return nil, err
		}
	} else {
		manifest, err := json.Marshal(indexObjects{Version: 1, Objects: []string{}})
		if err != nil {
			return nil, err
		}
		if err := authority.PublishFile(authDir, "index-objects.json", manifest); err != nil {
			return nil, err
		}
		indexObjectsDigest = fmt.Sprintf("%x", sha256.Sum256(manifest))
	}
	tree, err := snapshotTree(repo, resultDir, objectDir)
	if err != nil {
		return nil, err
	}
	idx, err := authority.ReadFile(authDir, "baseline-index", 512*1024*1024)
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	file, err := os.Open(executable)
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	_, hashErr := io.Copy(digest, file)
	closeErr := file.Close()
	if hashErr != nil {
		return nil, hashErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	workdir, err = filepath.EvalSymlinks(workdir)
	if err != nil {
		return nil, err
	}
	resultDir, err = filepath.EvalSymlinks(resultDir)
	if err != nil {
		return nil, err
	}
	binding := authority.Binding{Version: 2, RunID: filepath.Base(authDir), Head: strings.TrimSpace(head), BaselineTree: tree, IndexDigest: fmt.Sprintf("%x", sha256.Sum256(idx)), IndexPresent: indexPresent, IndexObjectsDigest: indexObjectsDigest, SharedIndex: sharedName, SharedIndexDigest: sharedDigest, CandidateHash: hex.EncodeToString(digest.Sum(nil)), Repository: repo, Workdir: workdir, ExportDir: resultDir, TerminalState: "BASELINE_CAPTURED"}
	if err := authority.Publish(authDir, binding); err != nil {
		return nil, err
	}
	manifest := snapshot{Version: 2, Head: binding.Head, Tree: tree, RunID: binding.RunID, AuthorityDir: authDir, ObjectDir: objectDir}
	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if err := artifact.WriteAtomic(resultDir, "baseline-snapshot.json", data, 0600); err != nil {
		return nil, err
	}
	return &Baseline{manifest: manifest, binding: binding}, nil
}

func captureSnapshot(repo, head, resultDir string, s snapshot) (Stats, error) {
	if s.Version != 2 || s.Head != head || !objectID(s.Tree) || s.AuthorityDir == "" || s.ObjectDir == "" {
		return Stats{}, fmt.Errorf("invalid or mismatched baseline snapshot")
	}
	authDir, err := authority.Open(s.RunID)
	if err != nil {
		return Stats{}, err
	}
	if s.AuthorityDir != authDir || s.ObjectDir != filepath.Join(authDir, "objects") {
		return Stats{}, fmt.Errorf("baseline authority locator mismatch")
	}
	binding, err := authority.Read(authDir)
	if err != nil {
		return Stats{}, err
	}
	exportDir, err := filepath.EvalSymlinks(resultDir)
	if err != nil {
		return Stats{}, err
	}
	if binding.RunID != s.RunID || binding.Head != head || binding.BaselineTree != s.Tree || binding.Repository != repo || binding.ExportDir != exportDir || binding.TerminalState != "BASELINE_CAPTURED" {
		return Stats{}, fmt.Errorf("baseline authority binding mismatch")
	}
	index, err := authority.ReadFile(authDir, "baseline-index", 512*1024*1024)
	if err != nil || binding.IndexDigest != fmt.Sprintf("%x", sha256.Sum256(index)) {
		return Stats{}, fmt.Errorf("baseline index changed")
	}
	indexManifest, err := authority.ReadFile(authDir, "index-objects.json", 8*1024*1024)
	if err != nil || binding.IndexObjectsDigest != fmt.Sprintf("%x", sha256.Sum256(indexManifest)) {
		return Stats{}, fmt.Errorf("index object closure changed")
	}
	var closure indexObjects
	if err := json.Unmarshal(indexManifest, &closure); err != nil || closure.Version != 1 || !sort.StringsAreSorted(closure.Objects) {
		return Stats{}, fmt.Errorf("invalid index object closure")
	}
	for i, oid := range closure.Objects {
		if i > 0 && oid == closure.Objects[i-1] {
			return Stats{}, fmt.Errorf("duplicate indexed object id")
		}
		if !objectID(oid) {
			return Stats{}, fmt.Errorf("invalid indexed object id")
		}
		kind, err := runGit(s.AuthorityDir, "cat-file", "-t", oid)
		if err != nil || (strings.TrimSpace(kind) != "blob" && strings.TrimSpace(kind) != "tree") {
			return Stats{}, fmt.Errorf("private indexed object is unavailable or unsupported")
		}
	}
	if binding.SharedIndex != "" {
		data, err := authority.ReadFile(authDir, binding.SharedIndex, 512*1024*1024)
		if err != nil || binding.SharedIndexDigest != fmt.Sprintf("%x", sha256.Sum256(data)) {
			return Stats{}, fmt.Errorf("shared index changed")
		}
	}
	for _, name := range []string{"capture.json", "diff.patch"} {
		if _, err := os.Lstat(filepath.Join(authDir, name)); err == nil {
			return Stats{}, fmt.Errorf("capture already exists; start a fresh run")
		} else if !os.IsNotExist(err) {
			return Stats{}, err
		}
	}
	resolved, err := runGitIndexObjects(repo, "", s.ObjectDir, "rev-parse", "--verify", s.Tree+"^{tree}")
	if err != nil || strings.TrimSpace(resolved) != s.Tree {
		return Stats{}, fmt.Errorf("baseline snapshot is unavailable or changed")
	}
	after, err := snapshotTree(repo, resultDir, s.ObjectDir)
	if err != nil {
		return Stats{}, err
	}
	out, err := runGitIndexObjects(repo, "", s.ObjectDir, "diff", "--no-renames", "--name-only", "-z", s.Tree, after)
	if err != nil {
		return Stats{}, err
	}
	files := splitZ(out)
	stats := Stats{FilesChanged: files}
	patch, err := diffPatchBytes(repo, "", s.ObjectDir, s.Tree, files, after)
	if err != nil {
		return Stats{}, err
	}
	if err := authority.PublishFile(authDir, "diff.patch", patch); err != nil {
		return Stats{}, err
	}
	binding.TerminalState = "DIFF_CAPTURED"
	binding.PatchDigest = fmt.Sprintf("%x", sha256.Sum256(patch))
	if err := authority.PublishCapture(authDir, binding); err != nil {
		return Stats{}, err
	}
	if err := artifact.WriteAtomic(resultDir, "diff.patch", patch, 0600); err != nil {
		return Stats{}, err
	}
	stats.LinesAdded, stats.LinesRemoved, err = numstatObjects(repo, "", s.ObjectDir, s.Tree, files, after)
	if err != nil {
		return Stats{}, err
	}
	if err := writeFilesChanged(filepath.Join(resultDir, "files-changed.txt"), files); err != nil {
		return Stats{}, err
	}
	if err := writeStatsJSON(filepath.Join(resultDir, "stats.json"), stats); err != nil {
		return Stats{}, err
	}
	return stats, nil
}

func objectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// gitInput passes NUL records and symlink bytes directly, without shell parsing.
func gitInput(repo, index, input string, args ...string) (string, error) {
	return gitInputObjects(repo, index, "", input, args...)
}

func gitInputObjects(repo, index, objectDir, input string, args ...string) (string, error) {
	cmd := gitCommand(repo, index, objectDir, args...)
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}

// --stdin-paths accepts Git's C-style quoting, including octal byte escapes.
// Quote bytes rather than Unicode codepoints to preserve arbitrary Unix names.
func quoteGitPath(path string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, c := range []byte(path) {
		if c < 32 || c >= 127 || c == '\\' || c == '"' {
			fmt.Fprintf(&out, "\\%03o", c)
		} else {
			out.WriteByte(c)
		}
	}
	out.WriteByte('"')
	return out.String()
}
