// Command pebbledb runs SQL queries over CSV files.
//
//	pebbledb data/*.csv          # interactive
//	pebbledb -c "SELECT ..." data/*.csv
//
// Each CSV file becomes a table named after the file, and its columns are
// typed by reading the file once at startup.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/shell-mp3/pebbledb/internal/catalog"
	"github.com/shell-mp3/pebbledb/internal/engine"
)

// version is the build's version. It is overridden at release time with
// -ldflags "-X main.version=...".
var version = "dev"

const usage = `pebbledb is a toy SQL engine that queries CSV files.

usage:
  pebbledb [flags] <file.csv | dir | glob>...

examples:
  pebbledb data/
  pebbledb data/*.csv
  pebbledb -c "EXPLAIN SELECT COUNT(*) FROM orders" data/

Each file becomes a table named after it, so data/orders.csv is the table
"orders". Inside the REPL, .help lists the meta-commands.

flags:
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pebbledb: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var (
		command    = flag.String("c", "", "run one statement and exit")
		noOptimize = flag.Bool("no-optimize", false, "skip the optimizer, which is useful for comparing EXPLAIN output")
		maxRows    = flag.Int("max-rows", 100000, "refuse to buffer more than this many result rows (0 for no limit)")
		showVer    = flag.Bool("version", false, "print the version and exit")
	)
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usage)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVer {
		fmt.Println("pebbledb " + version)
		return nil
	}
	if flag.NArg() == 0 {
		flag.Usage()
		return fmt.Errorf("no CSV files given")
	}

	cat, err := catalog.Load(flag.Args())
	if err != nil {
		return err
	}
	eng := &engine.Engine{Catalog: cat, Optimize: !*noOptimize, MaxRows: *maxRows}

	if *command != "" {
		return runOnce(eng, *command, os.Stdout)
	}
	return repl(eng, os.Stdin, os.Stdout)
}

// runOnce executes a single statement, for -c and for piped input.
func runOnce(eng *engine.Engine, sql string, out *os.File) error {
	res, err := eng.Query(sql)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, engine.FormatResult(res))
	return nil
}

// tableList renders the loaded tables one per line, for .tables.
func tableList(eng *engine.Engine) string {
	var sb strings.Builder
	for _, t := range eng.Catalog.Tables() {
		fmt.Fprintf(&sb, "%-12s %d rows, %d columns\n", t.Name, t.Rows, t.Schema.Len())
	}
	return sb.String()
}
