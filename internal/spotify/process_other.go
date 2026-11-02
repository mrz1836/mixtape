//go:build !darwin

package spotify

import "errors"

// IsRunning reports whether the Spotify app is running. Only macOS is
// supported.
func IsRunning() (bool, error) { return false, errors.ErrUnsupported }
