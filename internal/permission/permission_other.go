//go:build !darwin || !cgo

package permission

// Check reports Unknown where macOS privacy permissions do not exist.
func Check(Kind) State { return Unknown }

// Request reports Unknown where macOS privacy permissions do not exist.
func Request(Kind) State { return Unknown }

// App reports no host app where macOS privacy permissions do not exist.
func App() HostApp { return HostApp{} }
