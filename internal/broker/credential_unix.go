//go:build !windows

package broker

import "os"

func createCredentialTemp(dir, prefix string) (*os.File, error) {
	return os.CreateTemp(dir, prefix+"*")
}
