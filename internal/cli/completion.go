package cli

import (
	"github.com/spf13/cobra"
)

func newCompletionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "completion bash|zsh|fish|powershell",
		Short: "Generate a shell completion script",
		Long: `Generate a shell completion script for mixtape and print it to stdout.

Bash:
  source <(mixtape completion bash)

Zsh (once; start a new shell afterwards):
  mixtape completion zsh > "${fpath[1]}/_mixtape"

Fish:
  mixtape completion fish > ~/.config/fish/completions/mixtape.fish

PowerShell:
  mixtape completion powershell | Out-String | Invoke-Expression`,
		Example: `  mixtape completion zsh > "${fpath[1]}/_mixtape"
  source <(mixtape completion bash)`,
		GroupID:               groupOther,
		DisableFlagsInUseLine: true,
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, w := cmd.Root(), a.out.Writer()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(w, true)
			case "zsh":
				return root.GenZshCompletion(w)
			case "fish":
				return root.GenFishCompletion(w, true)
			default:
				return root.GenPowerShellCompletionWithDesc(w)
			}
		},
	}
}
