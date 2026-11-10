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
)

const (
	promptNew  = "pebbledb> "
	promptCont = "     ...> "
)

const replHelp = `.help              show this message
.tables            list the loaded tables
.schema            show every table's columns and inferred types
.optimize on|off   turn the optimizer on or off, to compare EXPLAIN output
.timer on|off      report how long each query took
.quit              leave (Ctrl-D also works)

Statements end with ";", like psql and sqlite3. Until then the prompt stays
"...>". An empty line discards an unfinished statement, which is how you get
out of one you no longer want.

Pasting a block is fine, and a copied "pebbledb>" or "...>" prompt in front of
a line is ignored.

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
func repl(eng *engine.Engine, in *os.File, out io.Writer) error {
	reader, interactive := newLineReader(in, out)
	defer reader.Close()
	return runREPL(eng, reader, out, interactive)
}

// runREPL is the loop itself, over any lineReader, so the whole session can be
// driven from a test with a scripted reader.
func runREPL(eng *engine.Engine, reader lineReader, out io.Writer, interactive bool) error {
	if interactive {
		fmt.Fprintf(out, "pebbledb %s: %s\n", version, strings.Join(eng.Catalog.Names(), ", "))
		fmt.Fprintln(out, `Statements end with ";". Type .help for commands, .quit to leave.`)
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
				state.flush()
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
		if stop := state.feedBlock(line, reader); stop {
			return nil
		}
	}
}

// replState holds the multi-line statement buffer and the session settings.
type replState struct {
	eng   *engine.Engine
	out   io.Writer
	lines []string
	timer bool
}

func (s *replState) buffered() bool { return len(s.lines) > 0 }

func (s *replState) reset() { s.lines = nil }

// feedBlock consumes one chunk of input and reports whether the REPL should
// stop.
//
// A chunk is normally one line, but a terminal that hands a paste over in one
// piece delivers the whole block with newlines still in it. Splitting here
// means a pasted query behaves exactly as if it had been typed, instead of
// becoming one enormous statement.
func (s *replState) feedBlock(block string, reader lineReader) bool {
	lines := strings.Split(strings.ReplaceAll(block, "\r\n", "\n"), "\n")
	// A paste usually ends with a newline, which leaves an empty final piece
	// that would otherwise discard the statement the paste just delivered.
	if n := len(lines); n > 1 && strings.TrimSpace(lines[n-1]) == "" {
		lines = lines[:n-1]
	}
	for _, line := range lines {
		if s.feed(line, reader) {
			return true
		}
	}
	return false
}

// feed consumes one input line and reports whether the REPL should stop.
func (s *replState) feed(line string, reader lineReader) bool {
	line = stripPrompt(line)
	trimmed := strings.TrimSpace(line)

	// No SQL continuation line starts with a dot, so a dot-command is always a
	// dot-command. Recognising it even mid-statement matters: the usual way to
	// end up here is forgetting a semicolon and then typing .tables, and
	// silently appending that to the query would be baffling.
	if strings.HasPrefix(trimmed, ".") {
		if s.buffered() {
			fmt.Fprintln(s.out, "(discarded the unfinished statement)")
			s.reset()
		}
		reader.AddHistory(trimmed)
		return s.meta(trimmed)
	}

	// Catch a shell command before it is buffered. Letting it through is what
	// turns one bad paste into a mess: the line never parses, so every line
	// after it is swallowed into the same unfinished statement.
	if !s.buffered() {
		if word, ok := shellCommand(trimmed); ok {
			fmt.Fprintf(s.out, "error: %q is a shell command, not SQL. Type a query here, or .quit and run it from your shell.\n", word)
			return false
		}
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

	// A statement runs when it ends with a semicolon, and only then, which is
	// what psql, sqlite3 and mysql all do.
	//
	// Running a line as soon as it happens to parse sounds friendlier and is
	// not, because the first line of a multi-line query usually parses on its
	// own:
	//
	//	SELECT c.name, o.amount FROM customers c
	//	  JOIN orders o ON c.id = o.customer_id
	//
	// With that rule the first line runs by itself, fails on the unknown
	// column o.amount, and the JOIN line then arrives as a statement of its
	// own. End of input also ends a statement, so piping a script in still
	// works without a trailing semicolon.
	if !strings.HasSuffix(strings.TrimSpace(stmt), ";") {
		return false
	}

	s.reset()
	reader.AddHistory(strings.TrimSpace(stmt))
	s.execute(stmt)
	return false
}

// flush runs whatever is still buffered. End of input ends a statement, so a
// piped script needs no semicolon on its last one.
func (s *replState) flush() {
	if !s.buffered() {
		return
	}
	stmt := strings.Join(s.lines, "\n")
	s.reset()
	s.execute(stmt)
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

// promptPrefixes are the prompts the REPL itself prints, plus a bare shell
// prompt. Stripping them means a transcript copied out of a terminal or out of
// the README can be pasted straight back in, which is exactly what people do.
// No SQL statement starts with any of them.
var promptPrefixes = []string{"pebbledb> ", "pebbledb>", "...> ", "...>", "$ "}

// stripPrompt removes any prompts a pasted line carries, including repeated
// ones, since copying a line that was already a paste gives "pebbledb>
// pebbledb> ...".
func stripPrompt(line string) string {
	for {
		trimmed := strings.TrimLeft(line, " \t")
		matched := false
		for _, p := range promptPrefixes {
			if strings.HasPrefix(trimmed, p) {
				line, matched = trimmed[len(p):], true
				break
			}
		}
		if !matched {
			return line
		}
	}
}

// shellCommands are first words that mean a shell command was pasted at the
// SQL prompt. None of them can begin a valid statement, so recognising them
// costs nothing and the message is far better than a parse error pointing at
// column 1.
var shellCommands = map[string]bool{
	"go": true, "git": true, "cd": true, "ls": true, "dir": true,
	"cat": true, "type": true, "echo": true, "make": true,
	"pebbledb": true, "pebbledb.exe": true,
	"./pebbledb": true, `.\pebbledb`: true, `.\pebbledb.exe`: true,
	"sqlite3": true, "psql": true, "mysql": true,
}

// shellCommand reports whether a statement begins with one of them, returning
// the offending word.
func shellCommand(stmt string) (string, bool) {
	fields := strings.Fields(stmt)
	if len(fields) == 0 {
		return "", false
	}
	word := fields[0]
	return word, shellCommands[strings.ToLower(word)]
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
func newLineReader(in *os.File, out io.Writer) (lineReader, bool) {
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
