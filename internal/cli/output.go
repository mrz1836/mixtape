package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// Output formats accepted by --output.
const (
	formatAuto = "auto"
	formatText = "text"
	formatJSON = "json"
)

// ansiSGR matches a CSI SGR escape sequence, stripped when color is off.
var ansiSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Output is the single gate for everything a command prints. It owns the
// text/JSON decision, --quiet and --verbose, and color, so no command
// writes to a stream directly.
//
// In JSON mode stdout carries only machine-readable documents (one result,
// or an NDJSON event stream) and stderr carries only JSON lines (warnings
// and the final error), so an agent can parse either stream, or both
// merged, without filtering.
type Output struct {
	mu      sync.Mutex
	json    bool
	quiet   bool
	verbose bool

	stdout    io.Writer
	stderr    io.Writer
	stdoutTTY bool
	stderrTTY bool
	color     bool // stdout is a terminal and color is allowed
	errColor  bool // stderr is a terminal and color is allowed
}

// outputOptions configures newOutput.
type outputOptions struct {
	format    string // auto, text or json
	quiet     bool
	verbose   bool
	noColor   bool
	stdout    io.Writer
	stderr    io.Writer
	stdoutTTY bool
	stderrTTY bool
}

func newOutput(o outputOptions) (*Output, error) {
	var jsonMode bool
	switch strings.ToLower(o.format) {
	case formatAuto, "":
		jsonMode = !o.stdoutTTY
	case formatText:
	case formatJSON:
		jsonMode = true
	default:
		return nil, fmt.Errorf("%w %q (want text, json or auto)", errBadOutput, o.format)
	}
	return &Output{
		json:      jsonMode,
		quiet:     o.quiet,
		verbose:   o.verbose,
		stdout:    o.stdout,
		stderr:    o.stderr,
		stdoutTTY: o.stdoutTTY,
		stderrTTY: o.stderrTTY,
		color:     o.stdoutTTY && !o.noColor,
		errColor:  o.stderrTTY && !o.noColor,
	}, nil
}

// JSON reports whether output is machine-readable.
func (o *Output) JSON() bool { return o.json }

// Quiet reports whether informational text is suppressed.
func (o *Output) Quiet() bool { return o.quiet }

// Verbose reports whether diagnostics are enabled.
func (o *Output) Verbose() bool { return o.verbose }

// Interactive reports whether a person is watching text output on a
// terminal: the condition for status lines and the update banner.
func (o *Output) Interactive() bool { return !o.json && !o.quiet && o.stdoutTTY }

// Result prints a command's outcome: v as one JSON document in JSON mode,
// or text otherwise. Text is printed even under --quiet, because it is the
// data the command exists to produce. JSON is indented on a terminal and
// compact when piped.
func (o *Output) Result(text string, v any) error {
	if o.json {
		return o.writeJSON(o.stdout, v, o.stdoutTTY)
	}
	return o.writeText(o.stdout, text, o.color)
}

// Event writes one compact NDJSON line to stdout, whatever the terminal.
func (o *Output) Event(v any) error { return o.writeJSON(o.stdout, v, false) }

// Println prints informational text to stdout. Suppressed by --quiet and
// in JSON mode, where events carry the same information.
func (o *Output) Println(format string, args ...any) {
	if o.json || o.quiet {
		return
	}
	_ = o.writeText(o.stdout, fmt.Sprintf(format, args...), o.color)
}

// Warn reports a non-fatal problem on stderr: "mixtape: warning: …" as
// text, or {"warning":{"message":…}} in JSON mode. Suppressed by --quiet.
func (o *Output) Warn(format string, args ...any) {
	if o.quiet {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if o.json {
		_ = o.writeJSON(o.stderr, map[string]any{"warning": map[string]string{"message": msg}}, false)
		return
	}
	_ = o.writeText(o.stderr, o.paintErr(colorYellow, "mixtape: warning: ")+msg, o.errColor)
}

// Fail renders e on stderr for the command at label ("record", "config
// set", or "" for the root): text with an optional "fix:" line, or a JSON
// error document.
func (o *Output) Fail(label string, e *Error) {
	if o.json {
		doc := errorDoc{Error: errorBody{Code: e.Code, Message: e.Message, Hint: e.Hint, ExitCode: e.Exit}}
		_ = o.writeJSON(o.stderr, doc, o.stderrTTY)
		return
	}
	prefix := "mixtape: "
	if label != "" {
		prefix += label + ": "
	}
	text := o.paintErr(colorRed, prefix) + e.Message
	if e.Hint != "" {
		text += "\n  fix: " + e.Hint
	}
	_ = o.writeText(o.stderr, text, o.errColor)
}

// Writer returns stdout for commands that render their own text (help,
// completion scripts).
func (o *Output) Writer() io.Writer { return o.stdout }

func (o *Output) writeJSON(w io.Writer, v any, indent bool) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep <, > and & readable: this is not HTML
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := w.Write(buf.Bytes())
	return err
}

func (o *Output) writeText(w io.Writer, s string, color bool) error {
	if !color {
		s = ansiSGR.ReplaceAllString(s, "")
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := io.WriteString(w, s)
	return err
}

// SGR color codes.
const (
	colorRed    = "31"
	colorGreen  = "32"
	colorYellow = "33"
	colorDim    = "2"
	colorBold   = "1"
)

// table renders rows as columns under a bold header, two spaces apart.
// Widths count what is visible, so colored cells do not skew the columns.
func (o *Output) table(header []string, rows [][]string) string {
	widths := make([]int, len(header))
	for _, r := range append([][]string{header}, rows...) {
		for i, c := range r {
			widths[i] = max(widths[i], visibleWidth(c))
		}
	}
	var b strings.Builder
	line := func(cells []string, paint func(string) string) {
		for i, c := range cells {
			if i > 0 {
				b.WriteString(strings.Repeat(" ", widths[i-1]-visibleWidth(cells[i-1])+2))
			}
			b.WriteString(paint(c))
		}
	}
	line(header, func(s string) string { return o.paint(colorBold, s) })
	for _, r := range rows {
		b.WriteByte('\n')
		line(r, func(s string) string { return s })
	}
	return b.String()
}

// visibleWidth is the number of characters s shows, without color codes.
func visibleWidth(s string) int { return utf8.RuneCountInString(ansiSGR.ReplaceAllString(s, "")) }

// paint wraps s in an SGR color when stdout color is on.
func (o *Output) paint(code, s string) string { return sgr(o.color, code, s) }

// paintErr wraps s in an SGR color when stderr color is on.
func (o *Output) paintErr(code, s string) string { return sgr(o.errColor, code, s) }

func sgr(on bool, code, s string) string {
	if !on || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
