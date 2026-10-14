package broker

import (
	"testing"
	"time"
)

func TestClaimAcquireRelease(t *testing.T) {
	s := NewClaimStore()
	c, conflicts, err := s.Acquire("agent-1", []string{"src/main.go", "src/util.go"}, "implement feature", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) > 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}
	if c.FencingToken != 1 {
		t.Fatalf("first token = %d, want 1", c.FencingToken)
	}
	if len(c.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(c.Files))
	}

	if err := s.Release(c.ID, "agent-1", c.FencingToken); err != nil {
		t.Fatal(err)
	}
	claims := s.List("")
	if len(claims) != 0 {
		t.Fatalf("claims after release = %d, want 0", len(claims))
	}
}

func TestClaimConflict(t *testing.T) {
	s := NewClaimStore()
	c1, _, err := s.Acquire("agent-1", []string{"src/shared.go"}, "first", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	_, conflicts, err := s.Acquire("agent-2", []string{"src/shared.go"}, "second", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %d, want 1", len(conflicts))
	}
	if conflicts[0].HolderOwner != "agent-1" {
		t.Fatalf("holder = %s, want agent-1", conflicts[0].HolderOwner)
	}
	if conflicts[0].Path != "src/shared.go" {
		t.Fatal("wrong conflict path")
	}

	// Release and retry should succeed.
	if err := s.Release(c1.ID, "agent-1", c1.FencingToken); err != nil {
		t.Fatal(err)
	}
	c2, _, err := s.Acquire("agent-2", []string{"src/shared.go"}, "second", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if c2 == nil {
		t.Fatal("claim should succeed after release")
	}
}

func TestClaimFencingTokenMonotonic(t *testing.T) {
	s := NewClaimStore()
	c1, _, err := s.Acquire("agent-1", []string{"f.go"}, "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Try to release with a wrong token.
	if err := s.Release(c1.ID, "agent-1", c1.FencingToken+999); err == nil {
		t.Fatal("stale token release should fail")
	}

	// Proper release, re-acquire: token increments.
	s.Release(c1.ID, "agent-1", c1.FencingToken)
	c2, _, err := s.Acquire("agent-2", []string{"f.go"}, "b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if c2.FencingToken <= c1.FencingToken {
		t.Fatalf("token not monotonic: %d <= %d", c2.FencingToken, c1.FencingToken)
	}
}

func TestClaimExpiry(t *testing.T) {
	s := NewClaimStore()
	base := time.Now().UTC()
	current := base
	s.setClock(func() time.Time { return current })

	c, _, err := s.Acquire("agent-1", []string{"f.go"}, "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Advance past TTL.
	current = base.Add(2 * time.Minute)

	// Next acquire should succeed because the old claim expired.
	c2, _, err := s.Acquire("agent-2", []string{"f.go"}, "b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if c2 == nil {
		t.Fatal("should succeed after expiry")
	}
	if c2.FencingToken <= c.FencingToken {
		t.Fatal("token should increment after expiry steal")
	}

	// Releasing the expired claim should fail as not found.
	if err := s.Release(c.ID, "agent-1", c.FencingToken); err != ErrClaimNotFound {
		t.Fatalf("expected ErrClaimNotFound, got %v", err)
	}
}

func TestClaimRenewal(t *testing.T) {
	s := NewClaimStore()
	c1, _, err := s.Acquire("agent-1", []string{"a.go"}, "first", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Same owner claims overlapping path: renewal, same fencing token.
	c2, _, err := s.Acquire("agent-1", []string{"a.go", "b.go"}, "second", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if c2.ID != c1.ID {
		t.Fatal("renewal should return same claim ID")
	}
	if c2.FencingToken != c1.FencingToken {
		t.Fatal("renewal should preserve fencing token")
	}
	if len(c2.Files) != 2 {
		t.Fatalf("files after renewal = %d, want 2", len(c2.Files))
	}

	// A newly added path must be indexed too, or another agent could claim it.
	if _, conflicts, err := s.Acquire("agent-2", []string{"b.go"}, "other", time.Minute); err != nil {
		t.Fatal(err)
	} else if len(conflicts) != 1 || conflicts[0].Path != "b.go" {
		t.Fatalf("renewed path conflict = %#v, want b.go conflict", conflicts)
	}
}

func TestClaimValidateForTask(t *testing.T) {
	s := NewClaimStore()
	c, _, err := s.Acquire("agent-1", []string{"a.go"}, "work", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Valid claim for right owner.
	if err := s.ValidateForTask("agent-1", []string{c.ID}); err != nil {
		t.Fatal(err)
	}

	// Wrong owner.
	if err := s.ValidateForTask("agent-2", []string{c.ID}); err == nil {
		t.Fatal("wrong owner should fail")
	}

	// Unknown claim.
	if err := s.ValidateForTask("agent-1", []string{"c_unknown"}); err == nil {
		t.Fatal("unknown claim should fail")
	}

	// Empty list always passes.
	if err := s.ValidateForTask("agent-1", nil); err != nil {
		t.Fatal(err)
	}
}

func TestClaimPathNormalisation(t *testing.T) {
	s := NewClaimStore()
	// Absolute path rejected.
	if _, _, err := s.Acquire("a", []string{"/abs/path.go"}, "i", time.Minute); err == nil {
		t.Fatal("absolute path should fail")
	}
	// Traversal rejected.
	if _, _, err := s.Acquire("a", []string{"../escape.go"}, "i", time.Minute); err == nil {
		t.Fatal("traversal should fail")
	}

	// Same file with ./ prefix and no prefix should be the same claim.
	c1, _, err := s.Acquire("a", []string{"./src/f.go"}, "i", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if c1.Files[0] != "src/f.go" {
		t.Fatalf("normalised = %s, want src/f.go", c1.Files[0])
	}
}
