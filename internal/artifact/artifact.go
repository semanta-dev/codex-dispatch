// Package artifact provides small, fail-closed primitives for controller-owned
// run artifacts. Callers must validate the containing directory before use.
package artifact

import (
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteAtomic publishes one leaf without following an existing symlink or
// truncating a hard-linked inode. The temporary file is exclusive and lives in
// the same directory, so publication is one filesystem rename.
func WriteAtomic(dir, name string, data []byte, perm os.FileMode) error {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return fmt.Errorf("invalid artifact name %q", name)
	}
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tmp := fmt.Sprintf(".%s.%x.tmp", name, suffix)
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = root.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		return err
	}
	ok = true
	return nil
}

// CopyAtomic copies a bounded reader into a private artifact using the same
// publication rules as WriteAtomic.
func CopyAtomic(dir, name string, src io.Reader, perm os.FileMode) error {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return fmt.Errorf("invalid artifact name %q", name)
	}
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tmp := fmt.Sprintf(".%s.%x.tmp", name, suffix)
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = root.Remove(tmp)
		}
	}()
	if _, err := io.Copy(f, src); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		return err
	}
	ok = true
	return nil
}
