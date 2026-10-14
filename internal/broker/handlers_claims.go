package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// claimStoreKey is the context key used to inject the ClaimStore into handlers.
type claimStoreKey struct{}

// WithClaimStore injects a ClaimStore into a handler context.
func WithClaimStore(ctx context.Context, s *ClaimStore) context.Context {
	return context.WithValue(ctx, claimStoreKey{}, s)
}

func claimStoreFrom(ctx context.Context) *ClaimStore {
	s, _ := ctx.Value(claimStoreKey{}).(*ClaimStore)
	return s
}

// HandleClaimAcquire returns a Handler for claim.acquire.
func HandleClaimAcquire(s *ClaimStore) Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Owner         string   `json:"owner"`
			Files         []string `json:"files"`
			Intent        string   `json:"intent"`
			TTLMs         int64    `json:"ttl_ms,omitempty"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		ttl := time.Duration(p.TTLMs) * time.Millisecond
		claim, conflicts, err := s.Acquire(p.Owner, p.Files, p.Intent, ttl)
		if err != nil { return nil, err }
		if len(conflicts) > 0 {
			return map[string]any{"claimed": false, "conflicts": conflicts}, nil
		}
		return map[string]any{"claimed": true, "claim": claim}, nil
	}
}

// HandleClaimRelease returns a Handler for claim.release.
func HandleClaimRelease(s *ClaimStore) Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ClaimID       string   `json:"claim_id"`
			Owner         string   `json:"owner"`
			FencingToken  int64    `json:"fencing_token"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if err := s.Release(p.ClaimID, p.Owner, p.FencingToken); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
}

// HandleClaimRenew returns a Handler for claim.renew.
func HandleClaimRenew(s *ClaimStore) Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ClaimID       string   `json:"claim_id"`
			Owner         string   `json:"owner"`
			FencingToken  int64    `json:"fencing_token"`
			TTLMs         int64    `json:"ttl_ms,omitempty"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		ttl := time.Duration(p.TTLMs) * time.Millisecond
		claim, err := s.Renew(p.ClaimID, p.Owner, p.FencingToken, ttl)
		if err != nil { return nil, err }
		return map[string]any{"claim": claim}, nil
	}
}

// HandleClaimList returns a Handler for claim.list.
func HandleClaimList(s *ClaimStore) Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Owner string `json:"owner,omitempty"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, fmt.Errorf("invalid params: %w", err)
			}
		}
		claims := s.List(p.Owner)
		if claims == nil { claims = []Claim{} }
		return map[string]any{"claims": claims}, nil
	}
}
