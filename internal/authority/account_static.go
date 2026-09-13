//go:build !cgo && !darwin && !windows

package authority

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// os/user.Current may fall back to USER/HOME in static builds. Read the local
// account record directly; an unresolvable account is unsupported, never an
// invitation to trust a caller-provided home directory.
func accountHome() (string, error) {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return "", err
	}
	defer f.Close()
	return accountHomeFromPasswd(f, os.Geteuid())
}
func accountHomeFromPasswd(reader io.Reader, uid int) (string, error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 7 || parts[2] != strconv.Itoa(uid) {
			continue
		}
		if !filepath.IsAbs(parts[5]) {
			return "", fmt.Errorf("account home must be absolute")
		}
		return parts[5], nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("authority requires an account-database home for effective uid %d; static builds require /etc/passwd entry", uid)
}
