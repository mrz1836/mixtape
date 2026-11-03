package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// walkCommands visits every command in the tree depth-first.
func walkCommands(cmd *cobra.Command, fn func(*cobra.Command)) {
	fn(cmd)
	for _, sub := range cmd.Commands() {
		walkCommands(sub, fn)
	}
}

func TestAllCommandsHaveShortAndLong(t *testing.T) {
	t.Parallel()
	walkCommands(testRoot(t), func(cmd *cobra.Command) {
		assert.NotEmpty(t, cmd.Short, "%s: missing Short", cmd.CommandPath())
		assert.NotEmpty(t, cmd.Long, "%s: missing Long", cmd.CommandPath())
		assert.LessOrEqual(t, len(cmd.Short), 80, "%s: Short too long", cmd.CommandPath())
		assert.NotEmpty(t, cmd.Use, "%s: missing Use", cmd.CommandPath())
	})
}

func TestLeafCommandsHaveExamples(t *testing.T) {
	t.Parallel()
	walkCommands(testRoot(t), func(cmd *cobra.Command) {
		if cmd.HasSubCommands() || (cmd.RunE == nil && cmd.Run == nil) {
			return // a group shows its help, and its subcommands have the examples
		}
		assert.NotEmpty(t, cmd.Example, "%s: leaf command missing Example", cmd.CommandPath())
	})
}

func TestExamplesNameTheBinary(t *testing.T) {
	t.Parallel()
	walkCommands(testRoot(t), func(cmd *cobra.Command) {
		for line := range strings.SplitSeq(cmd.Example, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			assert.Contains(t, line, "mixtape", "%s: example line %q", cmd.CommandPath(), line)
		}
	})
}

func TestNoEmbeddedExamplesInLong(t *testing.T) {
	t.Parallel()
	walkCommands(testRoot(t), func(cmd *cobra.Command) {
		assert.False(t, strings.Contains(cmd.Long, "\nExample:") || strings.Contains(cmd.Long, "\nExamples:"),
			"%s: move examples from Long to Example", cmd.CommandPath())
	})
}

func TestAllFlagsHaveDescriptions(t *testing.T) {
	t.Parallel()
	walkCommands(testRoot(t), func(cmd *cobra.Command) {
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			assert.NotEmpty(t, f.Usage, "flag --%s on %s has no description", f.Name, cmd.CommandPath())
		})
	})
}

func TestTopLevelCommandsAreGrouped(t *testing.T) {
	t.Parallel()
	root := testRoot(t)
	for _, cmd := range root.Commands() {
		if !cmd.IsAvailableCommand() {
			continue
		}
		assert.NotEmpty(t, cmd.GroupID, "top-level command %q missing GroupID", cmd.Name())
	}
}

func TestRootHelpShowsGroups(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.Equal(t, ExitOK, h.run("--help"))
	out := h.stdout.String()
	for _, title := range []string{"Recording:", "Library:", "Setup:", "Other:"} {
		assert.Contains(t, out, title)
	}
	assert.Contains(t, out, "--output")
	assert.Contains(t, out, "--library")
}

func TestRootWithoutArgsShowsHelp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.Equal(t, ExitOK, h.run())
	assert.Contains(t, h.stdout.String(), "Usage:")
	assert.Empty(t, h.stderr.String())
}

func TestParentCommandsListSubcommands(t *testing.T) {
	t.Parallel()
	walkCommands(testRoot(t), func(cmd *cobra.Command) {
		if !cmd.HasAvailableSubCommands() || cmd.Parent() == nil {
			return
		}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		require.NoError(t, cmd.Help())
		for _, sub := range cmd.Commands() {
			if sub.IsAvailableCommand() {
				assert.Contains(t, buf.String(), sub.Name(), "%s help misses %s", cmd.CommandPath(), sub.Name())
			}
		}
	})
}

func TestNoShorthandCollisions(t *testing.T) {
	t.Parallel()
	// Executing --help on every command merges inherited flags, which panics
	// on a shorthand collision (the reason root has no -c or -f).
	walkCommands(testRoot(t), func(cmd *cobra.Command) {
		path := strings.Fields(cmd.CommandPath())[1:]
		h := newHarness(t)
		assert.Equal(t, ExitOK, h.run(append(path, "--help")...), "%s --help", cmd.CommandPath())
	})
}
