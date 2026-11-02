//go:build !unix

package library

// lockFile is a no-op where flock does not exist; Mixtape only records on
// macOS.
func lockFile(string) (func(), error) { return func() {}, nil }
