package cli

import (
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Global flag names.
const (
	flagOutput  = "output"
	flagQuiet   = "quiet"
	flagVerbose = "verbose"
	flagNoColor = "no-color"
	flagHome    = "home"
	flagLibrary = "library"
)

// envOutput sets the default --output format.
const envOutput = "MIXTAPE_OUTPUT"

// globalFlags holds the persistent flags every command inherits.
//
// There is deliberately no root -c or -f: the update command's --check/-c
// and --force/-f own those shorthands.
type globalFlags struct {
	output     string
	quiet      bool
	verbose    bool
	noColor    bool
	home       string
	library    string
	librarySet bool
}

func (a *app) addGlobalFlags(root *cobra.Command) {
	pf := root.PersistentFlags()
	pf.StringVarP(&a.flags.output, flagOutput, "o", "",
		"Output format: text, json, or auto (text on a terminal, JSON when piped) (env MIXTAPE_OUTPUT)")
	pf.BoolVarP(&a.flags.quiet, flagQuiet, "q", false, "Print only results and errors")
	pf.BoolVarP(&a.flags.verbose, flagVerbose, "v", false, "Print diagnostics (and extra events in JSON mode)")
	pf.BoolVar(&a.flags.noColor, flagNoColor, false, "Disable color (also honors NO_COLOR)")
	pf.StringVar(&a.flags.home, flagHome, "", "Mixtape state directory (env MIXTAPE_HOME, default ~/.mixtape)")
	pf.VarP(&libraryFlag{a: &a.flags}, flagLibrary, "L", "Library folder (env MIXTAPE_LIBRARY, default ~/Music/Mixtape)")
}

// libraryFlag is a string flag that records whether it was set, so an
// empty --library is still distinguishable from an absent one.
type libraryFlag struct{ a *globalFlags }

var _ pflag.Value = (*libraryFlag)(nil)

func (f *libraryFlag) String() string { return f.a.library }
func (f *libraryFlag) Type() string   { return "path" }

func (f *libraryFlag) Set(v string) error {
	f.a.library, f.a.librarySet = v, true
	return nil
}
