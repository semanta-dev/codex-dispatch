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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/semanta-dev/codex-dispatch/internal/artifact"
)

// Stats is the JSON shape written to result_dir/stats.json.
type Stats struct {
	FilesChanged []string `json:"files_changed"`
	LinesAdded   int      `json:"lines_added"`
	LinesRemoved int      `json:"lines_removed"`
}

// CaptureBaseline saves a recoverable pre-run working-tree snapshot privately
// and writes its exported descriptor to resultDir.
// Any failure prevents dispatch because attribution would be unreliable.
func CaptureBaseline(workdir, resultDir string) error {
	_, err := CaptureBaselineHandle(workdir, resultDir)
	return err
}

// CaptureBaselineHandle returns controller-held identity for the lifetime of a
// dispatch. The model-written export is never used to choose capture targets.
func CaptureBaselineHandle(workdir, resultDir string) (*Baseline, error) {
	if resultDir == "" {
		return nil, fmt.Errorf("resultDir required")
	}
	repoRoot, err := gitTopLevel(workdir)
	if err != nil {
		return nil, err
	}
	return saveSnapshot(repoRoot, workdir, resultDir)
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
// CaptureTaskInDir requires the recoverable baseline created by CaptureBaseline.
func CaptureTaskInDir(workdir, baselineHead, resultDir string) (Stats, error) {
	return captureInDir(workdir, baselineHead, resultDir, true)
}

func CaptureInDir(workdir, baselineHead, resultDir string) (Stats, error) {
	return captureInDir(workdir, baselineHead, resultDir, false)
}

func captureInDir(workdir, baselineHead, resultDir string, requireSnapshot bool) (Stats, error) {
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
	if raw, err := readSnapshotExport(resultDir); err == nil {
		var s snapshot
		if err := json.Unmarshal(raw, &s); err != nil {
			return Stats{}, fmt.Errorf("invalid baseline snapshot: %w", err)
		}
		return captureSnapshot(repoRoot, baselineHead, resultDir, s)
	} else if requireSnapshot || !os.IsNotExist(err) {
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
	return runGitIndexObjects(workdir, indexFile, "", args...)
}

// gitCommand strips caller Git overrides; snapshot object writes have no
// alternates so each captured tree is self-contained in the private store.
func gitCommand(workdir, indexFile, objectDir string, args ...string) *exec.Cmd {
	return gitCommandContext(context.Background(), workdir, indexFile, objectDir, args...)
}

// emptyConfigPath is a path Git can stat and read as an empty file. On Windows
// os.DevNull is "NUL", a device some Git for Windows builds cannot stat when it
// is supplied as a config or hooks path: they abort every command with
// "unable to access 'NUL': Invalid argument". An empty regular file is
// semantically identical to /dev/null for these settings and is portable.
var emptyConfigPath = sync.OnceValue(func() string {
	if runtime.GOOS != "windows" {
		return os.DevNull
	}
	f, err := os.CreateTemp("", "codex-dispatch-empty-*.config")
	if err != nil {
		return os.DevNull // Fall back rather than failing the capture outright.
	}
	name := f.Name()
	f.Close()
	return name
})

func gitCommandContext(ctx context.Context, workdir, indexFile, objectDir string, args ...string) *exec.Cmd {
	if objectDir != "" {
		args = append([]string{"-c", "core.splitIndex=false"}, args...)
	}
	empty := emptyConfigPath()
	full := append([]string{"--no-replace-objects", "-c", "core.quotepath=false", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + empty, "-c", "protocol.allow=never"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.WaitDelay = time.Second
	cmd.Dir = workdir
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	// GIT_CONFIG_NOSYSTEM also stops Git opening the system config path at all,
	// which scripts/windows_evidence.py already sets and the Go path did not.
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM="+empty, "GIT_CONFIG_GLOBAL="+empty, "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0")
	if indexFile != "" {
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+indexFile)
	}
	if objectDir != "" {
		cmd.Env = append(cmd.Env, "GIT_OBJECT_DIRECTORY="+objectDir, "GIT_ALTERNATE_OBJECT_DIRECTORIES=")
	}
	return cmd
}

// boundedGitOutput cancels the child as soon as either output stream overflows.
// Returning a writer error alone could leave Git blocked on a full output pipe.
type boundedGitOutput struct {
	data     bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	overflow bool
}

func (b *boundedGitOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.data.Len() {
		b.overflow = true
		b.cancel()
		return 0, fmt.Errorf("git output exceeds quota")
	}
	return b.data.Write(p)
}

func runGitBounded(ctx context.Context, workdir, indexFile, objectDir, input string, limit int, args ...string) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := gitCommandContext(ctx, workdir, indexFile, objectDir, args...)
	cmd.Stdin = strings.NewReader(input)
	stdout := boundedGitOutput{limit: limit, cancel: cancel}
	stderr := boundedGitOutput{limit: 64000, cancel: cancel}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if stdout.overflow || stderr.overflow {
		return "", fmt.Errorf("git output exceeds quota")
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("git capture budget: %w", ctx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.data.String()))
	}
	return stdout.data.String(), nil
}

func runGitIndexObjects(workdir, indexFile, objectDir string, args ...string) (string, error) {
	cmd := gitCommand(workdir, indexFile, objectDir, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
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
	// This run owns the temporary name; remove only a prior interrupted copy,
	// then publish the replacement with O_EXCL so symlink substitutions fail.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return "", cleanup, err
	}

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
	root, err := os.OpenRoot(filepath.Dir(src))
	if err != nil {
		return err
	}
	defer root.Close()
	data, err := artifact.ReadRegularAt(root, filepath.Base(src), 512*1024*1024)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func gitAddIntentToAdd(workdir, indexFile string, paths []string) error {
	args := append([]string{"add", "--intent-to-add", "--"}, paths...)
	_, err := runGitIndex(workdir, indexFile, args...)
	return err
}

// hashFile returns the git blob hash of the working-tree content at path, or ""
// if the file is missing or git fails. hash-object does not consult the index.
func hashFile(repoRoot, path string) string {
	out, err := runGit(repoRoot, "hash-object", "--no-filters", "--", path)
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

func writeDiffPatch(repoRoot, indexFile, baseline string, files []string, path string, after ...string) error {
	return writeDiffPatchObjects(repoRoot, indexFile, "", baseline, files, path, after...)
}

func writeDiffPatchObjects(repoRoot, indexFile, objectDir, baseline string, files []string, path string, after ...string) error {
	patch, err := diffPatchBytes(repoRoot, indexFile, objectDir, baseline, files, after...)
	if err != nil {
		return err
	}
	return artifact.WriteAtomic(filepath.Dir(path), filepath.Base(path), patch, 0600)
}

func diffPatchBytes(repoRoot, indexFile, objectDir, baseline string, files []string, after ...string) ([]byte, error) {
	if len(files) == 0 {
		return nil, nil
	}
	args := append([]string{"--literal-pathspecs", "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", baseline}, after...)
	args = append(append(args, "--"), files...)
	out, err := runGitIndexObjects(repoRoot, indexFile, objectDir, args...)
	if err != nil {
		return nil, err
	}
	if len(out) > 8*1024*1024 {
		return nil, fmt.Errorf("task patch exceeds byte quota")
	}
	return []byte(out), nil
}

func numstat(repoRoot, indexFile, baseline string, files []string, after ...string) (int, int, error) {
	return numstatObjects(repoRoot, indexFile, "", baseline, files, after...)
}

func numstatObjects(repoRoot, indexFile, objectDir, baseline string, files []string, after ...string) (int, int, error) {
	if len(files) == 0 {
		return 0, 0, nil
	}
	args := append([]string{"--literal-pathspecs", "diff", baseline}, after...)
	args = append(append(args, "--no-renames", "--numstat", "-z", "--"), files...)
	out, err := runGitIndexObjects(repoRoot, indexFile, objectDir, args...)
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

func writeFilesChanged(path string, files []string) error {
	if len(files) == 0 {
		return artifact.WriteAtomic(filepath.Dir(path), filepath.Base(path), nil, 0o600)
	}
	var buf bytes.Buffer
	for _, f := range files {
		buf.WriteString(f)
		buf.WriteByte('\n')
	}
	return artifact.WriteAtomic(filepath.Dir(path), filepath.Base(path), buf.Bytes(), 0o600)
}

func writeStatsJSON(path string, stats Stats) error {
	if stats.FilesChanged == nil {
		stats.FilesChanged = []string{}
	}
	b, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	return artifact.WriteAtomic(filepath.Dir(path), filepath.Base(path), b, 0o600)
}
