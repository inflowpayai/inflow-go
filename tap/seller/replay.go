package seller

import (
	"context"
	"sync"
	"time"
)

type claimKey struct{ keyID, nonce string }

// MemoryReplayStore is process-local. Multiple instances need a shared atomic
// ReplayStore. Expired claims can be discarded; this is not payment idempotency.
type MemoryReplayStore struct {
	mu     sync.Mutex
	claims map[claimKey]int64
	clock  func() time.Time
}

func NewMemoryReplayStore(clock func() time.Time) *MemoryReplayStore {
	if clock == nil {
		clock = time.Now
	}
	return &MemoryReplayStore{claims: make(map[claimKey]int64), clock: clock}
}

func (s *MemoryReplayStore) Claim(ctx context.Context, keyID, nonce string, expires int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	now := s.clock().Unix()
	for key, expiry := range s.claims {
		if expiry <= now {
			delete(s.claims, key)
		}
	}
	key := claimKey{keyID, nonce}
	if _, exists := s.claims[key]; exists {
		return false, nil
	}
	s.claims[key] = expires
	return true, nil
}
