package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// colorEnabled reports whether ANSI colour should be emitted to w under the
// given --color mode. "auto" (the default) colours only a real terminal, and
// always yields to the NO_COLOR convention (https://no-color.org).
func colorEnabled(w io.Writer, mode string) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	}
	if _, noColor := os.LookupEnv("NO_COLOR"); noColor {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// ANSI SGR codes for the JSON palette.
const (
	ansiReset = "\x1b[0m"
	ansiKey   = "\x1b[36m" // cyan
	ansiStr   = "\x1b[32m" // green
	ansiNum   = "\x1b[33m" // yellow
	ansiLit   = "\x1b[35m" // magenta (true/false/null)
	ansiPunct = "\x1b[90m" // bright black (braces, commas, colons)
)

// fprintJSON pretty-prints a JSON byte slice to w, colourising when w is a
// terminal. Non-JSON input is echoed verbatim.
//
// Commands pass cmd.OutOrStdout() rather than os.Stdout: that is the writer
// cobra hands a test (or a caller embedding the command tree), and taking
// os.Stdout directly made every command's own output unreachable from a test
// that had already redirected the command.
func fprintJSON(w io.Writer, mode string, raw []byte) {
	if !json.Valid(bytes.TrimSpace(raw)) {
		fmt.Fprintln(w, string(raw))
		return
	}
	var sb strings.Builder
	if err := colorizeJSON(&sb, raw, colorEnabled(w, mode)); err != nil {
		// Fall back to plain indented output on any tokenizer hiccup.
		var value any
		if json.Unmarshal(raw, &value) == nil {
			pretty, _ := json.MarshalIndent(value, "", "  ")
			fmt.Fprintln(w, string(pretty))
			return
		}
		fmt.Fprintln(w, string(raw))
		return
	}
	fmt.Fprintln(w, sb.String())
}

// colorizeJSON re-emits raw JSON with 2-space indentation, preserving object
// key order (it streams tokens rather than round-tripping through a map). When
// color is false it produces plain, uncoloured pretty JSON.
func colorizeJSON(out io.Writer, raw []byte, color bool) error {
	paint := func(code, s string) string {
		if !color {
			return s
		}
		return code + s + ansiReset
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	type frame struct {
		isObject    bool
		count       int
		hasChildren bool
	}
	var stack []*frame
	var sb strings.Builder

	// prefix writes the separator (comma/newline/indent, or ": " for an
	// object value) that precedes the next element of the current container.
	prefix := func() {
		if len(stack) == 0 {
			return
		}
		f := stack[len(stack)-1]
		if f.isObject && f.count%2 == 1 { // value slot follows its key
			sb.WriteString(paint(ansiPunct, ":"))
			sb.WriteString(" ")
			return
		}
		if f.count > 0 {
			sb.WriteString(paint(ansiPunct, ","))
		}
		sb.WriteString("\n")
		sb.WriteString(strings.Repeat("  ", len(stack)))
		f.hasChildren = true
	}
	bump := func() {
		if len(stack) > 0 {
			stack[len(stack)-1].count++
		}
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				prefix()
				sb.WriteString(paint(ansiPunct, string(rune(t))))
				stack = append(stack, &frame{isObject: t == '{'})
			case '}', ']':
				if len(stack) == 0 {
					// Unbalanced close — can't happen for the valid JSON that
					// fprintJSON gates on, but never index into an empty stack.
					return fmt.Errorf("unbalanced JSON delimiter")
				}
				f := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if f.hasChildren {
					sb.WriteString("\n")
					sb.WriteString(strings.Repeat("  ", len(stack)))
				}
				sb.WriteString(paint(ansiPunct, string(rune(t))))
				bump()
			}
		default:
			isKey := len(stack) > 0 && stack[len(stack)-1].isObject && stack[len(stack)-1].count%2 == 0
			prefix()
			sb.WriteString(renderScalar(tok, paint, isKey))
			bump()
		}
	}

	_, err := io.WriteString(out, sb.String())
	return err
}

func renderScalar(tok any, paint func(code, s string) string, isKey bool) string {
	switch v := tok.(type) {
	case string:
		q := jsonQuote(v)
		if isKey {
			return paint(ansiKey, q)
		}
		return paint(ansiStr, q)
	case json.Number:
		return paint(ansiNum, v.String())
	case bool:
		if v {
			return paint(ansiLit, "true")
		}
		return paint(ansiLit, "false")
	case nil:
		return paint(ansiLit, "null")
	default:
		return fmt.Sprintf("%v", v)
	}
}

// jsonQuote renders a string as a valid JSON string literal — control
// characters escaped the JSON way (unlike strconv.Quote, which emits Go
// syntax like \x00 that isn't valid JSON). HTML escaping is disabled so
// <, >, & display literally in the terminal.
func jsonQuote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return `""`
	}
	return strings.TrimRight(buf.String(), "\n")
}
