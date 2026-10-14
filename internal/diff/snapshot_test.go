package diff

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/semanta-dev/codex-dispatch/internal/authority"
	"github.com/semanta-dev/codex-dispatch/internal/result"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func readSnapshot(t *testing.T, dir string) snapshot {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "baseline-snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSnapshotPreservesDeletedWIPAndRealIndex(t *testing.T) {
	repo, head := initRepo(t)
	dir := mkResultDir(t, repo)
	writeFile(t, repo, "README.md", "operator staged\n")
	if _, err := runGit(repo, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "README.md", "operator unstaged\n")
	writeFile(t, repo, "untracked.txt", "operator untracked\n")
	before, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CaptureBaseline(repo, dir); err != nil {
		t.Fatal(err)
	}
	s := readSnapshot(t, dir)
	writeFile(t, repo, "README.md", "initial\n")
	if err := os.Remove(filepath.Join(repo, "untracked.txt")); err != nil {
		t.Fatal(err)
	}
	stats, err := CaptureTaskInDir(repo, head, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stats.FilesChanged, []string{"README.md", "untracked.txt"}) {
		t.Fatalf("lost edits: %+v", stats)
	}
	for path, want := range map[string]string{"README.md": "operator unstaged\n", "untracked.txt": "operator untracked\n"} {
		got, err := runGit(s.AuthorityDir, "show", s.Tree+":"+path)
		if err != nil || got != want {
			t.Fatalf("unrecoverable %s: %q %v", path, got, err)
		}
	}
	after, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil || string(before) != string(after) {
		t.Fatal("real index changed", err)
	}
}

func TestSnapshotDeltaBinaryModesAndLiteralPaths(t *testing.T) {
	repo, head := initRepo(t)
	dir := mkResultDir(t, repo)
	writeFile(t, repo, "README.md", "initial\nprior WIP\n")
	if err := CaptureBaseline(repo, dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "README.md", "initial\nprior WIP\ntask\n")
	literalPath := ":(exclude)odd*"
	if runtime.GOOS == "windows" {
		literalPath = "literal[odd]"
	}
	writeFile(t, repo, literalPath, "literal\n")
	writeFile(t, repo, "binary", "\x00\x01\x02")
	if err := os.Chmod(filepath.Join(repo, "README.md"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("README.md", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	stats, err := CaptureTaskInDir(repo, head, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.FilesChanged) != 4 {
		t.Fatalf("missing changes: %+v", stats)
	}
	patch, err := os.ReadFile(filepath.Join(dir, "diff.patch"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(patch), "+prior WIP") || !strings.Contains(string(patch), "+task") || !strings.Contains(string(patch), "GIT binary patch") {
		t.Fatalf("bad task delta: %s", patch)
	}
	// Reverse the task delta in place, including binary, mode and symlink edits.
	if _, err := runGit(repo, "-c", "core.autocrlf=false", "apply", "--reverse", filepath.Join(dir, "diff.patch")); err != nil {
		t.Fatal(err)
	}
	got, err := snapshotTree(repo, dir, readSnapshot(t, dir).ObjectDir)
	if err != nil || got != readSnapshot(t, dir).Tree {
		t.Fatalf("replay differs: %s %v", got, err)
	}
}

func TestTaskCaptureRejectsMissingOrCorruptSnapshot(t *testing.T) {
	for _, scenario := range []string{"missing", "corrupt", "authority-missing", "head-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			repo, head := initRepo(t)
			dir := mkResultDir(t, repo)
			if err := CaptureBaseline(repo, dir); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing":
				if err := os.Remove(filepath.Join(dir, "baseline-snapshot.json")); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				writeFile(t, dir, "baseline-snapshot.json", "{")
			case "authority-missing":
				if err := os.Remove(filepath.Join(readSnapshot(t, dir).AuthorityDir, "authority.json")); err != nil {
					t.Fatal(err)
				}
			case "head-mismatch":
				head = strings.Repeat("0", 40)
			}
			if _, err := CaptureTaskInDir(repo, head, dir); err == nil {
				t.Fatal("accepted missing/invalid evidence")
			}
		})
	}
}

func TestSnapshotBypassesLossyCleanFilters(t *testing.T) {
	repo, _ := initRepo(t)
	dir := mkResultDir(t, repo)
	writeFile(t, repo, ".gitattributes", "README.md filter=lossy\n")
	if _, err := runGit(repo, "config", "filter.lossy.clean", "sed s/SECRET/PUBLIC/g"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "README.md", "SECRET user WIP\r\n")
	if err := CaptureBaseline(repo, dir); err != nil {
		t.Fatal(err)
	}
	got, err := runGit(readSnapshot(t, dir).AuthorityDir, "show", readSnapshot(t, dir).Tree+":README.md")
	if err != nil || got != "SECRET user WIP\r\n" {
		t.Fatalf("snapshot normalized bytes: %q %v", got, err)
	}
	original, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(filepath.Join(readSnapshot(t, dir).AuthorityDir, "baseline-index"))
	if err != nil || string(original) != string(backup) {
		t.Fatal("index backup differs", err)
	}
}

// BenchmarkSnapshotRoundTrip isolates the raw snapshot cost from model latency.
// Run with -run '^$' -bench BenchmarkSnapshotRoundTrip -benchtime=1x for a
// single observation; promotion measurements require repeated observations.
func BenchmarkSnapshotRoundTrip(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			repo := b.TempDir()
			for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "bench@example.invalid"}, {"config", "user.name", "bench"}} {
				if _, err := runGit(repo, args...); err != nil {
					b.Fatal(err)
				}
			}
			for i := 0; i < count; i++ {
				content := []byte(strings.Repeat("snapshot input\n", 8))
				if i%10 == 0 {
					content = make([]byte, 1024)
				}
				if err := os.WriteFile(filepath.Join(repo, fmt.Sprintf("file-%05d", i)), content, 0644); err != nil {
					b.Fatal(err)
				}
			}
			if _, err := runGit(repo, "add", "."); err != nil {
				b.Fatal(err)
			}
			if _, err := runGit(repo, "commit", "-qm", "benchmark"); err != nil {
				b.Fatal(err)
			}
			head, err := runGit(repo, "rev-parse", "HEAD")
			if err != nil {
				b.Fatal(err)
			}
			dir := filepath.Join(repo, ".codex-dispatch", "benchmark")
			if err := os.MkdirAll(dir, 0700); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := CaptureBaseline(repo, dir); err != nil {
					b.Fatal(err)
				}
				if _, err := CaptureTaskInDir(repo, strings.TrimSpace(head), dir); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestSnapshotBatchPreservesArbitraryPathBytes(t *testing.T) {
	repo, head := initRepo(t)
	dir := mkResultDir(t, repo)
	if err := CaptureBaseline(repo, dir); err != nil {
		t.Fatal(err)
	}
	paths := []string{"line\nbreak", "tab\there", "quote\"", "back\\slash", "unicode-雪"}
	if runtime.GOOS == "windows" {
		paths = []string{"space name", "bracket[name]", "unicode-雪"}
	}
	for _, p := range paths {
		writeFile(t, repo, p, "raw bytes\n")
	}
	stats, err := CaptureTaskInDir(repo, head, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.FilesChanged) != len(paths) {
		t.Fatalf("paths lost: %q", stats.FilesChanged)
	}
	if _, err := runGit(repo, "-c", "core.autocrlf=false", "apply", "--reverse", filepath.Join(dir, "diff.patch")); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if _, err := os.Lstat(filepath.Join(repo, p)); !os.IsNotExist(err) {
			t.Fatalf("path did not replay: %q %v", p, err)
		}
	}
}

func TestQuoteGitPathPreservesNonUTF8Byte(t *testing.T) {
	if got := quoteGitPath("raw-" + string([]byte{0xff})); got != "\"raw-\\377\"" {
		t.Fatalf("incorrect byte quoting: %q", got)
	}
}

func objectFiles(t *testing.T, repo string) map[string]struct{} {
	t.Helper()
	root := filepath.Join(repo, ".git", "objects")
	files := map[string]struct{}{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files[path] = struct{}{}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestPrivateSnapshotLeavesGitObjectsUntouched(t *testing.T) {
	repo, head := initRepo(t)
	dir := mkResultDir(t, repo)
	before := objectFiles(t, repo)
	writeFile(t, repo, "wip-only.txt", "confidential WIP\n")
	baseline, err := CaptureBaselineHandle(repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	after := objectFiles(t, repo)
	if len(before) != len(after) {
		t.Fatalf("ordinary object count changed: %d -> %d", len(before), len(after))
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			t.Fatalf("ordinary object changed: %s", path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Fatalf("WIP entered ordinary object store: %s", path)
		}
	}
	private, err := runGit(baseline.manifest.AuthorityDir, "show", baseline.manifest.Tree+":wip-only.txt")
	if err != nil || private != "confidential WIP\n" {
		t.Fatalf("private tree unavailable: %q %v", private, err)
	}
	writeFile(t, dir, "baseline-snapshot.json", `{"version":2}`)
	if _, err := baseline.Capture(repo, head, dir); err == nil {
		t.Fatal("accepted replaced workspace descriptor")
	}
}

func TestPrivateCaptureAndCleanVerificationRoundTrip(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("clean verifier is currently Linux-only")
	}
	if _, err := os.Stat("/usr/bin/bwrap"); err != nil {
		t.Skip("requires Linux bubblewrap")
	}
	repo, head := initRepo(t)
	dir := mkResultDir(t, repo)
	writeFile(t, repo, "module/value.txt", "operator WIP\n")
	module := filepath.Join(repo, "module")
	baseline, err := CaptureBaselineHandle(module, dir)
	if err != nil {
		t.Fatal(err)
	}
	before := objectFiles(t, repo)
	writeFile(t, repo, "module/value.txt", "operator WIP\ntask change\n")
	if _, err := baseline.Capture(module, head, dir); err != nil {
		t.Fatal(err)
	}
	after := objectFiles(t, repo)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("task objects entered ordinary repository")
	}
	if err := os.WriteFile(filepath.Join(dir, "baseline-head.txt"), []byte(head+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../scripts/clean-verify.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script, dir, "bash", "-c", `test "$(cat value.txt)" = "$(printf 'operator WIP\ntask change')" && test ! -e "$1" && ! cat "$1" && ! printf changed > "$1"`, "verify", filepath.Join(baseline.manifest.AuthorityDir, "baseline-index"))
	cmd.Dir = repo // the bound module workdir must win over caller's directory.
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clean verification: %v\n%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(dir, "diff.patch"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", script, dir, "true")
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "patch digest mismatch") {
		t.Fatalf("accepted substituted patch: %v %s", err, output)
	}
}

func TestPrivateSnapshotContainsCommittedBlobsWithoutAlternates(t *testing.T) {
	repo, _ := initRepo(t)
	dir := mkResultDir(t, repo)
	if err := CaptureBaseline(repo, dir); err != nil {
		t.Fatal(err)
	}
	s := readSnapshot(t, dir)
	if err := os.Rename(filepath.Join(repo, ".git", "objects"), filepath.Join(repo, ".git", "objects.saved")); err != nil {
		t.Fatal(err)
	}
	content, err := runGit(s.AuthorityDir, "show", s.Tree+":README.md")
	if err != nil || content != "initial\n" {
		t.Fatalf("private store depends on original objects: %q %v", content, err)
	}
	for _, name := range []string{"baseline-index", "index.tmp"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("private index leaked through %s: %v", name, err)
		}
	}
}

func TestTerminalSealUsesControllerResultAndRejectsResealing(t *testing.T) {
	for _, code := range []int{0, 4, 1, 124} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			repo, head := initRepo(t)
			dir := mkResultDir(t, repo)
			baseline, err := CaptureBaselineHandle(repo, dir)
			if err != nil {
				t.Fatal(err)
			}
			res := result.Result{ExitCode: code, SessionID: "controller-session", ErrorMessage: "controller diagnostic"}
			if code == 0 || code == 4 {
				if code == 0 {
					writeFile(t, repo, "README.md", "task change\n")
				}
				stats, err := baseline.Capture(repo, head, dir)
				if err != nil {
					t.Fatal(err)
				}
				res.DiffPath = filepath.Join(dir, "diff.patch")
				res.FilesChanged = stats.FilesChanged
			}

			writeFile(t, dir, "result.json", `{"exit_code":0,"session_id":"forged"}`)
			if err := baseline.SealResult(res); err != nil {
				t.Fatal(err)
			}
			got, err := authority.ReadFile(baseline.manifest.AuthorityDir, "result.json", 8192)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := result.Encode(res)
			if string(got) != string(want) {
				t.Fatalf("sealed exported result instead of controller result: %s", got)
			}
			data, err := authority.ReadFile(baseline.manifest.AuthorityDir, "terminal.json", 16384)
			if err != nil {
				t.Fatal(err)
			}
			var terminal map[string]any
			if err := json.Unmarshal(data, &terminal); err != nil {
				t.Fatal(err)
			}
			if terminal["result_digest"] != fmt.Sprintf("%x", sha256.Sum256(want)) {
				t.Fatal("result digest mismatch")
			}
			states := map[int]string{0: "SUCCEEDED", 4: "NO_CHANGES", 1: "FAILED", 124: "CANCELED"}
			if terminal["state"] != states[code] {
				t.Fatalf("wrong terminal outcome: %s", data)
			}
			if err := baseline.SealResult(res); err == nil {
				t.Fatal("terminal identity replaced")
			}
		})
	}
}

func TestCaptureDoesNotRunCleanFiltersBeforeRawSnapshot(t *testing.T) {
	repo, _ := initRepo(t)
	dir := mkResultDir(t, repo)
	writeFile(t, repo, ".gitattributes", "README.md filter=probe\n")
	if _, err := runGit(repo, "config", "filter.probe.clean", "touch filter-ran; cat"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "README.md", "private raw bytes\n")
	baseline, err := CaptureBaselineHandle(repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := baseline.PreexistingPatch(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(repo, "filter-ran")); !os.IsNotExist(err) {
		t.Fatalf("Git filter executed in controller: %v", err)
	}
	s := readSnapshot(t, dir)
	raw, err := runGit(s.AuthorityDir, "show", s.Tree+":README.md")
	if err != nil || raw != "private raw bytes\n" {
		t.Fatalf("raw snapshot failed: %q %v", raw, err)
	}
}

func TestRepeatedCapturePreservesPreviouslySealedPatch(t *testing.T) {
	repo, head := initRepo(t)
	dir := mkResultDir(t, repo)
	baseline, err := CaptureBaselineHandle(repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "README.md", "first change\n")
	if _, err := baseline.Capture(repo, head, dir); err != nil {
		t.Fatal(err)
	}
	private := baseline.manifest.AuthorityDir
	before, err := authority.ReadFile(private, "diff.patch", 8*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	seal, err := authority.ReadCapture(private)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "README.md", "second change\n")
	if _, err := baseline.Capture(repo, head, dir); err == nil {
		t.Fatal("accepted repeated capture")
	}
	after, err := authority.ReadFile(private, "diff.patch", 8*1024*1024)
	if err != nil || string(after) != string(before) {
		t.Fatal("rejected capture corrupted sealed patch", err)
	}
	if seal.PatchDigest != fmt.Sprintf("%x", sha256.Sum256(after)) {
		t.Fatal("original capture digest no longer matches")
	}
}

func TestSplitIndexHasIndependentPrivateRecovery(t *testing.T) {
	repo, _ := initRepo(t)
	dir := mkResultDir(t, repo)
	writeFile(t, repo, "README.md", "staged-only bytes\n")
	if _, err := runGit(repo, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	staged, err := runGit(repo, "rev-parse", ":README.md")
	if err != nil {
		t.Fatal(err)
	}
	staged = strings.TrimSpace(staged)
	// The working tree now differs, so only the original index retains staged bytes.
	writeFile(t, repo, "README.md", "unstaged working bytes\n")
	if _, err := runGit(repo, "update-index", "--split-index"); err != nil {
		t.Fatal(err)
	}
	want, err := runGit(repo, "ls-files", "--stage", "-z")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := CaptureBaselineHandle(repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.binding.SharedIndex == "" {
		t.Fatal("split index was not bound")
	}
	private := baseline.manifest.AuthorityDir
	if baseline.binding.IndexObjectsDigest == "" {
		t.Fatal("indexed blob closure was not bound")
	}
	got, err := runGitIndex(private, filepath.Join(private, "baseline-index"), "ls-files", "--stage", "-z")
	if err != nil || got != want {
		t.Fatalf("private split index cannot recover staging: %v %q", err, got)
	}
	blob, err := runGit(private, "cat-file", "blob", staged)
	if err != nil || blob != "staged-only bytes\n" {
		t.Fatalf("private store lost staged-only blob: %q %v", blob, err)
	}
	if err := os.Rename(filepath.Join(repo, ".git", "objects"), filepath.Join(repo, ".git", "objects.removed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, ".git", baseline.binding.SharedIndex)); err != nil {
		t.Fatal(err)
	}
	got, err = runGitIndex(private, filepath.Join(private, "baseline-index"), "ls-files", "--stage", "-z")
	if err != nil || got != want {
		t.Fatalf("private staging depended on source objects/index: %v", err)
	}
	blob, err = runGit(private, "cat-file", "blob", staged)
	if err != nil || blob != "staged-only bytes\n" {
		t.Fatalf("staged-only recovery depended on source objects: %q %v", blob, err)
	}
}

func TestAbsentIndexPublishesEmptyPrivateClosure(t *testing.T) {
	repo, head := initRepo(t)
	dir := mkResultDir(t, repo)
	if err := os.Remove(filepath.Join(repo, ".git", "index")); err != nil {
		t.Fatal(err)
	}
	baseline, err := CaptureBaselineHandle(repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.binding.IndexPresent {
		t.Fatal("missing index reported as present")
	}
	manifest, err := authority.ReadFile(baseline.manifest.AuthorityDir, "index-objects.json", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(manifest) != `{"version":1,"objects":[]}` {
		t.Fatalf("empty closure not canonical: %s", manifest)
	}
	if _, err := baseline.Capture(repo, head, dir); err != nil {
		t.Fatalf("missing index broke task capture: %v", err)
	}
}

func TestOversizedStagedBlobFailsWithinCaptureBudget(t *testing.T) {
	repo, _ := initRepo(t)
	dir := mkResultDir(t, repo)
	writeFile(t, repo, "staged-only.bin", strings.Repeat("x", 8*1024*1024+1))
	if _, err := runGit(repo, "add", "staged-only.bin"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "staged-only.bin", "small worktree copy\n")
	done := make(chan error, 1)
	go func() { _, err := CaptureBaselineHandle(repo, dir); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "exceeds capture limits") {
			t.Fatalf("wrong oversized blob result: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("oversized staged blob capture stalled")
	}
}

func TestSparseIndexPrivateRecoveryAndVerification(t *testing.T) {
	repo, _ := initRepo(t)
	writeFile(t, repo, "included/value.txt", "before\n")
	writeFile(t, repo, "excluded/nested/value.txt", "recover sparse bytes\n")
	for _, args := range [][]string{{"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "sparse fixture"}, {"sparse-checkout", "init", "--cone", "--sparse-index"}, {"sparse-checkout", "set", "included"}} {
		if _, err := runGit(repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	sparse, err := runGit(repo, "ls-files", "--sparse", "--stage")
	if err != nil || !strings.Contains(sparse, "040000 ") {
		t.Fatalf("fixture is not sparse: %s %v", sparse, err)
	}
	want, err := runGit(repo, "ls-files", "--stage", "-z")
	if err != nil {
		t.Fatal(err)
	}
	head, err := runGit(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	head = strings.TrimSpace(head)
	dir := mkResultDir(t, repo)
	baseline, err := CaptureBaselineHandle(repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	private := baseline.manifest.AuthorityDir
	t.Cleanup(func() { os.RemoveAll(private) })
	writeFile(t, repo, "included/value.txt", "after\n")
	if _, err := baseline.Capture(repo, head, dir); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("bwrap"); err == nil {
			writeFile(t, dir, "baseline-head.txt", head+"\n")
			script, err := filepath.Abs("../../scripts/clean-verify.sh")
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", script, dir, "bash", "-c", `test "$(cat included/value.txt)" = after`)
			cmd.Dir = repo
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("sparse clean verification: %v\n%s", err, output)
			}
		}
	}
	if err := os.Rename(filepath.Join(repo, ".git", "objects"), filepath.Join(repo, ".git", "objects.removed")); err != nil {
		t.Fatal(err)
	}
	got, err := runGitIndex(private, filepath.Join(private, "baseline-index"), "ls-files", "--stage", "-z")
	if err != nil || got != want {
		t.Fatalf("private sparse recovery lost entries: %v\ngot %q\nwant %q", err, got, want)
	}
	blob, err := runGitIndex(private, filepath.Join(private, "baseline-index"), "show", ":excluded/nested/value.txt")
	if err != nil || blob != "recover sparse bytes\n" {
		t.Fatalf("sparse bytes unavailable: %q %v", blob, err)
	}
}

func TestIndexClosureEnforcesDeadlineOnStalledObject(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX FIFO regression")
	}
	repo, _ := initRepo(t)
	writeFile(t, repo, "stalled.txt", "unique FIFO object\n")
	if _, err := runGit(repo, "add", "stalled.txt"); err != nil {
		t.Fatal(err)
	}
	oid, err := runGit(repo, "rev-parse", ":stalled.txt")
	if err != nil {
		t.Fatal(err)
	}
	oid = strings.TrimSpace(oid)
	object := filepath.Join(repo, ".git", "objects", oid[:2], oid[2:])
	if err := os.Remove(object); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("mkfifo", object).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v %s", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	private := t.TempDir()
	if _, err := runGit(private, "init", "--bare", "--quiet"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = copyIndexObjectClosureContext(ctx, repo, filepath.Join(repo, ".git", "index"), filepath.Join(private, "objects"), private)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("wrong stalled object result: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("Git read did not stop at deadline")
	}
}

func TestBoundedGitOutputRejectsOverflow(t *testing.T) {
	repo, _ := initRepo(t)
	oid, err := gitInput(repo, "", strings.Repeat("x", 1024*1024), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = runGitBounded(ctx, repo, "", "", "", 1024, "cat-file", "blob", strings.TrimSpace(oid))
	if err == nil || !strings.Contains(err.Error(), "exceeds quota") {
		t.Fatalf("wrong overflow result: %v", err)
	}
}
