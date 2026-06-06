// Package prompt assembles the per-run prompt sent to codex.
//
// Section order (frozen by golden test):
//
//	TASK / ACCEPTANCE CRITERIA / CONVENTIONS / RELEVANT FILES /
//	CONSTRAINTS / PRIOR FEEDBACK
//
// Each non-required section is omitted if empty.
//
// # Trust model
//
// TASK, ACCEPTANCE CRITERIA, CONVENTIONS and RELEVANT FILES carry content that
// originates outside the operator's control (the task body, files on disk, a
// conventions file in the checkout). That content must not be able to forge a
// later section header — in particular it must not be able to inject a fake
// "CONSTRAINTS" header and thereby override the operator-authoritative
// CONSTRAINTS section. Each untrusted block is therefore wrapped in a fenced
// "untrusted content" envelope delimited by a per-build nonce; any occurrence of
// the fence markers inside the content is neutralized so the envelope cannot be
// closed early. The CONSTRAINTS section is emitted outside any envelope and is
// the authoritative instruction set.
package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileSection is one entry in the RELEVANT FILES block.
type FileSection struct {
	Path    string
	Content string
	Missing bool // when true, Content is ignored and a "not found" note is emitted
}

// Inputs is the full set of variables the prompt assembler consumes.
type Inputs struct {
	Task               string
	Acceptance         string
	ConventionsTag     string // displayed in the section heading; usually the abs path
	Conventions        string // content; if empty and ConventionsMissing=false, the section is omitted
	ConventionsMissing bool   // emit "[CODEX_CONVENTIONS_FILE=<tag> not found]" instead of content
	FilesIncluded      []FileSection
	Constraints        string
	Feedback           string
}

// Build returns the assembled prompt text.
//
// The fence nonce is derived deterministically from every untrusted field (see
// deriveNonce) so the fence markers cannot be predicted — and thus forged —
// from the operator constraints alone, while keeping Build a pure function for
// the golden test.
func Build(in Inputs) string {
	return BuildWithNonce(deriveNonce(in), in)
}

// BuildWithNonce is Build with an explicit fence nonce. It exists so callers
// (notably tests) can inject a known nonce and craft content that contains the
// exact active fence strings, exercising the early-close-escape path in
// writeUntrusted that the content-derived nonce normally makes unreachable in a
// single Build call. Production code should call Build, not this.
func BuildWithNonce(nonce string, in Inputs) string {
	openFence := "<<<CODEX_UNTRUSTED:" + nonce
	closeFence := nonce + ":CODEX_UNTRUSTED>>>"

	var b strings.Builder

	b.WriteString("TASK\n")
	writeUntrusted(&b, in.Task, openFence, closeFence)
	b.WriteString("\nACCEPTANCE CRITERIA\n")
	writeUntrusted(&b, in.Acceptance, openFence, closeFence)

	switch {
	case in.ConventionsMissing:
		b.WriteString("\nCONVENTIONS\n")
		b.WriteString("[CODEX_CONVENTIONS_FILE=")
		b.WriteString(in.ConventionsTag)
		b.WriteString(" not found]\n")
	case in.Conventions != "":
		b.WriteString("\nCONVENTIONS (")
		b.WriteString(in.ConventionsTag)
		b.WriteString(")\n")
		writeUntrusted(&b, in.Conventions, openFence, closeFence)
	}

	if len(in.FilesIncluded) > 0 {
		b.WriteString("\nRELEVANT FILES\n")
		for _, f := range in.FilesIncluded {
			b.WriteString("\n=== ")
			b.WriteString(f.Path)
			b.WriteString(" ===\n")
			if f.Missing {
				b.WriteString("[file not found: ")
				b.WriteString(f.Path)
				b.WriteString("]\n")
			} else {
				writeUntrusted(&b, f.Content, openFence, closeFence)
			}
		}
	}

	// CONSTRAINTS and PRIOR FEEDBACK are operator-authoritative: they are emitted
	// outside any untrusted envelope so the model reads them as the binding
	// instruction set even if an earlier untrusted block tried to spoof a header.
	if in.Constraints != "" {
		b.WriteString("\nCONSTRAINTS\n")
		b.WriteString(in.Constraints)
		b.WriteString("\n")
	}

	if in.Feedback != "" {
		b.WriteString("\nPRIOR FEEDBACK\n")
		b.WriteString(in.Feedback)
		b.WriteString("\n")
	}

	return b.String()
}

// writeUntrusted emits content inside a nonce-delimited fence so it cannot forge
// a later section header. A trailing newline is guaranteed after the close fence
// so the next section's own leading "\n" produces the same blank-line separation
// the prior format used. Any literal occurrence of the open/close fence inside
// the content is escaped so the envelope cannot be terminated early.
func writeUntrusted(b *strings.Builder, content, openFence, closeFence string) {
	b.WriteString(openFence)
	b.WriteString("\n")
	safe := content
	if strings.Contains(safe, openFence) {
		safe = strings.ReplaceAll(safe, openFence, openFence+"_ESCAPED")
	}
	if strings.Contains(safe, closeFence) {
		safe = strings.ReplaceAll(safe, closeFence, "ESCAPED_"+closeFence)
	}
	b.WriteString(safe)
	if !strings.HasSuffix(safe, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(closeFence)
	b.WriteString("\n")
}

// deriveNonce returns a short, deterministic hex token derived from all
// untrusted fields. Deterministic so Build stays a pure function (golden test),
// content-derived so the fence markers are not guessable from the operator
// constraints alone — an injector cannot know which token will fence its own
// content.
func deriveNonce(in Inputs) string {
	h := sha256.New()
	for _, s := range []string{in.Task, in.Acceptance, in.Conventions} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	for _, f := range in.FilesIncluded {
		h.Write([]byte(f.Path))
		h.Write([]byte{0})
		h.Write([]byte(f.Content))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// DetectConventions walks upward from start looking for CLAUDE.md, then
// AGENTS.md, then .cursor/rules. Returns the absolute path of the first hit,
// or "" if none.
func DetectConventions(start string) string {
	dir, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for {
		for _, name := range []string{"CLAUDE.md", "AGENTS.md", ".cursor/rules"} {
			candidate := filepath.Join(dir, name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// ReadConventions returns the textual content of a conventions path. For a
// file, it's the file content; for a directory (.cursor/rules), all regular
// files are concatenated in lexical order, each separated by a labeled boundary
// so two rule files are never glued together with no delimiter. Nested rule
// files (e.g. .cursor/rules/<subdir>/*.mdc) are included as well, walked in
// lexical path order.
func ReadConventions(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		b, err := os.ReadFile(path)
		return string(b), err
	}

	// Collect every regular file under the directory (recursively) so a nested
	// .cursor/rules layout is honored, then concatenate in lexical path order
	// with an explicit per-file boundary between entries.
	var rels []string
	walkErr := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(path, p)
		if rerr != nil {
			return rerr
		}
		rels = append(rels, rel)
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	sort.Strings(rels)

	var b strings.Builder
	for i, rel := range rels {
		raw, err := os.ReadFile(filepath.Join(path, rel))
		if err != nil {
			return "", err
		}
		// Separate concatenated rule files with a labeled boundary so the
		// content of two files is never glued together with no delimiter.
		if i > 0 && b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
		b.WriteString("# --- ")
		b.WriteString(filepath.ToSlash(rel))
		b.WriteString(" ---\n")
		b.Write(raw)
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			b.WriteString("\n")
		}
	}
	return b.String(), nil
}
