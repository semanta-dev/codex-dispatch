//go:build !cgo && !darwin && !windows

package authority

import (
	"strings"
	"testing"
)

func TestStaticAccountLookupRejectsMissingUIDWithoutEnvironmentFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "looks-valid")
	if _, err := accountHomeFromPasswd(strings.NewReader("root:x:0:0:root:/root:/bin/sh\n"), 61000); err == nil {
		t.Fatal("trusted HOME for missing UID")
	}
	home, err := accountHomeFromPasswd(strings.NewReader("real:x:61000:61000:user:/account/home:/bin/sh\n"), 61000)
	if err != nil || home != "/account/home" {
		t.Fatalf("did not use matching account: %q %v", home, err)
	}
}
