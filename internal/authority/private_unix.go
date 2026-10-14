//go:build !windows

package authority

import (
	"fmt"
	"os"
	"syscall"
)

const nonblock = syscall.O_NONBLOCK

func mkdirPrivate(path string) error { return os.Mkdir(path, 0700) }
func checkPrivateRoot(root *os.Root) error {
	info, err := root.Stat(".")
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("authority directory must be owned by current user and mode 0700")
	}
	return nil
}
func syncRoot(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
