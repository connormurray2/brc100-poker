package webui

import (
	"context"
	"errors"

	sdk "github.com/bsv-blockchain/go-sdk/wallet"
)

type noopFunder struct{}

func (noopFunder) CreateAction(context.Context, sdk.CreateActionArgs, string) (*sdk.CreateActionResult, error) {
	return nil, errors.New("not used")
}
func (noopFunder) SignAction(context.Context, sdk.SignActionArgs, string) (*sdk.SignActionResult, error) {
	return nil, errors.New("not used")
}
func (noopFunder) AbortAction(context.Context, sdk.AbortActionArgs, string) (*sdk.AbortActionResult, error) {
	return nil, errors.New("not used")
}
