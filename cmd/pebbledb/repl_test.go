package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shell-mp3/pebbledb/internal/catalog"
	"github.com/shell-mp3/pebbledb/internal/engine"
)

// scriptReader replays canned input, so a whole REPL session can be driven
// from a test without a terminal.
type scriptReader struct {
	lines   []string
	pos     int
	prompts []string
	history []string
}

func (s *scriptReader) ReadLine(prompt string) (string, error) {
	if s.pos >= len(s.lines) {
		return "", io.EOF
	}
	s.prompts = append(s.prompts, prompt)
	line := s.lines[s.pos]
	s.pos++
	return line, nil
}

func (s *scriptReader) AddHistory(line string) { s.history = append(s.history, line) }

func (s *scriptReader) Close() error { return nil }

// session runs the REPL over the given input lines and returns everything it
// printed, plus the reader so prompts and history can be inspected.
func session(t *testing.T, lines ...string) (string, *scriptReader) {
	t.Helper()
	cat, err := catalog.Load([]string{filepath.Join("..", "..", "data")})
	if err != nil {
		t.Fatalf("loading data/: %v", err)
	}
	reader := &scriptReader{lines: lines}
	var out bytes.Buffer
	if err := runREPL(engine.New(cat), reader, &out, false); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	return out.String(), reader
}

func TestStripPrompt(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"SELECT 1 FROM orders", "SELECT 1 FROM orders"},
		{"pebbledb> SELECT 1 FROM orders", "SELECT 1 FROM orders"},
		{"pebbledb>SELECT 1 FROM orders", "SELECT 1 FROM orders"},
		{"     ...> SELECT 1 FROM orders", "SELECT 1 FROM orders"},
		{"...> SELECT 1 FROM orders", "SELECT 1 FROM orders"},
		// Copying a line that was itself a paste doubles the prompt.
		{"pebbledb> pebbledb> SELECT 1 FROM orders", "SELECT 1 FROM orders"},
		{"$ pebbledb -c \"SELECT 1\"", "pebbledb -c \"SELECT 1\""},
		{"pebbledb>", ""},
		{"", ""},
		// Indentation inside a statement is preserved when there is no prompt
		// to strip, since it is the user's own formatting.
		{"  JOIN orders o ON c.id = o.customer_id", "  JOIN orders o ON c.id = o.customer_id"},
	}
	for _, tc := range tests {
		if got := stripPrompt(tc.in); got != tc.want {
			t.Errorf("stripPrompt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShellCommandDetection(t *testing.T) {
	shell := []string{
		"go run ./cmd/pebbledb data/",
		"GO RUN ./cmd/pebbledb data/",
		"cd pebbledb",
		"pebbledb -c \"SELECT COUNT(*) FROM orders\" data/",
		"./pebbledb data/",
		"git status",
	}
	for _, line := range shell {
		if _, ok := shellCommand(line); !ok {
			t.Errorf("shellCommand(%q) = false, want true", line)
		}
	}
	sql := []string{
		"SELECT COUNT(*) FROM orders",
		"explain select 1 from orders",
		"",
		"  ",
		// "go" only matters as the first word; a column could be named for it.
		"SELECT go FROM orders",
	}
	for _, line := range sql {
		if word, ok := shellCommand(line); ok {
			t.Errorf("shellCommand(%q) flagged %q as a shell command", line, word)
		}
	}
}

// TestPastedShellCommandIsRejectedImmediately is the bug this all exists for.
// The shell command must not be buffered, because an unfinished statement
// swallows every line pasted after it.
func TestPastedShellCommandIsRejectedImmediately(t *testing.T) {
	out, reader := session(t,
		`go run ./cmd/pebbledb -c "SELECT COUNT(*) FROM orders" data/`,
		"SELECT COUNT(*) FROM orders;",
	)
	if !strings.Contains(out, `"go" is a shell command, not SQL`) {
		t.Errorf("no useful message for a pasted shell command:\n%s", out)
	}
	if !strings.Contains(out, "      30") {
		t.Errorf("the query after the shell command did not run:\n%s", out)
	}
	// The second line must have been read at a fresh prompt, not a
	// continuation one.
	for _, p := range reader.prompts {
		if p == promptCont {
			t.Errorf("a shell command left a half-finished statement behind; prompts were %q", reader.prompts)
			break
		}
	}
}

// TestPastedTranscriptRuns covers the other half: someone copies a block
// straight out of their terminal, prompts and all.
func TestPastedTranscriptRuns(t *testing.T) {
	out, _ := session(t,
		"pebbledb> SELECT COUNT(*) FROM orders;",
		"pebbledb> .tables",
	)
	if !strings.Contains(out, "      30") {
		t.Errorf("a pasted query with its prompt did not run:\n%s", out)
	}
	if !strings.Contains(out, "customers") {
		t.Errorf("a pasted meta-command with its prompt did not run:\n%s", out)
	}
}

// TestPastedBlockArrivesInOnePiece covers a terminal that hands the whole
// paste over as a single line with newlines still in it.
func TestPastedBlockArrivesInOnePiece(t *testing.T) {
	block := strings.Join([]string{
		"SELECT c.name, o.amount FROM customers c",
		"  JOIN orders o ON c.id = o.customer_id",
		"  WHERE c.country = 'CA' AND o.amount > 100;",
		"",
	}, "\r\n")
	out, _ := session(t, block)
	if !strings.Contains(out, "Lena Berg") {
		t.Errorf("a one-piece paste did not run as a statement:\n%s", out)
	}
	if strings.Contains(out, "error") {
		t.Errorf("a one-piece paste produced an error:\n%s", out)
	}
	// The trailing newline must not have discarded anything.
	if strings.Contains(out, "(discarded)") {
		t.Errorf("the paste's trailing newline discarded the statement:\n%s", out)
	}
}

func TestMultiLineStatementNeedsASemicolon(t *testing.T) {
	out, reader := session(t,
		"SELECT c.name, o.amount FROM customers c",
		"  JOIN orders o ON c.id = o.customer_id",
		"  WHERE c.country = 'CA' AND o.amount > 100;",
	)
	if !strings.Contains(out, "Lena Berg") {
		t.Errorf("multi-line statement did not run:\n%s", out)
	}
	// Lines two and three must have been read at the continuation prompt.
	want := []string{promptNew, promptCont, promptCont}
	for i, p := range want {
		if i >= len(reader.prompts) || reader.prompts[i] != p {
			t.Fatalf("prompts = %q, want %q", reader.prompts, want)
		}
	}
	if len(reader.history) != 1 {
		t.Errorf("history = %q, want the whole statement as one entry", reader.history)
	}
}

// TestEndOfInputRunsAPartialStatement is what keeps piping a script in
// working, since the last line of
//
//	echo "SELECT COUNT(*) FROM orders" | pebbledb data/
//
// has no semicolon.
func TestEndOfInputRunsAPartialStatement(t *testing.T) {
	out, _ := session(t, "SELECT COUNT(*) FROM orders")
	if !strings.Contains(out, "      30") {
		t.Errorf("end of input did not run the buffered statement:\n%s", out)
	}
}

// TestFirstLineOfAMultiLineQueryDoesNotRunAlone is the regression the
// semicolon rule exists for. The first line parses by itself, so an "it
// parses, so run it" shortcut executes it and fails on a column the JOIN has
// not introduced yet.
func TestFirstLineOfAMultiLineQueryDoesNotRunAlone(t *testing.T) {
	out, _ := session(t,
		"SELECT c.name, o.amount FROM customers c",
		"  JOIN orders o ON c.id = o.customer_id",
		"  WHERE o.amount > 150;",
	)
	if strings.Contains(out, "no such column: o.amount") {
		t.Errorf("the first line ran on its own:\n%s", out)
	}
	if !strings.Contains(out, "Lena Berg") {
		t.Errorf("the whole statement did not run:\n%s", out)
	}
}

// TestDotCommandDiscardsAnUnfinishedStatement covers the usual way people get
// stuck: forget the semicolon, then type a dot-command.
func TestDotCommandDiscardsAnUnfinishedStatement(t *testing.T) {
	out, _ := session(t, "SELECT COUNT(*) FROM orders", ".tables")
	if !strings.Contains(out, "discarded the unfinished statement") {
		t.Errorf("no notice that the statement was dropped:\n%s", out)
	}
	if !strings.Contains(out, "customers") {
		t.Errorf(".tables did not run:\n%s", out)
	}
}

func TestEmptyLineDiscardsAPartialStatement(t *testing.T) {
	out, _ := session(t,
		"SELECT c.name FROM customers c",
		"  WHERE c.country =",
		"",
		"SELECT COUNT(*) FROM orders",
	)
	if !strings.Contains(out, "(discarded)") {
		t.Errorf("an empty line did not discard the partial statement:\n%s", out)
	}
	if !strings.Contains(out, "      30") {
		t.Errorf("the REPL did not recover after discarding:\n%s", out)
	}
}

func TestMetaCommands(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{".tables", "orders"},
		{".schema", "unit_price"},
		{".help", ".optimize on|off"},
		{".optimize off", "optimizer off"},
		{".optimize", "optimizer is on"},
		{".timer on", "timer on"},
		{".nonsense", "unknown command .nonsense"},
	}
	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			out, _ := session(t, tc.line)
			if !strings.Contains(out, tc.want) {
				t.Errorf("%s printed:\n%s\nwant it to mention %q", tc.line, out, tc.want)
			}
		})
	}
}

func TestOptimizeToggleChangesThePlan(t *testing.T) {
	query := "EXPLAIN SELECT c.name FROM customers c JOIN orders o ON c.id = o.customer_id WHERE c.country = 'CA';"
	out, _ := session(t, ".optimize off", query, ".optimize on", query)

	// The unoptimized plan keeps the filter above the join; the optimized one
	// pushes it onto the customers scan.
	if !strings.Contains(out, "└─ Filter(c.country = 'CA')\n   └─ HashJoin") {
		t.Errorf("the unoptimized plan is missing:\n%s", out)
	}
	if !strings.Contains(out, "probe: Filter(c.country = 'CA')") {
		t.Errorf("the optimized plan is missing:\n%s", out)
	}
	if n := strings.Count(out, "rules applied:"); n != 1 {
		t.Errorf("rules were reported %d times, want once (only with the optimizer on):\n%s", n, out)
	}
}

func TestQuitStopsTheLoop(t *testing.T) {
	out, reader := session(t, ".quit", "SELECT COUNT(*) FROM orders;")
	if strings.Contains(out, "      30") {
		t.Errorf(".quit did not stop the loop:\n%s", out)
	}
	if reader.pos != 1 {
		t.Errorf("read %d lines after .quit, want 1", reader.pos)
	}
}

func TestQueryErrorsArePrintedAndRecoverable(t *testing.T) {
	out, _ := session(t, "SELECT nope FROM orders;", "SELECT COUNT(*) FROM orders;")
	if !strings.Contains(out, "error: no such column: nope") {
		t.Errorf("error not reported:\n%s", out)
	}
	if !strings.Contains(out, "      30") {
		t.Errorf("the REPL did not carry on after an error:\n%s", out)
	}
}

func TestBannerOnlyWhenInteractive(t *testing.T) {
	cat, err := catalog.Load([]string{filepath.Join("..", "..", "data")})
	if err != nil {
		t.Fatalf("loading data/: %v", err)
	}
	var out bytes.Buffer
	if err := runREPL(engine.New(cat), &scriptReader{}, &out, true); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if !strings.Contains(out.String(), "Type .help for commands") {
		t.Errorf("no banner in an interactive session:\n%s", out.String())
	}
}

func TestRunOnce(t *testing.T) {
	cat, err := catalog.Load([]string{filepath.Join("..", "..", "data")})
	if err != nil {
		t.Fatalf("loading data/: %v", err)
	}
	var out bytes.Buffer
	if err := runOnce(engine.New(cat), "SELECT COUNT(*) FROM orders", &out); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if !strings.Contains(out.String(), "      30") {
		t.Errorf("runOnce printed:\n%s", out.String())
	}
	if err := runOnce(engine.New(cat), "SELECT nope FROM orders", &out); err == nil {
		t.Error("runOnce returned no error for a bad query")
	}
}

func TestTableList(t *testing.T) {
	cat, err := catalog.Load([]string{filepath.Join("..", "..", "data")})
	if err != nil {
		t.Fatalf("loading data/: %v", err)
	}
	got := tableList(engine.New(cat))
	for _, want := range []string{"customers    12 rows, 5 columns", "orders       30 rows, 6 columns", "products     8 rows, 4 columns"} {
		if !strings.Contains(got, want) {
			t.Errorf(".tables output is missing %q:\n%s", want, got)
		}
	}
}
