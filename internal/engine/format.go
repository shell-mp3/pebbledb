package engine

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// FormatResult renders a result set as an aligned table. The REPL and the
// golden files share it, so what the tests assert on is exactly what a user
// sees.
//
// Numbers are right-aligned and everything else is left-aligned, which is
// what makes a column of amounts readable at a glance.
func FormatResult(r *Result) string {
	if r.IsExplain() {
		out := r.Plan
		if len(r.Rules) > 0 {
			out += "\n\nrules applied: " + strings.Join(r.Rules, ", ")
		}
		return out
	}

	cells := make([][]string, len(r.Rows))
	for i, row := range r.Rows {
		cells[i] = make([]string, len(r.Columns))
		for j := range r.Columns {
			if j < len(row) {
				cells[i][j] = row[j].String()
			}
		}
	}

	widths := make([]int, len(r.Columns))
	for i, name := range r.Columns {
		widths[i] = utf8.RuneCountInString(name)
	}
	for _, row := range cells {
		for j, cell := range row {
			if n := utf8.RuneCountInString(cell); n > widths[j] {
				widths[j] = n
			}
		}
	}

	// Lines are assembled and then right-trimmed, so padding the last column
	// never leaves trailing whitespace in a golden file.
	var lines []string
	var line strings.Builder
	for i, name := range r.Columns {
		if i > 0 {
			line.WriteString(" | ")
		}
		line.WriteString(pad(name, widths[i], false))
	}
	lines = append(lines, line.String())

	line.Reset()
	for i := range r.Columns {
		if i > 0 {
			line.WriteString("-+-")
		}
		line.WriteString(strings.Repeat("-", widths[i]))
	}
	lines = append(lines, line.String())

	for _, row := range cells {
		line.Reset()
		for j, cell := range row {
			if j > 0 {
				line.WriteString(" | ")
			}
			line.WriteString(pad(cell, widths[j], r.Kinds[j].IsNumeric()))
		}
		lines = append(lines, line.String())
	}
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	lines = append(lines, fmt.Sprintf("(%s)", plural(len(r.Rows), "row")))
	return strings.Join(lines, "\n")
}

// pad fits s to width, on the right for numbers and on the left otherwise.
// It counts runes rather than bytes, but still assumes one cell per rune, so
// a double-width character misaligns the table. See the README.
func pad(s string, width int, right bool) string {
	gap := width - utf8.RuneCountInString(s)
	if gap <= 0 {
		return s
	}
	if right {
		return strings.Repeat(" ", gap) + s
	}
	return s + strings.Repeat(" ", gap)
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
