//go:build !darwin

package cli

import "errors"

var errNotMacOS = errors.New("not macOS")

// osProductVersion is unknown off macOS; mixtape runs only there, but the
// package builds anywhere so CI can lint and test it on Linux.
func osProductVersion() (string, error) { return "", errNotMacOS }
