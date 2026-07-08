package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestDisplayWidthIgnoresANSI(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"plain", 5},
		{ansiBold + "hi" + ansiReset, 2},
		{ansiGreen + "healthy" + ansiReset, 7},
		{"", 0},
		{"café", 4}, // multibyte rune counts as one cell
	}
	for _, c := range cases {
		if got := displayWidth(c.in); got != c.want {
			t.Errorf("displayWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// padCell must pad based on visible width, so colored and plain cells of the
// same logical length line up. This guards the original alignment bug where
// ANSI escape bytes were counted toward column width.
func TestPadCellAlignsColoredAndPlain(t *testing.T) {
	colored := padCell(green("ok"), 10)
	plain := padCell("ok", 10)
	if displayWidth(colored) != displayWidth(plain) {
		t.Fatalf("visible widths differ: colored=%d plain=%d",
			displayWidth(colored), displayWidth(plain))
	}
	if displayWidth(colored) != 10 {
		t.Fatalf("padCell visible width = %d, want 10", displayWidth(colored))
	}
}

func TestTruncateVisible(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"short", 10, "short"},
		{"toolongvalue", 6, "toolo…"},
		{"exact", 5, "exact"},
		{"x", 1, "x"},
		{"xy", 1, "…"},
	}
	for _, c := range cases {
		if got := truncateVisible(c.in, c.width); got != c.want {
			t.Errorf("truncateVisible(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
	}
}

// The table's non-flex columns must be perfectly aligned regardless of whether
// cells contain ANSI color, and the flex (last) column must not wrap.
func TestTableAlignsColoredColumns(t *testing.T) {
	colorDisabled = false // exercise the colored path deterministically
	var buf bytes.Buffer
	tbl := newTable("NAME", "STATUS", "N")
	tbl.alignRight(2)
	tbl.row("alpha", statusDot("healthy"), 1)
	tbl.row("b", statusDot("unhealthy"), 20)
	tbl.flush(&buf)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines (header + 2 rows), got %d:\n%s", len(lines), buf.String())
	}
	// The STATUS column starts at the same visible offset on every row.
	off0 := visibleIndexOf(lines[1], "healthy")
	off1 := visibleIndexOf(lines[2], "unhealthy")
	if off0 <= 0 || off0 != off1 {
		t.Errorf("STATUS column misaligned: row1 offset=%d row2 offset=%d\n%s",
			off0, off1, buf.String())
	}
}

// visibleIndexOf returns the visible-cell index at which sub first appears in s,
// ignoring ANSI escape sequences. Returns -1 if not found.
func visibleIndexOf(s, sub string) int {
	visible := make([]rune, 0, len(s))
	inEsc := false
	for _, r := range s {
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			continue
		}
		visible = append(visible, r)
	}
	idx := strings.Index(string(visible), sub)
	if idx < 0 {
		return -1
	}
	// strings.Index returns a byte offset; convert to a rune offset.
	return len([]rune(string(visible)[:idx]))
}

func TestKVSectionAlignsColons(t *testing.T) {
	var buf bytes.Buffer
	newKV("").add("Short", "a").add("MuchLongerKey", "b").flush(&buf)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	c0 := strings.Index(lines[0], ":")
	c1 := strings.Index(lines[1], ":")
	if c0 != c1 {
		t.Errorf("colons not aligned: %d vs %d\n%s", c0, c1, buf.String())
	}
}
