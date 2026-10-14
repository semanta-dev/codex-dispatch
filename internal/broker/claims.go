package broker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Claim is a broker-enforced advisory lease on a set of file paths. A claim
// grants its holder the right to dispatch tasks that declare those paths, and
// blocks other agents from acquiring the same paths until the claim is released
// or expires. Each claim carries a fencing token that monotonically increases
// per path: a stale holder releasing with an old token fails instead of
// silently clobbering the current holder's work.
type Claim struct {
	ID           string    `json:"id"`
	Owner        string    `json:"owner"`
	Files        []string  `json:"files"`
	Intent       string    `json:"intent"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	FencingToken int64     `json:"fencing_token"`
}

// ClaimConflict describes a path already held by another active claim.
type ClaimConflict struct {
	Path         string `json:"path"`
	HolderID     string `json:"holder_id"`
	HolderOwner  string `json:"holder_owner"`
	HolderIntent string `json:"holder_intent"`
}

// Sentinel errors.
var (
	ErrClaimNotFound   = fmt.Errorf("claim not found")
	ErrClaimForbidden  = fmt.Errorf("claim held by another owner")
	ErrClaimStaleToken = fmt.Errorf("claim fencing token is stale")
)

// ClaimStore manages broker-enforced claims with fencing tokens and TTLs.
// Claims are advisory within this broker's lifetime: they do not physically lock
// files, but task admission checks them when the caller provides a claim_ids
// list. Expired claims are lazily reaped on the next mutation.
type ClaimStore struct {
	mu        sync.Mutex
	claims    map[string]*Claim
	byPath    map[string]string // normalised path -> claim ID
	nextFence int64
	now       func() time.Time
}

// NewClaimStore constructs an empty store with fencing tokens starting at 1.
func NewClaimStore() *ClaimStore {
	return &ClaimStore{
		claims:    map[string]*Claim{},
		byPath:    map[string]string{},
		nextFence: 1,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// setClock overrides the TTL clock for deterministic tests.
func (s *ClaimStore) setClock(now func() time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

// Acquire creates a claim on the given files. Returns the claim on success, or
// a list of conflicts if any path is already held by a non-expired claim owned
// by a different owner. Re-claiming your own paths extends the claim (renewal
// semantics), keeping the same fencing token.
func (s *ClaimStore) Acquire(owner string, files []string, intent string, ttl time.Duration) (*Claim, []ClaimConflict, error) {
	if owner == "" {
		return nil, nil, fmt.Errorf("claim owner required")
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("claim files required")
	}
	if intent == "" {
		return nil, nil, fmt.Errorf("claim intent required")
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}

	normalised := make([]string, 0, len(files))
	seen := map[string]struct{}{}
	for _, f := range files {
		f = normaliseClaimPath(f)
		if f == "" {
			return nil, nil, fmt.Errorf("empty claim file path")
		}
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		normalised = append(normalised, f)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()

	// Check conflicts.
	var conflicts []ClaimConflict
	for _, f := range normalised {
		holderID, held := s.byPath[f]
		if !held {
			continue
		}
		holder := s.claims[holderID]
		if holder == nil {
			// Stale index entry; clean up.
			delete(s.byPath, f)
			continue
		}
		if holder.Owner == owner {
			continue // self-renewal, not a conflict
		}
		conflicts = append(conflicts, ClaimConflict{
			Path: f, HolderID: holder.ID, HolderOwner: holder.Owner, HolderIntent: holder.Intent,
		})
	}
	if len(conflicts) > 0 {
		return nil, conflicts, nil
	}

	// Check whether any path is already ours (renewal path).
	existingID := ""
	for _, f := range normalised {
		if id, ok := s.byPath[f]; ok {
			if c := s.claims[id]; c != nil && c.Owner == owner {
				existingID = c.ID
				break
			}
		}
	}

	now := s.now()
	if existingID != "" {
		// Renewal: extend expiry and files, keep fencing token.
		c := s.claims[existingID]
		c.Files = mergeUnique(c.Files, normalised)
		for _, f := range normalised {
			if _, ok := s.byPath[f]; !ok {
				s.byPath[f] = c.ID
			}
		}
		c.Intent = intent
		c.ExpiresAt = now.Add(ttl)
		return c, nil, nil
	}

	// Fresh claim.
	id := newClaimID()
	token := atomic.AddInt64(&s.nextFence, 1) - 1
	c := &Claim{
		ID: id, Owner: owner, Files: normalised, Intent: intent,
		CreatedAt: now, ExpiresAt: now.Add(ttl), FencingToken: token,
	}
	s.claims[id] = c
	for _, f := range normalised {
		s.byPath[f] = id
	}
	return c, nil, nil
}

// Release removes a claim. The fencing token must match the current claim's
// token; a mismatch means the claim expired and was re-acquired by another
// agent, and the release is rejected as stale.
func (s *ClaimStore) Release(claimID string, owner string, fencingToken int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	c, ok := s.claims[claimID]
	if !ok {
		return ErrClaimNotFound
	}
	if c.Owner != owner {
		return fmt.Errorf("%w: claim held by %q, not %q", ErrClaimForbidden, c.Owner, owner)
	}
	if c.FencingToken != fencingToken {
		return fmt.Errorf("%w: claim %s fencing token %d does not match current %d", ErrClaimStaleToken, claimID, fencingToken, c.FencingToken)
	}
	for _, f := range c.Files {
		if s.byPath[f] == claimID {
			delete(s.byPath, f)
		}
	}
	delete(s.claims, claimID)
	return nil
}

// Renew extends a claim's expiry. Fencing token must match.
func (s *ClaimStore) Renew(claimID string, owner string, fencingToken int64, ttl time.Duration) (*Claim, error) {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	c, ok := s.claims[claimID]
	if !ok {
		return nil, ErrClaimNotFound
	}
	if c.Owner != owner {
		return nil, fmt.Errorf("%w: claim held by %q", ErrClaimForbidden, c.Owner)
	}
	if c.FencingToken != fencingToken {
		return nil, fmt.Errorf("%w: fencing token %d does not match current %d", ErrClaimStaleToken, fencingToken, c.FencingToken)
	}
	c.ExpiresAt = s.now().Add(ttl)
	return c, nil
}

// List returns all non-expired claims, optionally filtered by owner.
func (s *ClaimStore) List(owner string) []Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	var out []Claim
	for _, c := range s.claims {
		if owner != "" && c.Owner != owner {
			continue
		}
		out = append(out, *c)
	}
	return out
}

// ValidateForTask checks that the given claim IDs are held by the requesting
// session, are not expired, and still hold their fencing tokens. Used by task
// admission when the caller provides a claim_ids list.
func (s *ClaimStore) ValidateForTask(owner string, claimIDs []string) error {
	if len(claimIDs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	for _, id := range claimIDs {
		c, ok := s.claims[id]
		if !ok {
			return fmt.Errorf("%w: %s", ErrClaimNotFound, id)
		}
		if c.Owner != owner {
			return fmt.Errorf("%w: claim %s held by %q, not %q", ErrClaimForbidden, id, c.Owner, owner)
		}
	}
	return nil
}

// expireLocked removes expired claims and their path registrations.
func (s *ClaimStore) expireLocked() {
	now := s.now()
	for id, c := range s.claims {
		if now.After(c.ExpiresAt) {
			for _, f := range c.Files {
				if s.byPath[f] == id {
					delete(s.byPath, f)
				}
			}
			delete(s.claims, id)
		}
	}
}

// normaliseClaimPath cleans a file path for consistent claim matching. Rejects
// absolute paths and traversal: claims are repo-relative.
func normaliseClaimPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") || strings.Contains(p, "..") {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(p))
}

func mergeUnique(a, b []string) []string {
	seen := map[string]struct{}{}
	for _, s := range a {
		seen[s] = struct{}{}
	}
	out := a
	for _, s := range b {
		if _, dup := seen[s]; !dup {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

func newClaimID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("claim-%d", time.Now().UnixNano())
	}
	return "c_" + hex.EncodeToString(b[:])
}
