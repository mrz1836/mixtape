//go:build darwin

package cli

import "syscall"

// osProductVersion returns the macOS version, such as "15.8.2".
func osProductVersion() (string, error) { return syscall.Sysctl("kern.osproductversion") }
