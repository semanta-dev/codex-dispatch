//go:build cgo || darwin || windows

package authority

import (
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strconv"
)

func accountHome() (string, error) {
	var account *user.User
	var err error
	if runtime.GOOS == "windows" {
		account, err = user.Current()
	} else {
		account, err = user.LookupId(strconv.Itoa(os.Geteuid()))
	}
	if err != nil {
		return "", err
	}
	if account.HomeDir == "" {
		return "", fmt.Errorf("account database has no controller home")
	}
	return account.HomeDir, nil
}
