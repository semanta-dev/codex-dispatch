package diff

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type snapshot struct {
	Version int    `json:"version"`
	Head    string `json:"head"`
	Tree    string `json:"tree"`
	Ref     string `json:"ref"`
}

// snapshotTree stages only into a temporary index. Both trees exclude runtime
// artifacts; untracked non-ignored files, deletions, symlinks and modes survive.
func snapshotTree(repo, resultDir string) (string, error) {
	index, cleanup, err := setupTempIndex(repo, resultDir)
	if err != nil {
		return "", err
	}
	defer cleanup()
	rel, err := relIfUnder(repo, resultDir)
	if err != nil {
		return "", err
	}
	excluded := []string{".codex-dispatch"}
	if rel != "" {
		excluded = append(excluded, rel)
	}
	listed, err := runGitIndex(repo, index, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
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
	if _, err := runGitIndex(repo, index, "read-tree", "--empty"); err != nil {
		return "", err
	}
	var entries, batch strings.Builder
	var regularPaths, regularModes []string
	for _, p := range paths {
		full := filepath.Join(repo, filepath.FromSlash(p))
		info, err := os.Lstat(full)
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
			regularPaths = append(regularPaths, p)
			regularModes = append(regularModes, mode)
			batch.WriteString(quoteGitPath(p))
			batch.WriteByte('\n')
			continue
		case info.Mode()&os.ModeSymlink != 0:
			mode = "120000"
			var target string
			target, err = os.Readlink(full)
			if err == nil {
				hash, err = gitInput(repo, "", target, "hash-object", "--no-filters", "-w", "--stdin")
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
		out, err := gitInput(repo, "", batch.String(), "hash-object", "--no-filters", "-w", "--stdin-paths")
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
	if _, err := gitInput(repo, index, entries.String(), "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	tree, err := runGitIndex(repo, index, "write-tree")
	return strings.TrimSpace(tree), err
}

func saveSnapshot(repo, resultDir string) error {
	head, err := runGit(repo, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	gitDir, err := runGit(repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	if err := copyFile(filepath.Join(strings.TrimSpace(gitDir), "index"), filepath.Join(resultDir, "baseline-index")); err != nil && !os.IsNotExist(err) {
		return err
	}
	tree, err := snapshotTree(repo, resultDir)
	if err != nil {
		return err
	}
	var suffix [16]byte
	if _, err = rand.Read(suffix[:]); err != nil {
		return err
	}
	ref := "refs/codex-dispatch/baselines/" + hex.EncodeToString(suffix[:])
	// Keep the tree reachable for recovery until the operator removes this ref.
	if _, err = runGit(repo, "update-ref", ref, tree); err != nil {
		return err
	}
	b, err := json.Marshal(snapshot{Version: 2, Head: strings.TrimSpace(head), Tree: tree, Ref: ref})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(resultDir, "baseline-snapshot.json"), b, 0600)
}

func captureSnapshot(repo, head, resultDir string, s snapshot) (Stats, error) {
	if s.Version != 2 || s.Head != head || !objectID(s.Tree) || !strings.HasPrefix(s.Ref, "refs/codex-dispatch/baselines/") {
		return Stats{}, fmt.Errorf("invalid or mismatched baseline snapshot")
	}
	resolved, err := runGit(repo, "rev-parse", "--verify", s.Ref+"^{tree}")
	if err != nil || strings.TrimSpace(resolved) != s.Tree {
		return Stats{}, fmt.Errorf("baseline snapshot is unavailable or changed")
	}
	after, err := snapshotTree(repo, resultDir)
	if err != nil {
		return Stats{}, err
	}
	out, err := runGit(repo, "diff", "--no-renames", "--name-only", "-z", s.Tree, after)
	if err != nil {
		return Stats{}, err
	}
	files := splitZ(out)
	stats := Stats{FilesChanged: files}
	if err := writeDiffPatch(repo, "", s.Tree, files, filepath.Join(resultDir, "diff.patch"), after); err != nil {
		return Stats{}, err
	}
	stats.LinesAdded, stats.LinesRemoved, err = numstat(repo, "", s.Tree, files, after)
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
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = os.Environ()
	if index != "" {
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
	}
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
