package dispatch

import (
	"os"
	"path/filepath"
	"testing"
)

// makeGoworkRepo builds a minimal go.work monorepo in a temp dir: a root with
// go.work and modules shared/server/ui (each with go.mod), plus a nested
// non-module dir under server. Returns the root.
func makeGoworkRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.22\n"), 0o644); err != nil {
		t.Fatalf("write go.work: %v", err)
	}
	for _, m := range []string{"shared", "server", "ui"} {
		d := filepath.Join(root, m)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", m, err)
		}
		if err := os.WriteFile(filepath.Join(d, "go.mod"), []byte("module x/"+m+"\n\ngo 1.22\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
	}
	// A subdirectory inside server that is NOT its own module.
	if err := os.MkdirAll(filepath.Join(root, "server", "internal", "api"), 0o755); err != nil {
		t.Fatalf("mkdir server/internal/api: %v", err)
	}
	return root
}

func TestDeriveModuleDir(t *testing.T) {
	root := makeGoworkRepo(t)
	cases := []struct {
		name  string
		files string
		want  string // relative to root, or "" for no scoping
	}{
		{"single file in module", "server/server.go", "server"},
		{"new (nonexistent) file in module", "server/server_hello.go", "server"},
		{"deep file walks up to module", "server/internal/api/handler.go", "server"},
		{"multiple files same module", "server/a.go,server/internal/api/b.go", "server"},
		{"files span two modules → no scope", "server/a.go,shared/b.go", ""},
		{"file at repo root → no scope", "main.go", ""},
		{"empty files → no scope", "", ""},
		{"whitespace-only csv → no scope", " , ", ""},
		{"file outside repo → no scope", "../evil.go", ""},
		{"absolute file in module", filepath.Join(root, "ui", "ui.go"), "ui"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveModuleDir(root, tc.files)
			want := tc.want
			if want != "" {
				want = filepath.Join(root, want)
			}
			if got != want {
				t.Fatalf("DeriveModuleDir(root, %q) = %q, want %q", tc.files, got, want)
			}
		})
	}
}

// TestDeriveModuleDirSingleModuleRepo: a plain repo (manifest at root only) must
// never auto-scope — every file's owning module is the root.
func TestDeriveModuleDirSingleModuleRepo(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg", "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, files := range []string{"main.go", "pkg/sub/x.go", "pkg/a.go,pkg/sub/b.go"} {
		if got := DeriveModuleDir(root, files); got != "" {
			t.Fatalf("DeriveModuleDir(single-module, %q) = %q, want \"\"", files, got)
		}
	}
}

func TestSameDir(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "server")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !SameDir(root, root) {
		t.Fatal("SameDir(root, root) = false")
	}
	if !SameDir(root, filepath.Join(root, "server", "..")) {
		t.Fatal("SameDir did not normalize ..")
	}
	if SameDir(root, sub) {
		t.Fatal("SameDir(root, sub) = true")
	}
}
