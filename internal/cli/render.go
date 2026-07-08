package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// render.go — shared output primitives (ANSI-aware tables, key/value sections,
// summary lines, status glyphs). Pure stdlib + golang.org/x/sys; no TUI deps.

// --- Terminal width ----------------------------------------------------------

// termWidth returns the current terminal width in columns. It queries the
// TIOCGWINSZ ioctl on stdout, falls back to the $COLUMNS env var, and finally
// defaults to 80 when the width cannot be determined (e.g. piped output).
func termWidth() int {
	if ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 {
		return int(ws.Col)
	}
	if c := os.Getenv("COLUMNS"); c != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(c)); err == nil && n > 0 {
			return n
		}
	}
	return 80
}

// --- ANSI-aware string width -------------------------------------------------

// displayWidth returns the number of visible cells a string occupies, ignoring
// ANSI SGR escape sequences (e.g. color codes) which take no horizontal space.
func displayWidth(s string) int {
	w := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			// ANSI escape sequences end with a letter (typically 'm' for SGR).
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			continue
		}
		w++
	}
	return w
}

// padCell pads s with trailing spaces so its *visible* width equals width.
// ANSI escape bytes are not counted, so colored strings align correctly.
func padCell(s string, width int) string {
	n := width - displayWidth(s)
	if n <= 0 {
		return s
	}
	return s + strings.Repeat(" ", n)
}

// padLeft pads s with leading spaces to a visible width of width (right-align).
func padLeft(s string, width int) string {
	n := width - displayWidth(s)
	if n <= 0 {
		return s
	}
	return strings.Repeat(" ", n) + s
}

// truncateVisible shortens s to at most width visible cells, appending an
// ellipsis when truncation occurs. Plain text only (no ANSI) — used for the
// flex column, whose values (URLs, descriptions) are uncolored.
func truncateVisible(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	runes := []rune(s)
	return string(runes[:width-1]) + "…"
}

// --- Table builder -----------------------------------------------------------

// table accumulates rows and renders them as an aligned, minimally-styled grid.
// The final column is treated as a flexible column: it is truncated with an
// ellipsis so that rows never exceed the terminal width.
type table struct {
	headers []string
	rows    [][]string
	// rightAlign[i] == true renders column i right-aligned (for numbers).
	rightAlign map[int]bool
	indent     string
}

// newTable creates a table with the given column headers.
func newTable(headers ...string) *table {
	return &table{
		headers:    headers,
		rightAlign: map[int]bool{},
		indent:     "  ",
	}
}

// alignRight marks the given column indexes as right-aligned.
func (t *table) alignRight(cols ...int) *table {
	for _, c := range cols {
		t.rightAlign[c] = true
	}
	return t
}

// row appends a row. Values are converted with fmt.Sprint, so ints/strings/etc.
// may be passed directly.
func (t *table) row(cells ...any) {
	r := make([]string, len(cells))
	for i, c := range cells {
		r[i] = fmt.Sprint(c)
	}
	t.rows = append(t.rows, r)
}

// flush renders the table to out. Column widths are derived from visible
// content width; the last column flexes to the terminal width.
func (t *table) flush(out io.Writer) {
	ncol := len(t.headers)
	if ncol == 0 {
		return
	}

	// Compute the natural (untruncated) width of every column.
	widths := make([]int, ncol)
	for i, h := range t.headers {
		widths[i] = displayWidth(h)
	}
	for _, r := range t.rows {
		for i := 0; i < ncol && i < len(r); i++ {
			if w := displayWidth(r[i]); w > widths[i] {
				widths[i] = w
			}
		}
	}

	const gap = 2 // spaces between columns
	// Budget the final (flex) column against the terminal width.
	last := ncol - 1
	fixed := len(t.indent)
	for i := range last {
		fixed += widths[i] + gap
	}
	avail := termWidth() - fixed
	avail = max(avail, 8) // keep the flex column usable on very narrow terminals
	if widths[last] > avail {
		widths[last] = avail
	}

	sep := strings.Repeat(" ", gap)

	// Header row (dim, uppercase already supplied by caller).
	var hb strings.Builder
	hb.WriteString(t.indent)
	for i, h := range t.headers {
		cell := h
		if t.rightAlign[i] {
			cell = padLeft(h, widths[i])
		} else if i != last {
			cell = padCell(h, widths[i])
		}
		hb.WriteString(cell)
		if i != last {
			hb.WriteString(sep)
		}
	}
	fmt.Fprintln(out, dim(strings.TrimRight(hb.String(), " ")))

	// Data rows.
	for _, r := range t.rows {
		var b strings.Builder
		b.WriteString(t.indent)
		for i := range ncol {
			val := ""
			if i < len(r) {
				val = r[i]
			}
			if i == last {
				val = truncateVisible(val, widths[i])
				b.WriteString(val)
			} else if t.rightAlign[i] {
				b.WriteString(padLeft(val, widths[i]))
				b.WriteString(sep)
			} else {
				b.WriteString(padCell(val, widths[i]))
				b.WriteString(sep)
			}
		}
		fmt.Fprintln(out, strings.TrimRight(b.String(), " "))
	}
}

// --- Key/value sections ------------------------------------------------------

// kvSection renders aligned "key : value" lines under an optional bold title.
// Colons align to the widest key. Used by inspect/test/version-style output.
type kvSection struct {
	title  string
	indent string
	keys   []string
	vals   []string
}

// newKV creates a key/value section with the given (optional) title.
func newKV(title string) *kvSection {
	return &kvSection{title: title, indent: "  "}
}

// add appends a key/value pair. value is rendered via fmt.Sprint.
func (s *kvSection) add(key string, value any) *kvSection {
	s.keys = append(s.keys, key)
	s.vals = append(s.vals, fmt.Sprint(value))
	return s
}

// flush renders the section to out.
func (s *kvSection) flush(out io.Writer) {
	if s.title != "" {
		fmt.Fprintf(out, "%s%s\n", s.indent, bold(s.title))
	}
	kw := 0
	for _, k := range s.keys {
		if l := displayWidth(k); l > kw {
			kw = l
		}
	}
	childIndent := s.indent
	if s.title != "" {
		childIndent = s.indent + "  "
	}
	for i, k := range s.keys {
		fmt.Fprintf(out, "%s%s : %s\n", childIndent, padCell(k, kw), s.vals[i])
	}
}

// --- Summary line ------------------------------------------------------------

// summaryPart is one labeled count in a summary line.
type summaryPart struct {
	label string
	count int
}

// summaryLine builds a header like:
//
//	Upstreams   4 total · 3 healthy · 1 unhealthy
//
// title is bolded; parts are joined by "·". Parts whose label is already
// colored by the caller keep their color.
func summaryLine(title string, parts ...summaryPart) string {
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		segs = append(segs, fmt.Sprintf("%d %s", p.count, p.label))
	}
	return fmt.Sprintf("%s   %s", bold(title), strings.Join(segs, dim(" · ")))
}

// --- Status glyphs -----------------------------------------------------------

// statusDot returns a colored bullet followed by the colored status word,
// e.g. "● healthy". The glyph and word share the status color.
func statusDot(status string) string {
	glyph := "●"
	switch status {
	case "unknown", "input-required":
		glyph = "○"
	case "working", "submitted":
		glyph = "◐"
	}
	return colorStatus(glyph) + " " + colorStatus(status)
}

// ok / fail glyphs for one-shot result messages.
func okGlyph() string   { return green("✓") }
func failGlyph() string { return red("✗") }
