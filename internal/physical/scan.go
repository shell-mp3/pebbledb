package physical

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/shell-mp3/pebbledb/internal/catalog"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// Scan reads rows from a CSV file, converting only the columns the plan asked
// for. Everything else in the record is skipped without being parsed, which
// is the payoff from projection pruning.
type Scan struct {
	stats
	table  *catalog.Table
	alias  string
	cols   []int
	schema *types.Schema

	reader *csv.Reader
	closer io.Closer
	// row is reused between calls; buffering operators clone it.
	row types.Row
	// line counts data rows, for error messages that point at the file.
	line int
}

// NewScan builds a scan of the given table columns, by index into the table's
// own schema. An empty cols slice is valid and means no field is parsed at
// all, which is what SELECT COUNT(*) needs.
func NewScan(table *catalog.Table, alias string, cols []int) *Scan {
	schemaCols := make([]types.Column, len(cols))
	for i, j := range cols {
		c := table.Schema.Columns[j]
		c.Table = alias
		schemaCols[i] = c
	}
	return &Scan{
		table:  table,
		alias:  alias,
		cols:   cols,
		schema: types.NewSchema(schemaCols...),
		row:    make(types.Row, len(cols)),
	}
}

// Open opens the file and skips the header.
func (s *Scan) Open() error {
	return s.measureOpen(func() error {
		r, _, closer, err := catalog.NewReader(s.table.Path)
		if err != nil {
			return err
		}
		s.reader, s.closer, s.line = r, closer, 0
		return nil
	})
}

// Next reads one record and converts the selected fields.
func (s *Scan) Next() (types.Row, bool, error) { return s.measure(s.next) }

func (s *Scan) next() (types.Row, bool, error) {
	if s.reader == nil {
		return nil, false, errors.New("Scan.Next called before Open")
	}
	rec, err := s.reader.Read()
	if errors.Is(err, io.EOF) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", s.table.Path, err)
	}
	s.line++
	for i, j := range s.cols {
		v, err := catalog.ParseField(rec[j], s.schema.Columns[i].Kind)
		if err != nil {
			return nil, false, fmt.Errorf("%s row %d, column %s: %w", s.table.Path, s.line, s.schema.Columns[i].Name, err)
		}
		s.row[i] = v
	}
	return s.row, true, nil
}

// Close releases the file handle.
func (s *Scan) Close() error {
	if s.closer == nil {
		return nil
	}
	err := s.closer.Close()
	s.closer, s.reader = nil, nil
	return err
}

// Schema returns the scan's output columns.
func (s *Scan) Schema() *types.Schema { return s.schema }

// Children returns nothing: Scan is a leaf.
func (s *Scan) Children() []Operator { return nil }

// ChildLabels returns nothing.
func (s *Scan) ChildLabels() []string { return nil }

// Label spells out how much of the file this scan reads, so the effect of
// projection pruning is visible in EXPLAIN without a diff.
func (s *Scan) Label() string {
	name := s.table.Name
	if !strings.EqualFold(s.alias, s.table.Name) {
		name += " AS " + s.alias
	}
	names := make([]string, len(s.cols))
	for i, j := range s.cols {
		names[i] = s.table.Schema.Columns[j].Name
	}
	return fmt.Sprintf("Scan(%s) reads %d of %d columns [%s], %d rows",
		name, len(s.cols), s.table.Schema.Len(), strings.Join(names, ", "), s.table.Rows)
}
