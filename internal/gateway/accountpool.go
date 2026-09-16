// Package gateway implements the data plane: routing resolution, account
// pooling, failover, and the passthrough/converted relay of both protocol
// surfaces.
package gateway

import (
	"errors"
	"sync"
	"time"

	"github.com/PWZER/llm-switch/internal/engine"
)

// Outcome classifies an upstream attempt for account-pool bookkeeping.
type Outcome int

const (
	OutcomeOK          Outcome = iota // 2xx
	OutcomeRateLimited                // 429: honor Retry-After or an exponential base
	OutcomeAuthError                  // 401/403: short penalty; surfaced in the UI
	OutcomeServerError                // 5xx/network: brief penalty, prefer next candidate
)

// ErrNoAccounts means the provider has no usable (enabled) accounts at all.
var ErrNoAccounts = errors.New("provider has no enabled accounts")

// accountState is the mutable rotation state of one account. Lives outside
// the routing snapshot on purpose: admin edits must not reset rotation or cooldowns.
type accountState struct {
	current      int       // smooth weighted round-robin current weight
	cooldownDone time.Time // earliest moment the account may be retried
	cooldownBase time.Duration
}

// AccountPool tracks per-account rotation and cooldown for all providers.
type AccountPool struct {
	mu     sync.Mutex
	states map[int64]*accountState
	now    func() time.Time
}

// NewAccountPool creates an empty pool.
func NewAccountPool() *AccountPool {
	return &AccountPool{states: map[int64]*accountState{}, now: time.Now}
}

func (p *AccountPool) state(id int64) *accountState {
	st := p.states[id]
	if st == nil {
		st = &accountState{}
		p.states[id] = st
	}
	return st
}

// Pick selects the next usable account of a provider using smooth weighted
// round-robin (nginx algorithm), skipping cooled-down accounts. When every
// account is cooling, the one with the earliest cooldown is returned so
// failover proceeds.
func (p *AccountPool) Pick(provider *engine.Provider) (engine.Account, error) {
	if len(provider.Accounts) == 0 {
		return engine.Account{}, ErrNoAccounts
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()

	var ready []engine.Account
	var fallback engine.Account
	var earliest time.Time
	for _, a := range provider.Accounts {
		w := a.Weight
		if w < 1 {
			w = 1
		}
		a.Weight = w
		done := p.state(a.ID).cooldownDone
		if done.After(now) {
			if earliest.IsZero() || done.Before(earliest) {
				earliest, fallback = done, a
			}
			continue
		}
		ready = append(ready, a)
	}
	if len(ready) == 0 {
		return fallback, nil
	}

	total := 0
	var best engine.Account
	var bestSt *accountState
	for _, a := range ready {
		st := p.state(a.ID)
		st.current += a.Weight
		total += a.Weight
		if bestSt == nil || st.current > bestSt.current {
			best, bestSt = a, st
		}
	}
	bestSt.current -= total
	return best, nil
}

// Report records the outcome of an attempt on an account, applying cooldowns.
func (p *AccountPool) Report(accountID int64, outcome Outcome, retryAfter time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state(accountID)
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

// Cooling reports whether an account is currently in cooldown.
func (p *AccountPool) Cooling(accountID int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[accountID]
	return st != nil && st.cooldownDone.After(p.now())
}

// Cooldowns returns accounts currently cooling (accountID -> until), for UI display.
func (p *AccountPool) Cooldowns() map[int64]time.Time {
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
