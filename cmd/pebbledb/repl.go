package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chzyer/readline"
	"github.com/shell-mp3/pebbledb/internal/engine"
	"github.com/shell-mp3/pebbledb/internal/parser"
)

const (
	promptNew  = "pebbledb> "
	promptCont = "      ...> "
)

const replHelp = `.help              show this message
.tables            list the loaded tables
.schema            show every table's columns and inferred types
.optimize on|off   turn the optimizer on or off, to compare EXPLAIN output
.timer on|off      report how long each query took
.quit              leave (Ctrl-D also works)

Statements run when a line ends with ";", or as soon as the buffer parses on
its own. An empty line discards a partial statement.

Try:
  EXPLAIN SELECT c.name, o.amount FROM customers c
    JOIN orders o ON c.id = o.customer_id
    WHERE c.country = 'CA' AND o.amount > 50;`

// lineReader is the part of the REPL that differs between a terminal and a
// pipe, so that the loop itself can be tested with a plain string reader.
type lineReader interface {
	// ReadLine returns the next line, or io.EOF at end of input.
	ReadLine(prompt string) (string, error)
	// AddHistory records a completed statement. It is a no-op when there is
	// no terminal.
	AddHistory(line string)
	Close() error
}

// repl runs the interactive loop. It uses readline when stdin is a terminal,
// so arrow keys, Ctrl-R and a persistent history file work, and falls back to
// a plain scanner when input is piped, which is what makes the shell examples
// in the README testable.
func repl(eng *engine.Engine, in *os.File, out *os.File) error {
	reader, interactive := newLineReader(in, out)
	defer reader.Close()

	if interactive {
		fmt.Fprintf(out, "pebbledb %s: %s\n", version, strings.Join(eng.Catalog.Names(), ", "))
		fmt.Fprintln(out, `Type .help for commands, .quit to leave.`)
	}

	state := &replState{eng: eng, out: out}
	for {
		prompt := promptNew
		if state.buffered() {
			prompt = promptCont
		}
		line, err := reader.ReadLine(prompt)
		if err != nil {
			if err == io.EOF {
				if interactive {
					fmt.Fprintln(out)
				}
				return nil
			}
			// readline returns this when the user presses Ctrl-C, which
			// should abandon the partial statement rather than exit.
			if err == readline.ErrInterrupt {
				state.reset()
				continue
			}
			return err
		}
		if stop := state.feed(line, reader); stop {
			return nil
		}
	}
}

// replState holds the multi-line statement buffer and the session settings.
type replState struct {
	eng   *engine.Engine
	out   *os.File
	lines []string
	timer bool
}

func (s *replState) buffered() bool { return len(s.lines) > 0 }

func (s *replState) reset() { s.lines = nil }

// feed consumes one input line and reports whether the REPL should stop.
func (s *replState) feed(line string, reader lineReader) bool {
	trimmed := strings.TrimSpace(line)

	// A meta-command only makes sense at the start of a statement, so that a
	// line beginning with a dot inside a query is not swallowed.
	if !s.buffered() && strings.HasPrefix(trimmed, ".") {
		reader.AddHistory(trimmed)
		return s.meta(trimmed)
	}
	if trimmed == "" {
		// An empty line throws away whatever was half-typed, which is the
		// only way out of a statement with an unclosed quote.
		if s.buffered() {
			fmt.Fprintln(s.out, "(discarded)")
		}
		s.reset()
		return false
	}

	s.lines = append(s.lines, line)
	stmt := strings.Join(s.lines, "\n")

	// A statement runs when it ends with a semicolon, or when it is a single
	// line that already parses.
	//
	// The single-line shortcut is what makes "SELECT 1 FROM t" work without a
	// semicolon, and it is deliberately limited to one line: a multi-line
	// query often parses before it is finished, and
	//
	//	SELECT a FROM t
	//	WHERE b = 1
	//
	// would otherwise run after the first line and then choke on the second.
	if !strings.HasSuffix(strings.TrimSpace(stmt), ";") {
		if len(s.lines) > 1 {
			return false
		}
		if _, err := parser.Parse(stmt); err != nil {
			return false
		}
	}

	s.reset()
	reader.AddHistory(strings.TrimSpace(stmt))
	s.execute(stmt)
	return false
}

// execute runs one statement and prints the result or the error.
func (s *replState) execute(stmt string) {
	start := time.Now()
	res, err := s.eng.Query(stmt)
	if err != nil {
		fmt.Fprintln(s.out, "error: "+err.Error())
		return
	}
	fmt.Fprintln(s.out, engine.FormatResult(res))
	if s.timer {
		fmt.Fprintf(s.out, "time: %s (execution %s)\n", round(time.Since(start)), round(res.Elapsed))
	}
}

// meta handles a dot-command and reports whether to stop.
func (s *replState) meta(line string) bool {
	fields := strings.Fields(line)
	arg := ""
	if len(fields) > 1 {
		arg = strings.ToLower(fields[1])
	}
	switch strings.ToLower(fields[0]) {
	case ".quit", ".exit":
		return true
	case ".help":
		fmt.Fprintln(s.out, replHelp)
	case ".tables":
		fmt.Fprint(s.out, tableList(s.eng))
	case ".schema":
		fmt.Fprint(s.out, s.eng.Schemas())
	case ".optimize":
		if on, ok := onOff(arg); ok {
			s.eng.Optimize = on
			fmt.Fprintf(s.out, "optimizer %s\n", stateWord(on))
		} else {
			fmt.Fprintf(s.out, "optimizer is %s; use .optimize on or .optimize off\n", stateWord(s.eng.Optimize))
		}
	case ".timer":
		if on, ok := onOff(arg); ok {
			s.timer = on
			fmt.Fprintf(s.out, "timer %s\n", stateWord(on))
		} else {
			fmt.Fprintf(s.out, "timer is %s; use .timer on or .timer off\n", stateWord(s.timer))
		}
	default:
		fmt.Fprintf(s.out, "unknown command %s; try .help\n", fields[0])
	}
	return false
}

func onOff(arg string) (bool, bool) {
	switch arg {
	case "on", "true", "1":
		return true, true
	case "off", "false", "0":
		return false, true
	}
	return false, false
}

func stateWord(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// round trims a duration to something readable in a terminal.
func round(d time.Duration) time.Duration {
	switch {
	case d < time.Microsecond:
		return d
	case d < time.Millisecond:
		return d.Round(10 * time.Nanosecond)
	case d < time.Second:
		return d.Round(10 * time.Microsecond)
	}
	return d.Round(time.Millisecond)
}

// newLineReader returns a readline-backed reader on a terminal and a plain
// one otherwise, reporting which it chose.
func newLineReader(in, out *os.File) (lineReader, bool) {
	if !isTerminal(in) {
		return &plainReader{scanner: bufio.NewScanner(in)}, false
	}
	rl, err := readline.NewEx(&readline.Config{
		Prompt:            promptNew,
		HistoryFile:       historyFile(),
		HistoryLimit:      1000,
		InterruptPrompt:   "^C",
		EOFPrompt:         "",
		HistorySearchFold: true,
		Stdout:            out,
	})
	if err != nil {
		// A terminal that readline cannot drive still gets a working REPL,
		// just without history.
		fmt.Fprintf(out, "note: history is unavailable (%v)\n", err)
		return &plainReader{scanner: bufio.NewScanner(in)}, true
	}
	return &readlineReader{rl: rl}, true
}

// isTerminal reports whether f is a console rather than a pipe or a file.
//
// This is checked here rather than with readline's own helper, which looks at
// the process's standard handles and reports a terminal even when stdin has
// been redirected. Getting it wrong is not cosmetic: readline would then take
// over the terminal and read nothing.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// historyFile puts the history next to the user's other dotfiles, falling
// back to the working directory when there is no home directory.
func historyFile() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".pebbledb_history")
	}
	return ".pebbledb_history"
}

type readlineReader struct{ rl *readline.Instance }

func (r *readlineReader) ReadLine(prompt string) (string, error) {
	r.rl.SetPrompt(prompt)
	line, err := r.rl.Readline()
	if err == readline.ErrInterrupt {
		return "", readline.ErrInterrupt
	}
	return line, err
}

func (r *readlineReader) AddHistory(line string) {
	if line != "" {
		_ = r.rl.SaveHistory(line)
	}
}

func (r *readlineReader) Close() error { return r.rl.Close() }

// plainReader reads lines without any terminal handling, for piped input.
type plainReader struct{ scanner *bufio.Scanner }

func (p *plainReader) ReadLine(string) (string, error) {
	if !p.scanner.Scan() {
		if err := p.scanner.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	return p.scanner.Text(), nil
}

func (p *plainReader) AddHistory(string) {}

func (p *plainReader) Close() error { return nil }
