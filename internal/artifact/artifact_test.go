package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAtomicReplacesSymlinkWithoutFollowing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "result.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := WriteAtomic(dir, "result.json", []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "original" {
		t.Fatalf("external target changed: %q (%v)", got, err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil || string(got) != "replacement" {
		t.Fatalf("replacement missing: %q (%v)", got, err)
	}
}

func TestWriteAtomicRejectsPathComponents(t *testing.T) {
	if err := WriteAtomic(t.TempDir(), "../escape", nil, 0o600); err == nil {
		t.Fatal("expected invalid artifact name")
	}
}
