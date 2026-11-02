package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mrz1836/mixtape/internal/config"
)

// configSettingDoc is one row of `config show -o json`.
type configSettingDoc struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Source  string `json:"source"`
	Env     string `json:"env"`
	Default string `json:"default"`
	Help    string `json:"help"`
}

type configShowDoc struct {
	Path     string             `json:"path"`
	Settings []configSettingDoc `json:"settings"`
}

type configSetDoc struct {
	Path  string `json:"path"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

type pathDoc struct {
	Path string `json:"path"`
}

func newConfigCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or change persistent settings",
		Long: `Show or change Mixtape's persistent settings, stored as JSON in
~/.mixtape/config.json (or under --home).

Each setting resolves from, highest first: a command-line flag, a MIXTAPE_*
environment variable, the config file, and the built-in default.
'mixtape config show' prints every value and where it came from.`,
		GroupID: groupSetup,
		Args:    cobra.NoArgs,
	}
	cmd.AddCommand(newConfigShowCmd(a), newConfigSetCmd(a), newConfigPathCmd(a))
	return cmd
}

func newConfigShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print every setting, its value and its source",
		Long: `Print every setting with its resolved value and the layer it came from:
flag, env, file or default.`,
		Example: `  mixtape config show
  mixtape config show -o json`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			s, err := a.settings(config.Overrides{})
			if err != nil {
				return err
			}
			doc := configShowDoc{Path: config.Path(a.home)}
			var b strings.Builder
			tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", a.out.paint(colorBold, "KEY"),
				a.out.paint(colorBold, "VALUE"), a.out.paint(colorBold, "SOURCE"))
			for _, k := range config.Keys() {
				row := configSettingDoc{
					Key: k.Name, Value: s.Value(k.Name), Source: string(s.Sources[k.Name]),
					Env: k.Env, Default: k.Default, Help: k.Help,
				}
				doc.Settings = append(doc.Settings, row)
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", row.Key, row.Value, row.Source)
			}
			_ = tw.Flush()
			text := b.String() + a.out.paint(colorDim, "file: "+doc.Path)
			return a.out.Result(text, doc)
		},
	}
}

func newConfigSetCmd(a *app) *cobra.Command {
	keys := make([]string, 0, len(config.Keys()))
	var help strings.Builder
	for _, k := range config.Keys() {
		keys = append(keys, k.Name)
		_, _ = fmt.Fprintf(&help, "\n  %-13s %s (default %s)", k.Name, k.Help, k.Default)
	}
	return &cobra.Command{
		Use:   "set KEY VALUE",
		Short: "Change a setting in the config file",
		Long: `Validate VALUE and write it to the config file. An empty VALUE ("")
removes the key, so the setting falls back to its default.

Settings:` + help.String(),
		Example: `  mixtape config set library ~/Music/Mixtape
  mixtape config set capture device
  mixtape config set artwork true
  mixtape config set capture ""`,
		Args:      cobra.ExactArgs(2),
		ValidArgs: keys,
		RunE: func(_ *cobra.Command, args []string) error {
			path := config.Path(a.home)
			key, value := strings.ToLower(args[0]), args[1]
			if err := config.Set(path, key, value, a.deps.userHome); err != nil {
				return err
			}
			text := fmt.Sprintf("%s = %s", key, value)
			if value == "" {
				text = key + " reset to its default"
			}
			return a.out.Result(text, configSetDoc{Path: path, Key: key, Value: value})
		},
	}
}

func newConfigPathCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the config file path",
		Long:  `Print the path of the config file, whether or not it exists yet.`,
		Example: `  mixtape config path
  open "$(mixtape config path -o text)"`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path := config.Path(a.home)
			return a.out.Result(path, pathDoc{Path: path})
		},
	}
}
