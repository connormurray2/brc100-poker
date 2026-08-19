package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func allUp(r *Reporter) {
	for _, n := range []string{DepDatabase, DepOracle, DepHeaders, DepStatusTracking} {
		r.Set(n, StateUp, "")
	}
}

// An unchecked dependency must not read as healthy: the service proves it can work rather than
// assuming it until told otherwise.
func TestUncheckedDependenciesAreNotReady(t *testing.T) {
	r := New(false, false)
	ready, reasons := r.Ready()
	if ready {
		t.Fatal("a freshly built reporter claimed readiness with nothing checked")
	}
	if len(reasons) != 4 {
		t.Fatalf("got %d reasons, want one per required dependency: %v", len(reasons), reasons)
	}
	for _, d := range r.Snapshot() {
		if d.State != StateUnknown {
			t.Errorf("%s starts as %q, want unknown", d.Name, d.State)
		}
	}
}

func TestReadyWhenEveryRequiredDependencyIsUp(t *testing.T) {
	r := New(false, false)
	allUp(r)
	ready, reasons := r.Ready()
	if !ready {
		t.Fatalf("not ready with everything up: %v", reasons)
	}
}

// Status tracking is required, not advisory: without it transactions never receive a status.
func TestStatusTrackingIsRequiredForReadiness(t *testing.T) {
	r := New(false, false)
	allUp(r)
	r.Set(DepStatusTracking, StateDown, "monitor daemon is not running")

	ready, reasons := r.Ready()
	if ready {
		t.Fatal("ready without status tracking; transactions would never get a status")
	}
	joined := strings.Join(reasons, "; ")
	if !strings.Contains(joined, DepStatusTracking) {
		t.Errorf("the reasons do not name status tracking: %v", reasons)
	}
}

func TestOracleDownBlocksReadiness(t *testing.T) {
	r := New(true, true)
	allUp(r)
	r.Set(DepOracle, StateDown, "unreachable")

	ready, _ := r.Ready()
	if ready {
		t.Fatal("ready with the transaction oracle down; a hand could not settle")
	}
	rv, reasons := r.RealValueReady()
	if rv {
		t.Fatal("real-value ready with the oracle down")
	}
	if !strings.Contains(strings.Join(reasons, ";"), "oracle") {
		t.Errorf("reasons do not name the oracle: %v", reasons)
	}
}

// Real value is strictly stricter than readiness, because an unrestorable wallet loses coins
// rather than merely availability.
func TestRealValueRequiresAProvedRestore(t *testing.T) {
	r := New(true, false) // enabled, but no restore proved
	allUp(r)

	if ready, _ := r.Ready(); !ready {
		t.Fatal("the service should be ready to serve play")
	}
	rv, reasons := r.RealValueReady()
	if rv {
		t.Fatal("real value was permitted with no proved restore")
	}
	if !strings.Contains(strings.Join(reasons, ";"), "restore") {
		t.Errorf("reasons do not mention the restore: %v", reasons)
	}
}

func TestRealValueRequiresBeingEnabled(t *testing.T) {
	r := New(false, true) // restore proved, but play not enabled
	allUp(r)
	rv, reasons := r.RealValueReady()
	if rv {
		t.Fatal("real value was permitted without being enabled")
	}
	if !strings.Contains(strings.Join(reasons, ";"), "not enabled") {
		t.Errorf("reasons do not mention being disabled: %v", reasons)
	}
}

func TestRealValueReadyWhenFullyProvisioned(t *testing.T) {
	r := New(true, true)
	allUp(r)
	if rv, reasons := r.RealValueReady(); !rv {
		t.Fatalf("real value refused when fully provisioned: %v", reasons)
	}
}

// Liveness must not consult dependencies: restarting a process because a remote service is
// unreachable makes an outage worse.
func TestLivenessIgnoresDependencies(t *testing.T) {
	r := New(false, false)
	for _, n := range []string{DepDatabase, DepOracle, DepHeaders, DepStatusTracking} {
		r.Set(n, StateDown, "everything is broken")
	}

	rec := httptest.NewRecorder()
	r.LivenessHandler()(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("liveness returned %d with dependencies down; it must only report the process", rec.Code)
	}
}

func TestReadinessHandlerStatusCodes(t *testing.T) {
	r := New(true, true)

	rec := httptest.NewRecorder()
	r.ReadinessHandler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness returned %d before any check, want 503", rec.Code)
	}

	allUp(r)
	rec = httptest.NewRecorder()
	r.ReadinessHandler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readiness returned %d with everything up, want 200", rec.Code)
	}

	var body readinessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Ready || !body.RealValueReady {
		t.Errorf("body reports ready=%v realValueReady=%v", body.Ready, body.RealValueReady)
	}
	if len(body.Dependencies) != 4 {
		t.Errorf("body lists %d dependencies, want 4", len(body.Dependencies))
	}
}

// A readiness probe must say WHY, or an operator is left guessing.
func TestReadinessExplainsWhyItIsNotReady(t *testing.T) {
	r := New(true, true)
	allUp(r)
	r.Set(DepDatabase, StateDown, "connection refused")

	rec := httptest.NewRecorder()
	r.ReadinessHandler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	var body readinessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Ready {
		t.Fatal("reported ready with the database down")
	}
	joined := strings.Join(body.Reasons, ";")
	if !strings.Contains(joined, "connection refused") {
		t.Errorf("the reasons omit the detail: %v", body.Reasons)
	}
}

func TestOptionalDependencyDoesNotBlockReadiness(t *testing.T) {
	r := New(false, false)
	allUp(r)
	r.SetOptional("metrics", StateDown, "scrape endpoint unreachable")

	ready, reasons := r.Ready()
	if !ready {
		t.Fatalf("an optional dependency blocked readiness: %v", reasons)
	}
	found := false
	for _, d := range r.Snapshot() {
		if d.Name == "metrics" {
			found = true
			if d.Required {
				t.Error("the optional dependency was registered as required")
			}
		}
	}
	if !found {
		t.Error("the optional dependency is missing from the snapshot")
	}
}

func TestSnapshotIsStablyOrdered(t *testing.T) {
	r := New(false, false)
	allUp(r)
	first := r.Snapshot()
	for i := 0; i < 5; i++ {
		again := r.Snapshot()
		for j := range first {
			if again[j].Name != first[j].Name {
				t.Fatal("snapshot order varies between calls")
			}
		}
	}
	// And it is actually sorted, so a diff between probes is readable.
	for i := 1; i < len(first); i++ {
		if first[i-1].Name > first[i].Name {
			t.Fatalf("snapshot is not sorted: %s before %s", first[i-1].Name, first[i].Name)
		}
	}
}

func TestSetRecordsDetailAndTime(t *testing.T) {
	r := New(false, false)
	r.Set(DepOracle, StateDown, "504 from the gateway")
	for _, d := range r.Snapshot() {
		if d.Name != DepOracle {
			continue
		}
		if d.State != StateDown || d.Detail != "504 from the gateway" {
			t.Errorf("dependency = %+v", d)
		}
		if d.CheckedAt.IsZero() {
			t.Error("no check time was recorded")
		}
	}
}
