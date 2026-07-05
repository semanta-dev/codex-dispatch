package dispatch

import (
	"os"
	"path/filepath"
	"strings"
)

// moduleManifests are the files that mark a directory as the root of a
// sub-project/module. go.work is intentionally excluded: it marks the
// multi-module *parent*, not a module, so it must never cause auto-scoping.
var moduleManifests = []string{
	"go.mod", "package.json", "pyproject.toml", "Cargo.toml",
	"composer.json", "build.gradle", "build.gradle.kts", "pom.xml",
}

// DeriveModuleDir returns the directory codex should run in when a dispatch is
// scoped to a single module of a multi-module repo, derived from the seeded
// files. It is the module (nearest ancestor carrying a module manifest) that
// owns ALL of filesCSV, provided that module is a strict subdirectory of
// repoRoot. It returns "" when:
//   - no files are seeded,
//   - the files span more than one module (common ancestor's owning module is
//     the repo root),
//   - any file resolves outside repoRoot,
//   - the owning module is the repo root itself (nothing to scope).
//
// This makes monorepo dispatches scope deterministically without relying on the
// caller to set CODEX_WORKDIR: e.g. seeding `server/server.go` in a go.work repo
// runs codex in `<root>/server`. The result is always inside repoRoot, so
// threadCWD never has to reject it.
func DeriveModuleDir(repoRoot, filesCSV string) string {
	files := splitCSV(filesCSV)
	if len(files) == 0 {
		return ""
	}
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return ""
	}

	var dirs []string
	for _, f := range files {
		p := f
		if !filepath.IsAbs(p) {
			p = filepath.Join(absRoot, p)
		}
		p = filepath.Clean(p)
		rel, err := filepath.Rel(absRoot, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "" // a seed file outside the repo → don't scope
		}
		dirs = append(dirs, filepath.Dir(p))
	}

	common := commonAncestor(dirs)
	if common == "" {
		return ""
	}
	// Walk up from the common ancestor to (but not including) the repo root,
	// returning the deepest directory that carries a module manifest. Reaching
	// the repo root means the owning module is the root — nothing to scope.
	for d := common; d != absRoot; d = filepath.Dir(d) {
		// Guard against walking above the root (shouldn't happen: all dirs are
		// inside absRoot, verified above).
		if rel, err := filepath.Rel(absRoot, d); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ""
		}
		if hasModuleManifest(d) {
			return d
		}
	}
	return ""
}

// SameDir reports whether a and b refer to the same directory, comparing
// absolute, symlink-resolved paths (falling back to lexical comparison when a
// path can't be resolved). Used to decide whether a dispatch was invoked at the
// repo root (and thus is a candidate for module auto-derivation).
func SameDir(a, b string) bool {
	ca := canonicalDir(a)
	cb := canonicalDir(b)
	return ca == cb
}

func canonicalDir(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func hasModuleManifest(dir string) bool {
	for _, m := range moduleManifests {
		if fi, err := os.Stat(filepath.Join(dir, m)); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
}

// commonAncestor returns the deepest directory that is an ancestor of (or equal
// to) every dir in dirs. All inputs are expected to be absolute, cleaned paths.
func commonAncestor(dirs []string) string {
	if len(dirs) == 0 {
		return ""
	}
	segs := strings.Split(dirs[0], string(filepath.Separator))
	for _, d := range dirs[1:] {
		other := strings.Split(d, string(filepath.Separator))
		n := min(len(other), len(segs))
		i := 0
		for i < n && segs[i] == other[i] {
			i++
		}
		segs = segs[:i]
	}
	common := strings.Join(segs, string(filepath.Separator))
	if common == "" {
		// All paths diverged at the filesystem root ("/a" vs "/b" share "").
		return string(filepath.Separator)
	}
	return common
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
