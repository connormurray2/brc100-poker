// Package health reports whether the service is running and whether it can serve play.
//
// The distinction matters operationally: a process that is alive but cannot settle must not
// receive traffic, and an orchestrator can only act on that if the two are reported separately.
package health

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// State is a dependency's condition.
type State string

const (
	// StateUp means the dependency is reachable and working.
	StateUp State = "up"
	// StateDown means it is not.
	StateDown State = "down"
	// StateUnknown means it has not been checked yet. Distinct from down: at startup
	// "not yet checked" must not read as "broken".
	StateUnknown State = "unknown"
)

// Dependency is one external or internal dependency.
type Dependency struct {
	Name  string `json:"name"`
	State State  `json:"state"`
	// Detail explains a non-up state in operator terms.
	Detail string `json:"detail,omitempty"`
	// Required marks a dependency without which play cannot proceed.
	Required  bool      `json:"required"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
}

// Well-known dependency names.
const (
	// DepDatabase is the wallet and ledger store. Losing it loses the coins.
	DepDatabase = "database"
	// DepOracle is the transaction oracle: the only broadcast target and the only
	// double-spend adjudicator. Without it a hand cannot settle.
	DepOracle = "oracle"
	// DepHeaders is the block-header service, needed to verify merkle proofs locally.
	DepHeaders = "headers"
	// DepStatusTracking is the monitor daemon. Without it transactions never receive a
	// status at all, so it is required rather than advisory.
	DepStatusTracking = "statusTracking"
)

// Reporter tracks dependency state and answers health probes.
type Reporter struct {
	mu   sync.RWMutex
	deps map[string]Dependency
	// realValuePlay records whether the operator has enabled hands that move real coins.
	realValuePlay bool
	// backupVerified records whether a database restore has been proved.
	backupVerified bool
}

// New builds a reporter with the required dependencies registered as unknown.
//
// Registering them up front means a probe before the first check reports "unknown" rather than
// omitting the dependency, so a missing check is visible instead of invisible.
func New(realValuePlay, backupVerified bool) *Reporter {
	r := &Reporter{
		deps:           make(map[string]Dependency),
		realValuePlay:  realValuePlay,
		backupVerified: backupVerified,
	}
	for _, name := range []string{DepDatabase, DepOracle, DepHeaders, DepStatusTracking} {
		r.deps[name] = Dependency{Name: name, State: StateUnknown, Required: true}
	}
	return r
}

// Set records a dependency's state.
func (r *Reporter) Set(name string, state State, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.deps[name]
	if !ok {
		d = Dependency{Name: name}
	}
	d.State = state
	d.Detail = detail
	d.CheckedAt = time.Now().UTC()
	r.deps[name] = d
}

// SetOptional registers a dependency whose absence degrades service without stopping play.
func (r *Reporter) SetOptional(name string, state State, detail string) {
	r.mu.Lock()
	if _, ok := r.deps[name]; !ok {
		r.deps[name] = Dependency{Name: name, Required: false}
	}
	r.mu.Unlock()
	r.Set(name, state, detail)
}

// Snapshot returns the current dependencies, sorted by name for stable output.
func (r *Reporter) Snapshot() []Dependency {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Dependency, 0, len(r.deps))
	for _, d := range r.deps {
		out = append(out, d)
	}
	// Deterministic order so a diff between two probes is readable.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Name < out[j-1].Name; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Ready reports whether the service can serve play, and why not if it cannot.
//
// A required dependency that is unknown counts as not ready: the service must prove it can
// work rather than assume it until told otherwise.
func (r *Reporter) Ready() (bool, []string) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var reasons []string
	for _, d := range r.deps {
		if !d.Required {
			continue
		}
		switch d.State {
		case StateUp:
		case StateDown:
			reasons = append(reasons, fmt.Sprintf("%s is down: %s", d.Name, d.Detail))
		default:
			reasons = append(reasons, fmt.Sprintf("%s has not been checked", d.Name))
		}
	}
	return len(reasons) == 0, reasons
}

// RealValueReady reports whether a hand may move real coins.
//
// Stricter than Ready: real value additionally requires that a database restore has been
// proved, because an unrestorable wallet does not merely lose availability — it loses the coins.
func (r *Reporter) RealValueReady() (bool, []string) {
	ok, reasons := r.Ready()
	r.mu.RLock()
	defer r.mu.RUnlock()

	if !r.realValuePlay {
		reasons = append(reasons, "real-value play is not enabled")
		ok = false
	}
	if !r.backupVerified {
		reasons = append(reasons, "no database restore has been proved: an unrestorable wallet loses the coins, not just availability")
		ok = false
	}
	return ok, reasons
}

// LivenessHandler reports that the process is running.
//
// Deliberately does not consult dependencies: an orchestrator restarting a process because a
// remote oracle is unreachable would make an outage worse, not better.
func (r *Reporter) LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "alive"})
	}
}

// readinessResponse is the readiness probe's body.
type readinessResponse struct {
	Ready           bool         `json:"ready"`
	RealValueReady  bool         `json:"realValueReady"`
	Reasons         []string     `json:"reasons,omitempty"`
	RealValueBlocks []string     `json:"realValueBlockedBy,omitempty"`
	Dependencies    []Dependency `json:"dependencies"`
}

// ReadinessHandler reports whether the service can serve play.
func (r *Reporter) ReadinessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		ready, reasons := r.Ready()
		rvReady, rvReasons := r.RealValueReady()

		body := readinessResponse{
			Ready:           ready,
			RealValueReady:  rvReady,
			Reasons:         reasons,
			RealValueBlocks: rvReasons,
			Dependencies:    r.Snapshot(),
		}

		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}
