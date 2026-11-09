// Package catalog turns CSV files into tables the planner can reason about.
//
// A CSV file carries no type information, so the catalog reads each file once
// at startup to infer a type per column and to count rows. That cost buys two
// things the engine needs: comparisons that work on numbers rather than text,
// and a row count for EXPLAIN to report. The data itself is not cached; Scan
// re-reads the file every time it is opened.
package catalog

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/shell-mp3/pebbledb/internal/types"
)

// Table is one CSV file registered under a name.
type Table struct {
	// Name is the file's base name without its extension, lower-cased.
	Name string
	// Path is the file the Scan operator reads.
	Path string
	// Schema describes the columns, qualified by Name.
	Schema *types.Schema
	// Rows is the number of data rows, excluding the header. EXPLAIN reports
	// it as the scan's estimated cardinality.
	Rows int64
}

// Catalog is the set of tables available to a query.
type Catalog struct {
	byName map[string]*Table
}

// New returns an empty catalog.
func New() *Catalog { return &Catalog{byName: map[string]*Table{}} }

// Load builds a catalog from file and directory paths. A directory
// contributes every .csv file directly inside it; a glob pattern is expanded,
// which matters on Windows where the shell does not do it.
func Load(paths []string) (*Catalog, error) {
	files, err := expand(paths)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("no CSV files found")
	}
	c := New()
	for _, f := range files {
		if err := c.Add(f); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// expand resolves directories and glob patterns to a sorted, deduplicated
// list of CSV files, so the catalog is identical however it was invoked.
func expand(paths []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		switch {
		case err == nil && info.IsDir():
			entries, err := filepath.Glob(filepath.Join(p, "*.csv"))
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				add(e)
			}
		case err == nil:
			add(p)
		default:
			matches, gerr := filepath.Glob(p)
			if gerr != nil {
				return nil, fmt.Errorf("bad pattern %q: %w", p, gerr)
			}
			if len(matches) == 0 {
				return nil, fmt.Errorf("cannot open %q: %w", p, err)
			}
			for _, m := range matches {
				add(m)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// Add registers one CSV file under its base name, inferring its schema.
func (c *Catalog) Add(path string) error {
	name := strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	if name == "" {
		return fmt.Errorf("cannot derive a table name from %q", path)
	}
	if prev, ok := c.byName[name]; ok {
		return fmt.Errorf("two files map to table %q: %s and %s", name, prev.Path, path)
	}
	t, err := inferTable(name, path)
	if err != nil {
		return err
	}
	c.byName[name] = t
	return nil
}

// Get looks up a table by name, case-insensitively.
func (c *Catalog) Get(name string) (*Table, error) {
	t, ok := c.byName[strings.ToLower(name)]
	if !ok {
		return nil, fmt.Errorf("no such table: %s (loaded: %s)", name, strings.Join(c.Names(), ", "))
	}
	return t, nil
}

// Names returns the table names in sorted order.
func (c *Catalog) Names() []string {
	out := make([]string, 0, len(c.byName))
	for n := range c.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Tables returns the tables in sorted name order.
func (c *Catalog) Tables() []*Table {
	out := make([]*Table, 0, len(c.byName))
	for _, n := range c.Names() {
		out = append(out, c.byName[n])
	}
	return out
}

// NewReader opens path as a CSV reader positioned after the header, and
// returns the header it skipped. Both the catalog and the Scan operator use
// it, so they always agree on how a file is read.
func NewReader(path string) (*csv.Reader, []string, io.Closer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	r := csv.NewReader(f)
	// Require a rectangular file: a short row is a data error, not a row of
	// NULLs. ReuseRecord avoids one allocation per row.
	r.FieldsPerRecord = 0
	r.ReuseRecord = true
	header, err := r.Read()
	if err != nil {
		f.Close()
		if errors.Is(err, io.EOF) {
			return nil, nil, nil, fmt.Errorf("%s: file is empty (a CSV table needs a header row)", path)
		}
		return nil, nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	// ReuseRecord means the slice is overwritten by the next Read.
	cols := make([]string, len(header))
	copy(cols, header)
	return r, cols, f, nil
}

// inferTable reads the whole file to decide each column's type and to count
// rows. Inference widens: a column stays int until a value does not parse as
// one, then becomes float, then string. A column that is entirely empty is
// typed as string, since there is nothing to go on.
func inferTable(name, path string) (*Table, error) {
	r, header, closer, err := NewReader(path)
	if err != nil {
		return nil, err
	}
	defer closer.Close()

	if err := checkHeader(path, header); err != nil {
		return nil, err
	}

	kinds := make([]types.Kind, len(header))
	for i := range kinds {
		kinds[i] = types.KindInt
	}
	seen := make([]bool, len(header))

	var rows int64
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		rows++
		for i, field := range rec {
			if field == "" {
				continue
			}
			seen[i] = true
			kinds[i] = widen(kinds[i], field)
		}
	}

	cols := make([]types.Column, len(header))
	for i, h := range header {
		k := kinds[i]
		if !seen[i] {
			k = types.KindString
		}
		cols[i] = types.Column{Table: name, Name: h, Kind: k}
	}
	return &Table{Name: name, Path: path, Schema: types.NewSchema(cols...), Rows: rows}, nil
}

// checkHeader rejects headers that would make queries impossible to write:
// an empty column name, or the same name twice.
func checkHeader(path string, header []string) error {
	seen := map[string]int{}
	for i, h := range header {
		if strings.TrimSpace(h) == "" {
			return fmt.Errorf("%s: column %d in the header has no name", path, i+1)
		}
		key := strings.ToLower(h)
		if prev, ok := seen[key]; ok {
			return fmt.Errorf("%s: header has duplicate column %q (columns %d and %d)", path, h, prev+1, i+1)
		}
		seen[key] = i
	}
	return nil
}

// widen returns the narrowest kind that can hold both the current kind and
// the new field.
func widen(k types.Kind, field string) types.Kind {
	switch k {
	case types.KindInt:
		if _, err := strconv.ParseInt(field, 10, 64); err == nil {
			return types.KindInt
		}
		if isFloat(field) {
			return types.KindFloat
		}
		return types.KindString
	case types.KindFloat:
		if isFloat(field) {
			return types.KindFloat
		}
		return types.KindString
	}
	return types.KindString
}

// isFloat rejects the forms Go's parser accepts but CSV data should not use,
// so that a column of hex-looking product codes is not mistaken for numbers.
func isFloat(field string) bool {
	if strings.ContainsAny(field, "xXpP_") {
		return false
	}
	f, err := strconv.ParseFloat(field, 64)
	if err != nil {
		return false
	}
	// Inf and NaN spellings are words, not numbers, in a data file.
	if f != f || f > 1e308 || f < -1e308 {
		return false
	}
	return true
}

// ParseField converts one CSV field to a Value of the given kind. An empty
// field is NULL: a CSV file cannot distinguish an empty string from a missing
// value, and treating it as NULL is the more useful of the two readings.
func ParseField(field string, kind types.Kind) (types.Value, error) {
	if field == "" {
		return types.Null, nil
	}
	switch kind {
	case types.KindInt:
		n, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return types.Null, fmt.Errorf("value %q is not an integer", field)
		}
		return types.NewInt(n), nil
	case types.KindFloat:
		f, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return types.Null, fmt.Errorf("value %q is not a number", field)
		}
		return types.NewFloat(f), nil
	}
	return types.NewString(field), nil
}
