package diff

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
		got, err := runGit(repo, "show", s.Ref+":"+path)
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
	writeFile(t, repo, ":(exclude)odd*", "literal\n")
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
	if _, err := runGit(repo, "apply", "--reverse", filepath.Join(dir, "diff.patch")); err != nil {
		t.Fatal(err)
	}
	got, err := snapshotTree(repo, dir)
	if err != nil || got != readSnapshot(t, dir).Tree {
		t.Fatalf("replay differs: %s %v", got, err)
	}
}

func TestTaskCaptureRejectsMissingOrCorruptSnapshot(t *testing.T) {
	for _, scenario := range []string{"missing", "corrupt", "ref-changed", "head-mismatch"} {
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
			case "ref-changed":
				if _, err := runGit(repo, "update-ref", "-d", readSnapshot(t, dir).Ref); err != nil {
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
	got, err := runGit(repo, "show", readSnapshot(t, dir).Ref+":README.md")
	if err != nil || got != "SECRET user WIP\r\n" {
		t.Fatalf("snapshot normalized bytes: %q %v", got, err)
	}
	original, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(filepath.Join(dir, "baseline-index"))
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
	if _, err := runGit(repo, "apply", "--reverse", filepath.Join(dir, "diff.patch")); err != nil {
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
