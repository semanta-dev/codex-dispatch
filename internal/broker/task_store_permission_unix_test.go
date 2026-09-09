//go:build !windows

package broker

import (
	"os"
	"testing"
)

func makeTaskStoreReadOnly(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	probe, err := os.CreateTemp(dir, "probe")
	if err == nil {
		probe.Close()
		os.Remove(probe.Name())
		t.Skip("requires unprivileged filesystem permissions")
	}
}
