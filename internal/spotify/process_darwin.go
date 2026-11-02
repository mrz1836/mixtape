//go:build darwin

package spotify

import (
	"bytes"
	"fmt"

	"golang.org/x/sys/unix"
)

// processName is the executable name of the Spotify app's main process.
const processName = "Spotify"

// IsRunning reports whether the Spotify app is running, from the kernel's
// process table. It sends no Apple Events, so it never launches Spotify and
// needs no permission.
func IsRunning() (bool, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return false, fmt.Errorf("list processes: %w", err)
	}
	for i := range procs {
		comm := procs[i].Proc.P_comm[:]
		if n := bytes.IndexByte(comm, 0); n >= 0 {
			comm = comm[:n]
		}
		if string(comm) == processName {
			return true, nil
		}
	}
	return false, nil
}
