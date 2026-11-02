package cli

import (
	"io"

	selfupdate "github.com/mrz1836/go-selfupdate"
	"github.com/mrz1836/go-selfupdate/cobracmd"
	"github.com/mrz1836/go-selfupdate/notify"
	"github.com/spf13/cobra"
)

// Release coordinates shared by `mixtape update` and the passive banner.
const (
	releaseOwner = "mrz1836"
	releaseRepo  = "mixtape"
	tokenEnvVar  = "MIXTAPE_GITHUB_TOKEN" //nolint:gosec // G101 false positive: an environment variable name, not a credential
)

// updateConfig returns the self-update config for the running build.
// Mixtape ships macOS binaries only, so other platforms get a clear refusal
// before any network call.
func updateConfig(version string) selfupdate.Config {
	return selfupdate.Config{
		Owner:          releaseOwner,
		Repo:           releaseRepo,
		BinaryName:     rootName,
		CurrentVersion: version,
		TokenEnvVar:    tokenEnvVar,
		Platforms: []selfupdate.Platform{
			{OS: "darwin", Arch: "arm64"},
			{OS: "darwin", Arch: "amd64"},
		},
	}
}

// attachUpdate registers `mixtape update` (alias `upgrade`) and the passive
// "a new version is available" banner.
//
// It uses cobracmd.New plus AttachBanner rather than Attach so the banner
// gets its own writer: one that prints only when a person is reading text
// on a terminal. JSON output, --quiet, pipes and agent logs never see it.
// Failures are reported by the root like every other command's, so the
// library's own error reporting stays off.
func attachUpdate(a *app, root *cobra.Command) {
	cfg := updateConfig(a.deps.info.Version)
	cmd := cobracmd.New(cfg)
	cmd.GroupID = groupOther
	cmd.Example = `  mixtape update            # download, verify and install the latest release
  mixtape update --check    # report whether a newer version is available
  mixtape update --force    # reinstall the latest release even if current`
	root.AddCommand(cmd)

	cobracmd.AttachBanner(root, notify.Config{
		Owner:          cfg.Owner,
		Repo:           cfg.Repo,
		BinaryName:     cfg.BinaryName,
		CurrentVersion: cfg.CurrentVersion,
		TokenEnvVar:    cfg.TokenEnvVar,
		BannerOut:      bannerWriter{a: a},
		Getenv:         a.deps.getenv,
	})
}

// bannerWriter forwards the update banner to stderr only for an
// interactive, text-mode, non-quiet invocation. The decision is made at
// write time, after the global flags are resolved.
type bannerWriter struct{ a *app }

var _ io.Writer = bannerWriter{}

func (w bannerWriter) Write(p []byte) (int, error) {
	o := w.a.out
	if o == nil || o.JSON() || o.Quiet() || !w.a.deps.stderrTTY {
		return len(p), nil
	}
	return w.a.deps.stderr.Write(p)
}
