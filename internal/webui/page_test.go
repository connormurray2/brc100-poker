package webui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The setup page is the whole onboarding path, so its content is worth asserting rather than
// eyeballing. A player who cannot see how to start a wallet cannot play at all.
func TestSetupPageExplainsHowToRunAWallet(t *testing.T) {
	h := NewStore().Handler("02deadbeef", "test", "ttn")
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("GET / = %d, want 200", w.Code)
	}
	body := w.Body.String()

	// The commands a player must run, and the controls they then use.
	for _, want := range []string{
		"Start your wallet",
		"cmd/agent",
		"cmd/keygen",
		"-table",
		"-origin",
		`id="agentUrl"`,
		`id="faucet"`,
		`id="balance"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the setup page does not mention %q", want)
		}
	}

	// The server-dealt path is deliberately gone: offering it made the page ambiguous about
	// whether a hand was private, and a player should not have to reason about that.
	for _, gone := range []string{"bsv-sdk.js", "server shuffles", "(optional)"} {
		if strings.Contains(body, gone) {
			t.Errorf("the setup page still offers %q", gone)
		}
	}
}

// The page drives the wallet over HTTP, so it must not depend on the vendored SDK bundle that
// used to be served for the browser-wallet path.
func TestBundledSDKIsNoLongerServed(t *testing.T) {
	h := NewStore().Handler("02deadbeef", "test", "ttn")
	r := httptest.NewRequest("GET", "/bsv-sdk.js", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == 200 {
		t.Fatalf("GET /bsv-sdk.js = 200, want the bundle to be gone")
	}
}
