package artifact

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ReadRegularAt reads bounded bytes through a pinned directory. It rejects
// symlink ancestors and compares the inspected and opened file identities.
func ReadRegularAt(root *os.Root, name string, limit int64) ([]byte, error) {
	if !filepath.IsLocal(name) {
		return nil, fmt.Errorf("unsafe artifact path")
	}
	for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
		info, err := root.Lstat(parent)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("non-directory artifact ancestor")
		}
	}
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact is not regular")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|nonblockingRead, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("artifact changed while opening")
	}
	if opened.Size() > limit {
		return nil, fmt.Errorf("artifact exceeds byte quota")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("artifact grew beyond byte quota")
	}
	return data, nil
}
