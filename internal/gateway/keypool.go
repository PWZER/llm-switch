// Package gateway implements the data plane: routing resolution, key pooling,
// failover, and the passthrough/converted relay of both protocol surfaces.
package gateway

import (
	"errors"
	"sync"
	"time"

	"github.com/PWZER/llm-switch/internal/engine"
)

// Outcome classifies an upstream attempt for key-pool bookkeeping.
type Outcome int

const (
	OutcomeOK          Outcome = iota // 2xx
	OutcomeRateLimited                // 429: honor Retry-After or an exponential base
	OutcomeAuthError                  // 401/403: short penalty; surfaced in the UI
	OutcomeServerError                // 5xx/network: brief penalty, prefer next candidate
)

// ErrNoKeys means the provider has no usable (enabled) keys at all.
var ErrNoKeys = errors.New("provider has no enabled api keys")

// keyState is the mutable rotation state of one provider key. Lives outside
// the routing snapshot on purpose: admin edits must not reset rotation or cooldowns.
type keyState struct {
	current      int       // smooth weighted round-robin current weight
	cooldownDone time.Time // earliest moment the key may be retried
	cooldownBase time.Duration
}

// KeyPool tracks per-key rotation and cooldown for all providers.
type KeyPool struct {
	mu     sync.Mutex
	states map[int64]*keyState
	now    func() time.Time
}

// NewKeyPool creates an empty pool.
func NewKeyPool() *KeyPool {
	return &KeyPool{states: map[int64]*keyState{}, now: time.Now}
}

func (p *KeyPool) state(id int64) *keyState {
	st := p.states[id]
	if st == nil {
		st = &keyState{}
		p.states[id] = st
	}
	return st
}

// Pick selects the next usable key of a provider using smooth weighted
// round-robin (nginx algorithm), skipping cooled-down keys. When every key is
// cooling, the one with the earliest cooldown is returned so failover proceeds.
func (p *KeyPool) Pick(provider *engine.Provider) (engine.Key, error) {
	if len(provider.Keys) == 0 {
		return engine.Key{}, ErrNoKeys
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()

	var ready []engine.Key
	var fallback engine.Key
	var earliest time.Time
	for _, k := range provider.Keys {
		w := k.Weight
		if w < 1 {
			w = 1
		}
		k.Weight = w
		done := p.state(k.ID).cooldownDone
		if done.After(now) {
			if earliest.IsZero() || done.Before(earliest) {
				earliest, fallback = done, k
			}
			continue
		}
		ready = append(ready, k)
	}
	if len(ready) == 0 {
		return fallback, nil
	}

	total := 0
	var best engine.Key
	var bestSt *keyState
	for _, k := range ready {
		st := p.state(k.ID)
		st.current += k.Weight
		total += k.Weight
		if bestSt == nil || st.current > bestSt.current {
			best, bestSt = k, st
		}
	}
	bestSt.current -= total
	return best, nil
}

// Report records the outcome of an attempt on a key, applying cooldowns.
func (p *KeyPool) Report(keyID int64, outcome Outcome, retryAfter time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state(keyID)
	now := p.now()
	switch outcome {
	case OutcomeRateLimited:
		base := st.cooldownBase
		if base == 0 {
			base = 30 * time.Second
		}
		cd := retryAfter
		if cd <= 0 {
			cd = base
			next := base * 2
			if next > 5*time.Minute {
				next = 5 * time.Minute
			}
			st.cooldownBase = next
		}
		if cd > 5*time.Minute {
			cd = 5 * time.Minute
		}
		st.cooldownDone = now.Add(cd)
	case OutcomeAuthError:
		// No long automatic cooldown by design (bad key is surfaced in the UI);
		// a short penalty avoids hammering a revoked key inside one failover batch.
		st.cooldownDone = now.Add(10 * time.Second)
	case OutcomeServerError:
		st.cooldownDone = now.Add(2 * time.Second)
	case OutcomeOK:
		st.cooldownBase = 0
		st.cooldownDone = time.Time{}
	}
}

// Cooldowns returns keys currently cooling (keyID -> until), for UI display.
func (p *KeyPool) Cooldowns() map[int64]time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[int64]time.Time{}
	now := p.now()
	for id, st := range p.states {
		if st.cooldownDone.After(now) {
			out[id] = st.cooldownDone
		}
	}
	return out
}
