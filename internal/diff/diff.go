// Package diff isolates codex-attributable working-tree changes against a
// baseline git rev and writes the artifacts dispatch consumes.
//
// Single-tree concurrency: dispatch may run several captures against the same
// working tree at once (parallel packet fanout, Option A). Every git command
// that would otherwise touch the shared .git/index (intent-to-add staging of
// untracked files, and the diffs that read it) is routed through a per-run
// temporary GIT_INDEX_FILE seeded from the real index, so concurrent captures
// never corrupt each other's staging or the operator's index.
package diff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Stats is the JSON shape written to result_dir/stats.json.
type Stats struct {
	FilesChanged []string `json:"files_changed"`
	LinesAdded   int      `json:"lines_added"`
	LinesRemoved int      `json:"lines_removed"`
}

// CaptureBaseline records, before codex runs, which paths are already dirty or
// untracked in the working tree (so they can be excluded from attribution) and
// a content signature (blob hash) for each (so a later edit to an already-dirty
// file is still attributed, while a no-op is not). It writes
// baseline-pre-files.txt (NUL-delimited paths) and baseline-pre-hashes.txt
// (NUL-delimited path,hash pairs) under resultDir.
//
// It is best-effort: git enumeration errors are swallowed (a missing baseline
// just means nothing is pre-excluded); only a resultDir write failure returns
// an error.
func CaptureBaseline(workdir, resultDir string) error {
	if resultDir == "" {
		return fmt.Errorf("resultDir required")
	}
	repoRoot, err := gitTopLevel(workdir)
	if err != nil {
		// Not a usable git repo; write empty baselines so the post-run reader
		// has well-defined inputs.
		_ = writeNULList(filepath.Join(resultDir, "baseline-pre-files.txt"), nil)
		_ = writeNULPairs(filepath.Join(resultDir, "baseline-pre-hashes.txt"), nil, nil)
		return nil
	}
	resultRel, _ := relIfUnder(repoRoot, resultDir)

	tracked, _ := listChangedNames(repoRoot, "", "HEAD", resultRel)
	untracked, _ := listUntracked(repoRoot, "", resultRel)
	pre := dedup(append(tracked, untracked...))

	hashes := make(map[string]string, len(pre))
	for _, f := range pre {
		if h := hashFile(repoRoot, f); h != "" {
			hashes[f] = h
		}
	}

	if err := writeNULList(filepath.Join(resultDir, "baseline-pre-files.txt"), pre); err != nil {
		return err
	}
	return writeNULPairs(filepath.Join(resultDir, "baseline-pre-hashes.txt"), pre, hashes)
}

// Capture runs against the current working directory. CaptureInDir is the
// testable variant.
func Capture(baselineHead, resultDir string) (Stats, error) {
	wd, err := os.Getwd()
	if err != nil {
		return Stats{}, err
	}
	return CaptureInDir(wd, baselineHead, resultDir)
}

// CaptureInDir takes the repo's working directory as an explicit argument so
// tests don't need to chdir.
func CaptureInDir(workdir, baselineHead, resultDir string) (Stats, error) {
	if baselineHead == "" {
		return Stats{}, fmt.Errorf("baselineHead required")
	}
	if resultDir == "" {
		return Stats{}, fmt.Errorf("resultDir required")
	}

	repoRoot, err := gitTopLevel(workdir)
	if err != nil {
		return Stats{}, err
	}

	preFiles, err := readPreFiles(filepath.Join(resultDir, "baseline-pre-files.txt"))
	if err != nil {
		return Stats{}, err
	}
	preHashes := readPreHashes(filepath.Join(resultDir, "baseline-pre-hashes.txt"))

	resultRel, err := relIfUnder(repoRoot, resultDir)
	if err != nil {
		return Stats{}, err
	}

	// Per-run temp index so concurrent captures never mutate the shared index.
	indexFile, cleanup, err := setupTempIndex(repoRoot, resultDir)
	if err != nil {
		return Stats{}, err
	}
	defer cleanup()

	untracked, err := listUntracked(repoRoot, indexFile, resultRel)
	if err != nil {
		return Stats{}, err
	}
	if len(untracked) > 0 {
		// Stage intent-to-add into the *temp* index only, so untracked files
		// surface in `git diff <baseline>`; the real index is never touched and
		// no reset is needed (the temp index is discarded by cleanup()).
		if err := gitAddIntentToAdd(repoRoot, indexFile, untracked); err != nil {
			return Stats{}, err
		}
	}

	postFiles, err := listChangedNames(repoRoot, indexFile, baselineHead, resultRel)
	if err != nil {
		return Stats{}, err
	}

	codexFiles := attribute(repoRoot, postFiles, preFiles, preHashes)

	stats := Stats{FilesChanged: codexFiles}
	if err := writeDiffPatch(repoRoot, indexFile, baselineHead, codexFiles, filepath.Join(resultDir, "diff.patch")); err != nil {
		return Stats{}, err
	}
	added, removed, err := numstat(repoRoot, indexFile, baselineHead, codexFiles)
	if err != nil {
		return Stats{}, err
	}
	stats.LinesAdded = added
	stats.LinesRemoved = removed

	if err := writeFilesChanged(filepath.Join(resultDir, "files-changed.txt"), codexFiles); err != nil {
		return Stats{}, err
	}
	if err := writeStatsJSON(filepath.Join(resultDir, "stats.json"), stats); err != nil {
		return Stats{}, err
	}
	return stats, nil
}

// attribute keeps a post-baseline changed path when it is a brand-new change,
// or — for a path already dirty/untracked at baseline — only when its content
// differs from the captured baseline signature. A pre-existing dirty path with
// no baseline signature falls back to name-only exclusion (dropped), matching
// the historical behavior when no content baseline is available.
func attribute(repoRoot string, post []string, pre map[string]struct{}, preHashes map[string]string) []string {
	out := make([]string, 0, len(post))
	for _, f := range post {
		if _, dirty := pre[f]; !dirty {
			out = append(out, f) // brand-new change
			continue
		}
		base, hadBase := preHashes[f]
		if !hadBase {
			continue // pre-existing WIP, no signature -> exclude (name-only)
		}
		if hashFile(repoRoot, f) != base {
			out = append(out, f) // content changed relative to baseline -> codex edit
		}
		// else: identical to baseline -> no-op, exclude.
	}
	sort.Strings(out)
	return out
}

// --- git helpers -----------------------------------------------------------

func gitTopLevel(workdir string) (string, error) {
	out, err := runGit(workdir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// runGit runs git in workdir against the real index. quotepath is disabled so
// paths with non-ASCII bytes are emitted verbatim (paired with -z parsing).
func runGit(workdir string, args ...string) (string, error) {
	return runGitIndex(workdir, "", args...)
}

// runGitIndex runs git in workdir; when indexFile is non-empty the command is
// pointed at it via GIT_INDEX_FILE so it never reads or writes the shared index.
func runGitIndex(workdir, indexFile string, args ...string) (string, error) {
	full := append([]string{"-c", "core.quotepath=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = workdir
	if indexFile != "" {
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+indexFile)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// setupTempIndex creates <resultDir>/index.tmp seeded from the repo's current
// index (copied so stat cache and staged state are preserved), or — when no
// index exists yet — seeded from HEAD via read-tree. The returned cleanup
// removes the temp file.
func setupTempIndex(repoRoot, resultDir string) (string, func(), error) {
	tmp := filepath.Join(resultDir, "index.tmp")
	cleanup := func() { _ = os.Remove(tmp) }

	gitDir, err := runGit(repoRoot, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", cleanup, err
	}
	realIndex := filepath.Join(strings.TrimSpace(gitDir), "index")

	if err := copyFile(realIndex, tmp); err != nil {
		if !os.IsNotExist(err) {
			return "", cleanup, err
		}
		// No index on disk yet (rare); seed the temp index from HEAD.
		if _, err := runGitIndex(repoRoot, tmp, "read-tree", "HEAD"); err != nil {
			return "", cleanup, err
		}
	}
	return tmp, cleanup, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func gitAddIntentToAdd(workdir, indexFile string, paths []string) error {
	args := append([]string{"add", "--intent-to-add", "--"}, paths...)
	_, err := runGitIndex(workdir, indexFile, args...)
	return err
}

// hashFile returns the git blob hash of the working-tree content at path, or ""
// if the file is missing or git fails. hash-object does not consult the index.
func hashFile(repoRoot, path string) string {
	out, err := runGit(repoRoot, "hash-object", "--", path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// --- file/path helpers -----------------------------------------------------

func readPreFiles(path string) (map[string]struct{}, error) {
	set := map[string]struct{}{}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return set, nil
		}
		return nil, err
	}
	// Tolerate both the NUL-delimited format written by CaptureBaseline and the
	// historical newline-delimited format (hand-written fixtures, older runs).
	for _, f := range splitNULorNL(string(b)) {
		if f != "" {
			set[f] = struct{}{}
		}
	}
	return set, nil
}

func readPreHashes(path string) map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	toks := splitZ(string(b))
	for i := 0; i+1 < len(toks); i += 2 {
		if toks[i] != "" {
			m[toks[i]] = toks[i+1]
		}
	}
	return m
}

func relIfUnder(repoRoot, dir string) (string, error) {
	abs, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		// dir does not yet exist on disk; fall back to lexical Abs so
		// callers that pass a future path still get a useful answer.
		abs, err = filepath.Abs(dir)
		if err != nil {
			return "", err
		}
	}
	// repoRoot should already be canonical from `git rev-parse --show-toplevel`,
	// but apply EvalSymlinks defensively. Ignore error; fall back to repoRoot as-is.
	if resolved, err := filepath.EvalSymlinks(repoRoot); err == nil {
		repoRoot = resolved
	}
	rel, err := filepath.Rel(repoRoot, abs)
	if err != nil {
		return "", nil
	}
	if strings.HasPrefix(rel, "..") || rel == "." {
		return "", nil
	}
	return rel, nil
}

func isUnder(path, prefix string) bool {
	if prefix == "" {
		return false
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func isRuntimeArtifact(path string) bool {
	return isUnder(path, ".codex-dispatch")
}

func keepPath(f, resultRel string) bool {
	return f != "" && !isRuntimeArtifact(f) && !isUnder(f, resultRel)
}

func listUntracked(repoRoot, indexFile, resultRel string) ([]string, error) {
	out, err := runGitIndex(repoRoot, indexFile, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range splitZ(out) {
		if keepPath(f, resultRel) {
			files = append(files, f)
		}
	}
	return files, nil
}

func listChangedNames(repoRoot, indexFile, baseline, resultRel string) ([]string, error) {
	out, err := runGitIndex(repoRoot, indexFile, "diff", baseline, "--name-only", "-z")
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var files []string
	for _, f := range splitZ(out) {
		if !keepPath(f, resultRel) {
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		files = append(files, f)
	}
	sort.Strings(files)
	return files, nil
}

func writeDiffPatch(repoRoot, indexFile, baseline string, files []string, path string) error {
	if len(files) == 0 {
		return os.WriteFile(path, nil, 0o644)
	}
	args := append([]string{"diff", baseline, "--"}, files...)
	out, err := runGitIndex(repoRoot, indexFile, args...)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(out), 0o644)
}

func numstat(repoRoot, indexFile, baseline string, files []string) (int, int, error) {
	if len(files) == 0 {
		return 0, 0, nil
	}
	args := append([]string{"diff", baseline, "--numstat", "-z", "--"}, files...)
	out, err := runGitIndex(repoRoot, indexFile, args...)
	if err != nil {
		return 0, 0, err
	}
	added, removed := 0, 0
	// -z numstat: records are NUL-terminated. A normal record is
	// "<add>\t<del>\t<path>"; a rename/copy is "<add>\t<del>\t" followed by two
	// separate NUL-terminated tokens (old path, new path) which we skip.
	toks := splitZ(out)
	for i := 0; i < len(toks); {
		fields := strings.SplitN(toks[i], "\t", 3)
		if len(fields) < 3 {
			i++ // a bare path token (rename continuation) or malformed; skip
			continue
		}
		added += parseNumstat(fields[0])
		removed += parseNumstat(fields[1])
		if fields[2] == "" {
			i += 3 // rename/copy: consume the old+new path tokens that follow
		} else {
			i++
		}
	}
	return added, removed, nil
}

func parseNumstat(s string) int {
	if s == "-" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// splitZ splits NUL-delimited git output, dropping the trailing empty token.
func splitZ(s string) []string {
	parts := strings.Split(s, "\x00")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitNULorNL splits on NUL or newline (tolerating either artifact format).
func splitNULorNL(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '\x00' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func dedup(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, f := range in {
		if f == "" {
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func writeNULList(path string, items []string) error {
	var buf bytes.Buffer
	for _, s := range items {
		buf.WriteString(s)
		buf.WriteByte(0)
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func writeNULPairs(path string, keys []string, m map[string]string) error {
	var buf bytes.Buffer
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		buf.WriteString(k)
		buf.WriteByte(0)
		buf.WriteString(v)
		buf.WriteByte(0)
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func writeFilesChanged(path string, files []string) error {
	if len(files) == 0 {
		return os.WriteFile(path, nil, 0o644)
	}
	var buf bytes.Buffer
	for _, f := range files {
		buf.WriteString(f)
		buf.WriteByte('\n')
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func writeStatsJSON(path string, stats Stats) error {
	if stats.FilesChanged == nil {
		stats.FilesChanged = []string{}
	}
	b, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
