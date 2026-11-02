package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// versionDoc is the JSON shape of `mixtape version`.
type versionDoc struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

func newVersionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		Long: `Print the version, commit and build date of this mixtape binary.

On a terminal the version prints as text; piped or with -o json it prints as
a JSON object with version, commit and date keys.`,
		Example: `  mixtape version
  mixtape version -o json`,
		GroupID: groupOther,
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			info := a.deps.info
			text := fmt.Sprintf("mixtape %s\ncommit:  %s\nbuilt:   %s", info.Version, info.Commit, info.Date)
			return a.out.Result(text, versionDoc(info))
		},
	}
}
