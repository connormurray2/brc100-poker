// Package config resolves the chain-service configuration this project runs against.
//
// The network is always set explicitly. Nothing here relies on a library default:
// storage.Provider defaults to mainnet while perfprovider defaults to testnet, so a
// component that is merely left alone does not agree with its neighbours.
package config

import (
	"fmt"

	"github.com/galt-tr/go-arcade-toolbox/pkg/defs"
)

// Network is the only chain this project targets.
const Network = defs.NetworkTTN

// FeeRateSatPerKB is the rate every action pays.
//
// The toolbox default is 100, which the docs warn leaves no margin: arcade's validator
// prices the extended-format size, which the toolbox's own fee arithmetic does not count.
const FeeRateSatPerKB = 125

// Services describes the resolved chain endpoints.
type Services struct {
	Arcade      string
	Events      string
	ChainTracks string
}

// ResolveServices returns the endpoints for the configured network.
func ResolveServices() (Services, error) {
	cfg := defs.DefaultServicesConfig(Network)
	if cfg.Arcade.URL == "" {
		return Services{}, fmt.Errorf("config: no arcade URL resolved for network %q", Network)
	}
	if cfg.ChainTracks.URL == "" {
		return Services{}, fmt.Errorf("config: no chaintracks URL resolved for network %q", Network)
	}
	return Services{
		Arcade:      cfg.Arcade.URL,
		Events:      cfg.Arcade.EventsURL,
		ChainTracks: cfg.ChainTracks.URL,
	}, nil
}

// FeeModel returns the fee model every wallet and storage provider must be built with.
func FeeModel() defs.FeeModel {
	return defs.FeeModel{Type: defs.SatPerKB, Value: FeeRateSatPerKB}
}

// MinBroadcastFeeRateSatPerKB is the floor below which a transaction fails locally
// instead of being broadcast and rejected.
//
// A rejection is a verdict, not a transport failure: it cannot be retried. Turning
// underpayment into a local error is strictly better than discovering it remotely.
const MinBroadcastFeeRateSatPerKB = 100
