package diff

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// initRepo builds a fresh temp repo and returns its abs path + the SHA of HEAD
// after the initial commit. README.md contains "initial\n".
func initRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@test"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	head, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return dir, string(bytesTrimRight(head, "\n"))
}

// bytesTrimRight avoids pulling in bytes just for this helper. Returns a []byte.
func bytesTrimRight(b []byte, cut string) []byte {
	for len(b) > 0 && containsByte(cut, b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}

func containsByte(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return true
		}
	}
	return false
}

// mkResultDir creates a result dir inside `repo` with an empty baseline-pre-files.txt.
func mkResultDir(t *testing.T, repo string) string {
	t.Helper()
	dir := filepath.Join(repo, ".codex-dispatch", "runs", "test-run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir result dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "baseline-pre-files.txt"), nil, 0o644); err != nil {
		t.Fatalf("write baseline-pre-files: %v", err)
	}
	return dir
}

func TestCaptureNoChanges(t *testing.T) {
	repo, head := initRepo(t)
	resultDir := mkResultDir(t, repo)

	stats, err := CaptureInDir(repo, head, resultDir)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(stats.FilesChanged) != 0 {
		t.Fatalf("FilesChanged = %v, want empty", stats.FilesChanged)
	}
	if stats.LinesAdded != 0 || stats.LinesRemoved != 0 {
		t.Fatalf("lines = (%d,%d), want (0,0)", stats.LinesAdded, stats.LinesRemoved)
	}

	// Verify on-disk artifacts.
	for _, name := range []string{"diff.patch", "files-changed.txt", "stats.json"} {
		if _, err := os.Stat(filepath.Join(resultDir, name)); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
	got, _ := os.ReadFile(filepath.Join(resultDir, "files-changed.txt"))
	if len(got) != 0 {
		t.Fatalf("files-changed.txt should be empty, got %q", got)
	}
	raw, _ := os.ReadFile(filepath.Join(resultDir, "stats.json"))
	var fromFile Stats
	if err := json.Unmarshal(raw, &fromFile); err != nil {
		t.Fatalf("stats.json unmarshal: %v", err)
	}
	if fromFile.LinesAdded != 0 || fromFile.LinesRemoved != 0 || len(fromFile.FilesChanged) != 0 {
		t.Fatalf("stats.json content = %+v, want zero values", fromFile)
	}
}

// writeFile is a small helper for the scenario tests.
func writeFile(t *testing.T, repo, rel, content string) {
	t.Helper()
	path := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestCaptureUntrackedFileAttributed(t *testing.T) {
	repo, head := initRepo(t)
	resultDir := mkResultDir(t, repo)
	writeFile(t, repo, "hello.txt", "hello\n")

	stats, err := CaptureInDir(repo, head, resultDir)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(stats.FilesChanged) != 1 || stats.FilesChanged[0] != "hello.txt" {
		t.Fatalf("FilesChanged = %v, want [hello.txt]", stats.FilesChanged)
	}
	if stats.LinesAdded < 1 {
		t.Fatalf("LinesAdded = %d, want >= 1", stats.LinesAdded)
	}
}

func TestCaptureModifiedTrackedFile(t *testing.T) {
	repo, head := initRepo(t)
	resultDir := mkResultDir(t, repo)
	writeFile(t, repo, "README.md", "initial\nnew line\n")

	stats, err := CaptureInDir(repo, head, resultDir)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(stats.FilesChanged) != 1 || stats.FilesChanged[0] != "README.md" {
		t.Fatalf("FilesChanged = %v, want [README.md]", stats.FilesChanged)
	}
	if stats.LinesAdded < 1 {
		t.Fatalf("LinesAdded = %d, want >= 1", stats.LinesAdded)
	}
	if stats.LinesRemoved != 0 {
		t.Fatalf("LinesRemoved = %d, want 0 (pure addition)", stats.LinesRemoved)
	}
}

func TestCaptureRenamedFile(t *testing.T) {
	repo, head := initRepo(t)
	resultDir := mkResultDir(t, repo)
	// Rename README.md -> NOTES.md
	if err := os.Rename(filepath.Join(repo, "README.md"), filepath.Join(repo, "NOTES.md")); err != nil {
		t.Fatalf("rename: %v", err)
	}

	stats, err := CaptureInDir(repo, head, resultDir)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	// Either rename detection treats it as one rename (1 file) or as a
	// delete+add pair (2 files). Both are acceptable — assert non-empty.
	if len(stats.FilesChanged) == 0 {
		t.Fatalf("FilesChanged should not be empty after rename")
	}
}

func TestCaptureExcludesPreExistingWIP(t *testing.T) {
	repo, head := initRepo(t)
	resultDir := mkResultDir(t, repo)
	// Pre-existing WIP: README.md was dirty before the run.
	if err := os.WriteFile(filepath.Join(resultDir, "baseline-pre-files.txt"),
		[]byte("README.md\n"), 0o644); err != nil {
		t.Fatalf("write baseline-pre-files: %v", err)
	}
	// "codex" adds README.md changes AND a new file.
	writeFile(t, repo, "README.md", "initial\ndirty\nfrom codex\n")
	writeFile(t, repo, "codex-only.txt", "from codex\n")

	stats, err := CaptureInDir(repo, head, resultDir)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	for _, f := range stats.FilesChanged {
		if f == "README.md" {
			t.Fatalf("README.md must be excluded as pre-existing WIP; got %v", stats.FilesChanged)
		}
	}
	var foundCodex bool
	for _, f := range stats.FilesChanged {
		if f == "codex-only.txt" {
			foundCodex = true
		}
	}
	if !foundCodex {
		t.Fatalf("codex-only.txt should be attributed; got %v", stats.FilesChanged)
	}
}

func TestCaptureExcludesResultDirItself(t *testing.T) {
	repo, head := initRepo(t)
	resultDir := mkResultDir(t, repo)
	// Codex did not edit anything in the working tree; only the run dir
	// itself exists under .codex-dispatch/runs/test-run. None of it should
	// show up as a codex-attributable change.

	stats, err := CaptureInDir(repo, head, resultDir)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(stats.FilesChanged) != 0 {
		t.Fatalf("FilesChanged = %v, want empty (run dir excluded)", stats.FilesChanged)
	}
}

func TestCaptureExcludesRuntimeArtifactsOutsideCurrentRunDir(t *testing.T) {
	repo, head := initRepo(t)
	resultDir := mkResultDir(t, repo)
	writeFile(t, repo, ".codex-dispatch/broker.addr", "127.0.0.1:12345\n")
	writeFile(t, repo, ".codex-dispatch/runs/other-run/stdout.log", "log\n")
	writeFile(t, repo, "meaningful.txt", "from codex\n")

	stats, err := CaptureInDir(repo, head, resultDir)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(stats.FilesChanged) != 1 || stats.FilesChanged[0] != "meaningful.txt" {
		t.Fatalf("FilesChanged = %v, want [meaningful.txt]", stats.FilesChanged)
	}
}

func TestCaptureLeavesIndexClean(t *testing.T) {
	repo, head := initRepo(t)
	resultDir := mkResultDir(t, repo)
	writeFile(t, repo, "untracked.txt", "new\n")

	if _, err := CaptureInDir(repo, head, resultDir); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// `git status --porcelain` should NOT show "A" (intent-to-add staged) for
	// untracked.txt — only "??" (untracked).
	out, err := runGit(repo, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "A ") || strings.HasPrefix(line, "AM") {
			t.Fatalf("intent-to-add was not reverted: %q", line)
		}
	}
}

// --- Packet 001 additions --------------------------------------------------

// mkRunDir creates a run dir under repo without seeding any baseline files (so
// CaptureBaseline can populate them via the real pre-run flow).
func mkRunDir(t *testing.T, repo string) string {
	t.Helper()
	dir := filepath.Join(repo, ".codex-dispatch", "runs", "test-run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	return dir
}

// commitFile adds and commits a file, returning the new HEAD sha.
func commitFile(t *testing.T, repo, rel, content string) string {
	t.Helper()
	writeFile(t, repo, rel, content)
	if _, err := runGit(repo, "add", "--", rel); err != nil {
		t.Fatalf("git add %s: %v", rel, err)
	}
	if _, err := runGit(repo, "commit", "-q", "-m", "add "+rel); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	out, err := runGit(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(out)
}

// TestCaptureExcludesPreExistingUntracked verifies that an untracked file that
// existed *before* the run (captured by CaptureBaseline) is excluded, while a
// genuinely new untracked file is attributed.
func TestCaptureExcludesPreExistingUntracked(t *testing.T) {
	repo, head := initRepo(t)
	resultDir := mkRunDir(t, repo)

	// Pre-existing untracked WIP present before the run.
	writeFile(t, repo, "preexisting.txt", "wip\n")
	if err := CaptureBaseline(repo, resultDir); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}

	// "codex" adds a new untracked file (and leaves preexisting.txt alone).
	writeFile(t, repo, "codex-new.txt", "from codex\n")

	stats, err := CaptureInDir(repo, head, resultDir)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if slices.Contains(stats.FilesChanged, "preexisting.txt") {
		t.Fatalf("pre-existing untracked file must be excluded; got %v", stats.FilesChanged)
	}
	if !slices.Contains(stats.FilesChanged, "codex-new.txt") {
		t.Fatalf("codex-new.txt should be attributed; got %v", stats.FilesChanged)
	}
}

// TestCaptureUnicodeSpaceName verifies a changed file whose name contains a
// space and non-ASCII bytes is reported verbatim and contributes line counts.
// This exercises core.quotepath=false + -z parsing.
func TestCaptureUnicodeSpaceName(t *testing.T) {
	repo, head := initRepo(t)
	resultDir := mkResultDir(t, repo)
	name := "résumé final.txt"
	writeFile(t, repo, name, "line one\nline two\n")

	stats, err := CaptureInDir(repo, head, resultDir)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !slices.Contains(stats.FilesChanged, name) {
		t.Fatalf("FilesChanged = %v, want to contain %q", stats.FilesChanged, name)
	}
	if stats.LinesAdded < 2 {
		t.Fatalf("LinesAdded = %d, want >= 2 (unicode/space file content)", stats.LinesAdded)
	}
}

// TestCaptureAttributesEditToAlreadyDirtyFile verifies the content-aware gate:
// a file already dirty at baseline that codex edits *further* is attributed,
// rather than being dropped by name-only subtraction (the resume-path case).
func TestCaptureAttributesEditToAlreadyDirtyFile(t *testing.T) {
	repo, _ := initRepo(t)
	head := commitFile(t, repo, "doc.txt", "v1\n")
	resultDir := mkRunDir(t, repo)

	// Pre-existing WIP edit to a tracked file, captured as the baseline.
	writeFile(t, repo, "doc.txt", "v1\nwip\n")
	if err := CaptureBaseline(repo, resultDir); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}

	// codex edits the already-dirty file further.
	writeFile(t, repo, "doc.txt", "v1\nwip\ncodex\n")

	stats, err := CaptureInDir(repo, head, resultDir)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !slices.Contains(stats.FilesChanged, "doc.txt") {
		t.Fatalf("an edit to an already-dirty file must be attributed; got %v", stats.FilesChanged)
	}
}

// TestCaptureNoOpOnAlreadyDirtyFileExcluded verifies that a file dirty at
// baseline which codex does NOT touch is excluded, yielding an empty changed
// set (which dispatch maps to exit_code=4).
func TestCaptureNoOpOnAlreadyDirtyFileExcluded(t *testing.T) {
	repo, _ := initRepo(t)
	head := commitFile(t, repo, "doc.txt", "v1\n")
	resultDir := mkRunDir(t, repo)

	writeFile(t, repo, "doc.txt", "v1\nwip\n")
	if err := CaptureBaseline(repo, resultDir); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	// codex does nothing to doc.txt (content identical to baseline).

	stats, err := CaptureInDir(repo, head, resultDir)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(stats.FilesChanged) != 0 {
		t.Fatalf("no-op on already-dirty file should yield empty set; got %v", stats.FilesChanged)
	}
}

// TestCaptureConcurrentDisjointUntracked runs two captures against the same
// working tree at the same time, each with a baseline that lists the other's
// file as pre-existing. With per-run index isolation each reports only its own
// file and neither errors; without it the shared .git/index would race.
func TestCaptureConcurrentDisjointUntracked(t *testing.T) {
	repo, head := initRepo(t)
	writeFile(t, repo, "a.txt", "alpha\n")
	writeFile(t, repo, "b.txt", "bravo\n")

	dirA := filepath.Join(repo, ".codex-dispatch", "runs", "run-a")
	dirB := filepath.Join(repo, ".codex-dispatch", "runs", "run-b")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// A's baseline says b.txt pre-existed; B's says a.txt pre-existed.
	if err := os.WriteFile(filepath.Join(dirA, "baseline-pre-files.txt"), []byte("b.txt\n"), 0o644); err != nil {
		t.Fatalf("write baseline A: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "baseline-pre-files.txt"), []byte("a.txt\n"), 0o644); err != nil {
		t.Fatalf("write baseline B: %v", err)
	}

	var (
		wg             sync.WaitGroup
		statsA, statsB Stats
		errA, errB     error
	)
	wg.Add(2)
	go func() { defer wg.Done(); statsA, errA = CaptureInDir(repo, head, dirA) }()
	go func() { defer wg.Done(); statsB, errB = CaptureInDir(repo, head, dirB) }()
	wg.Wait()

	if errA != nil || errB != nil {
		t.Fatalf("concurrent captures errored: A=%v B=%v", errA, errB)
	}
	if len(statsA.FilesChanged) != 1 || statsA.FilesChanged[0] != "a.txt" {
		t.Fatalf("capture A = %v, want [a.txt]", statsA.FilesChanged)
	}
	if len(statsB.FilesChanged) != 1 || statsB.FilesChanged[0] != "b.txt" {
		t.Fatalf("capture B = %v, want [b.txt]", statsB.FilesChanged)
	}
}
