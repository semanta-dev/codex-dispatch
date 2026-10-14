package authority

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func binding() Binding {
	return Binding{Version: 2, RunID: strings.Repeat("a", 32), Head: strings.Repeat("b", 40), BaselineTree: strings.Repeat("c", 40), IndexDigest: strings.Repeat("d", 64), IndexObjectsDigest: strings.Repeat("f", 64), CandidateHash: strings.Repeat("e", 64), Repository: filepath.Join(os.TempDir(), "repo"), Workdir: filepath.Join(os.TempDir(), "repo"), ExportDir: filepath.Join(os.TempDir(), "run"), TerminalState: "BASELINE_CAPTURED"}
}

func TestTemporaryRootCannotGrantAuthorityAccess(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "authority")
	child := filepath.Join(root, "nested")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	for _, temporary := range []string{parent, root, child} {
		if err := rejectTemporaryOverlap(root, temporary); err == nil {
			t.Fatalf("accepted overlapping temporary root %s", temporary)
		}
	}
	sibling := t.TempDir()
	if err := rejectTemporaryOverlap(root, sibling); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		alias := filepath.Join(sibling, "alias")
		if err := os.Symlink(child, alias); err != nil {
			t.Fatal(err)
		}
		if err := rejectTemporaryOverlap(root, alias); err == nil {
			t.Fatal("accepted symlink alias to authority")
		}
	}
}

// Reproduces the macOS /var -> /private/var condition on any platform: when the
// authority root itself is reached through a symlinked ancestor, the overlap must
// still be detected. Resolving only the temporary side accepted a real overlap.
func TestSymlinkedAncestorStillDetectsOverlap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX ancestor symlink semantics")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	root := filepath.Join(real, "authority")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	// Reach the same authority root through a symlinked ancestor, the way macOS
	// reaches every temporary directory through /var.
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	aliased := filepath.Join(link, "authority")
	for _, temporary := range []string{root, aliased, filepath.Join(aliased, "nested")} {
		if err := os.MkdirAll(temporary, 0700); err != nil {
			t.Fatal(err)
		}
		if err := rejectTemporaryOverlap(aliased, temporary); err == nil {
			t.Fatalf("accepted overlapping temporary root %s through symlinked ancestor", temporary)
		}
		if err := rejectTemporaryOverlap(root, temporary); err == nil {
			t.Fatalf("accepted overlapping temporary root %s against real root", temporary)
		}
	}
	// A disjoint sibling must still be accepted through the same alias.
	sibling := filepath.Join(base, "sibling")
	if err := os.MkdirAll(sibling, 0700); err != nil {
		t.Fatal(err)
	}
	if err := rejectTemporaryOverlap(aliased, sibling); err != nil {
		t.Fatalf("rejected disjoint temporary root: %v", err)
	}
}

func TestAccountStoreRejectsAuthorityTMPDIR(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows temporary root uses native environment precedence")
	}
	root, err := BaseDir()
	if err != nil {
		t.Fatal(err)
	}
	// Let Store create/check its root before evaluating the overlapping temp path.
	t.Setenv("TMPDIR", root)
	if _, err := Store(""); err == nil || !strings.Contains(err.Error(), "overlaps private authority") {
		t.Fatalf("unsafe account store result: %v", err)
	}
}

func TestPublishReadCreateOnly(t *testing.T) {
	dir, err := Store(filepath.Join(t.TempDir(), "authority"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(dir, binding()); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil || got != binding() {
		t.Fatalf("got %+v: %v", got, err)
	}
	if err := Publish(dir, binding()); err == nil {
		t.Fatal("expected duplicate rejection")
	}
}

func TestReadRejectsSymlink(t *testing.T) {
	dir, err := Store(filepath.Join(t.TempDir(), "authority"))
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "authority")
	if err := os.WriteFile(outside, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "authority.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestStoreRejectsPreexistingSymlinkAndBroadPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode checks; Windows uses DACLs")
	}
	base := t.TempDir()
	broad := filepath.Join(base, "broad")
	if err := os.Mkdir(broad, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Store(broad); err == nil {
		t.Fatal("accepted shared authority root")
	}
	private := filepath.Join(base, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Store(link); err == nil {
		t.Fatal("followed substituted authority root")
	}
}

func TestConcurrentPublicationHasOneCompleteWinner(t *testing.T) {
	dir, err := Store(filepath.Join(t.TempDir(), "authority"))
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 16)
	for i := 0; i < cap(results); i++ {
		go func() { results <- Publish(dir, binding()) }()
	}
	wins := 0
	for i := 0; i < cap(results); i++ {
		if <-results == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("got %d successful immutable publications", wins)
	}
	got, err := Read(dir)
	if err != nil || got != binding() {
		t.Fatalf("winner was partial or inconsistent: %v %+v", err, got)
	}
}

func TestBindingRejectsPlaceholderAndPhaseConfusion(t *testing.T) {
	b := binding()
	b.CandidateHash = "pending"
	if err := b.Validate(); err == nil {
		t.Fatal("accepted placeholder fingerprint")
	}
	b = binding()
	b.PatchDigest = strings.Repeat("a", 64)
	if err := b.Validate(); err == nil {
		t.Fatal("accepted terminal patch on baseline")
	}
	b = binding()
	b.TerminalState = "DIFF_CAPTURED"
	if err := b.Validate(); err == nil {
		t.Fatal("accepted unsealed capture")
	}
}
