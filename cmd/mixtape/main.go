// Command mixtape records Spotify tracks, albums and playlists on macOS into
// tagged, per-song M4A files. All logic lives in internal/cli; this file
// only carries the build metadata in and the exit code out.
package main

import (
	"context"
	"os"

	"github.com/mrz1836/mixtape/internal/cli"
)

// Build metadata, injected via -ldflags by magex and goreleaser:
//
//	-X main.version=…  -X main.commit=…  -X main.buildDate=…
//
//nolint:gochecknoglobals // -ldflags can only target package-level variables
var (
	version   string
	commit    string
	buildDate string
)

func main() {
	os.Exit(cli.Execute(context.Background(), cli.BuildInfo{
		Version: version,
		Commit:  commit,
		Date:    buildDate,
	}))
}
