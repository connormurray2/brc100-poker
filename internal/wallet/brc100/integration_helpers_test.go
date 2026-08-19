//go:build integration

package brc100

// boolPtr is shared by the integration tests, which need pointers for the SDK's optional
// booleans (notably RandomizeOutputs, which defaults to true and must be turned off).
func boolPtr(b bool) *bool { return &b }
