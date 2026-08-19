package config

import (
	"strings"
	"testing"

	"github.com/galt-tr/go-arcade-toolbox/pkg/defs"
)

func TestNetworkIsTeratestnet(t *testing.T) {
	if Network != defs.NetworkTTN {
		t.Fatalf("network = %q, want %q", Network, defs.NetworkTTN)
	}
	if !Network.IsTestnetBased() {
		t.Fatal("teratestnet must be testnet-based so key and address params are testnet")
	}
}

func TestResolveServicesTargetsTTN(t *testing.T) {
	svc, err := ResolveServices()
	if err != nil {
		t.Fatalf("ResolveServices: %v", err)
	}
	if !strings.Contains(svc.Arcade, "ttn") {
		t.Errorf("arcade URL %q does not look like a teratestnet endpoint", svc.Arcade)
	}
	if !strings.HasPrefix(svc.Arcade, "https://") {
		t.Errorf("arcade URL %q is not https", svc.Arcade)
	}
	if !strings.HasPrefix(svc.ChainTracks, "https://") {
		t.Errorf("chaintracks URL %q is not https", svc.ChainTracks)
	}
	t.Logf("arcade=%s events=%s chaintracks=%s", svc.Arcade, svc.Events, svc.ChainTracks)
}

func TestFeeModelIsNot100(t *testing.T) {
	fm := FeeModel()
	if fm.Value == 100 {
		t.Fatal("fee rate is the toolbox default of 100, which leaves no margin against arcade's validator")
	}
	if fm.Value != FeeRateSatPerKB {
		t.Fatalf("fee value = %d, want %d", fm.Value, FeeRateSatPerKB)
	}
}
