package physical

import (
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/eval"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// Filter passes through the rows for which the predicate is TRUE. FALSE and
// UNKNOWN both fail, which is why a NULL amount never satisfies amount > 0.
type Filter struct {
	stats
	input Operator
	pred  *eval.Expr
}

// NewFilter builds a filter over input.
func NewFilter(input Operator, pred *eval.Expr) *Filter {
	return &Filter{input: input, pred: pred}
}

// Open opens the input.
func (f *Filter) Open() error { return f.measureOpen(f.input.Open) }

// Next returns the next passing row.
func (f *Filter) Next() (types.Row, bool, error) { return f.measure(f.next) }

func (f *Filter) next() (types.Row, bool, error) {
	for {
		row, ok, err := f.input.Next()
		if err != nil || !ok {
			return nil, false, err
		}
		v, err := f.pred.Eval(row)
		if err != nil {
			return nil, false, err
		}
		pass, err := types.IsTrue(v)
		if err != nil {
			return nil, false, err
		}
		if pass {
			return row, true, nil
		}
	}
}

// Close closes the input.
func (f *Filter) Close() error { return f.input.Close() }

// Schema passes the input's schema through.
func (f *Filter) Schema() *types.Schema { return f.input.Schema() }

// Children returns the input.
func (f *Filter) Children() []Operator { return []Operator{f.input} }

// ChildLabels returns nothing.
func (f *Filter) ChildLabels() []string { return nil }

// Label shows the predicate.
func (f *Filter) Label() string { return "Filter(" + f.pred.String() + ")" }

// Project evaluates the select list.
type Project struct {
	stats
	input  Operator
	exprs  []*eval.Expr
	schema *types.Schema
	out    types.Row
}

// NewProject builds a projection whose output columns are described by
// schema, in the same order as exprs.
func NewProject(input Operator, exprs []*eval.Expr, schema *types.Schema) *Project {
	return &Project{input: input, exprs: exprs, schema: schema, out: make(types.Row, len(exprs))}
}

// Open opens the input.
func (p *Project) Open() error { return p.measureOpen(p.input.Open) }

// Next projects the next row.
func (p *Project) Next() (types.Row, bool, error) { return p.measure(p.next) }

func (p *Project) next() (types.Row, bool, error) {
	row, ok, err := p.input.Next()
	if err != nil || !ok {
		return nil, false, err
	}
	for i, e := range p.exprs {
		v, err := e.Eval(row)
		if err != nil {
			return nil, false, err
		}
		p.out[i] = v
	}
	return p.out, true, nil
}

// Close closes the input.
func (p *Project) Close() error { return p.input.Close() }

// Schema returns the projected columns.
func (p *Project) Schema() *types.Schema { return p.schema }

// Children returns the input.
func (p *Project) Children() []Operator { return []Operator{p.input} }

// ChildLabels returns nothing.
func (p *Project) ChildLabels() []string { return nil }

// Label lists the projected expressions, naming only the ones whose output
// name is not already implied by the expression, so a plain column list
// prints as Project(c.name, o.amount) rather than repeating itself.
func (p *Project) Label() string {
	parts := make([]string, len(p.exprs))
	for i, e := range p.exprs {
		parts[i] = e.String()
		if name := p.schema.Columns[i].Name; name != ast.DisplayName(e.Source()) {
			parts[i] += " AS " + name
		}
	}
	return "Project(" + strings.Join(parts, ", ") + ")"
}

// SortKey is one compiled ORDER BY key.
type SortKey struct {
	Expr *eval.Expr
	Desc bool
}

// Sort is a blocking operator: it reads its whole input in Open, sorts it,
// then streams the result. There is no external merge sort, so the input has
// to fit in memory.
type Sort struct {
	stats
	input Operator
	keys  []SortKey
	rows  []sortRow
	pos   int
}

// sortRow pairs a buffered row with its precomputed key values, so comparing
// two rows never re-evaluates an expression and an evaluation error surfaces
// once, during Open, rather than from inside the comparison function.
type sortRow struct {
	row  types.Row
	keys types.Row
}

// NewSort builds a sort over input.
func NewSort(input Operator, keys []SortKey) *Sort {
	return &Sort{input: input, keys: keys}
}

// Open buffers and sorts the input.
func (s *Sort) Open() error {
	return s.measureOpen(func() error {
		if err := s.input.Open(); err != nil {
			return err
		}
		for {
			row, ok, err := s.input.Next()
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			keys := make(types.Row, len(s.keys))
			for i, k := range s.keys {
				v, err := k.Expr.Eval(row)
				if err != nil {
					return err
				}
				keys[i] = v
			}
			s.rows = append(s.rows, sortRow{row: row.Clone(), keys: keys})
		}
		// Stable, so rows that compare equal keep their input order and the
		// golden files are reproducible.
		sort.SliceStable(s.rows, func(i, j int) bool {
			for k := range s.keys {
				c := types.CompareOrdering(s.rows[i].keys[k], s.rows[j].keys[k])
				if c == 0 {
					continue
				}
				if s.keys[k].Desc {
					return c > 0
				}
				return c < 0
			}
			return false
		})
		s.pos = 0
		return nil
	})
}

// Next returns the next row in sorted order.
func (s *Sort) Next() (types.Row, bool, error) { return s.measure(s.next) }

func (s *Sort) next() (types.Row, bool, error) {
	if s.pos >= len(s.rows) {
		return nil, false, nil
	}
	row := s.rows[s.pos].row
	s.pos++
	return row, true, nil
}

// Close releases the buffer and closes the input.
func (s *Sort) Close() error {
	s.rows = nil
	return s.input.Close()
}

// Schema passes the input's schema through.
func (s *Sort) Schema() *types.Schema { return s.input.Schema() }

// Children returns the input.
func (s *Sort) Children() []Operator { return []Operator{s.input} }

// ChildLabels returns nothing.
func (s *Sort) ChildLabels() []string { return nil }

// Label lists the keys and their directions. NULL ordering is spelled out
// because engines disagree about it.
func (s *Sort) Label() string {
	parts := make([]string, len(s.keys))
	for i, k := range s.keys {
		parts[i] = k.Expr.String()
		if k.Desc {
			parts[i] += " DESC"
		}
	}
	return "Sort(" + strings.Join(parts, ", ") + ")"
}

// Limit stops after n rows. It does not close its input early: the operator
// above it decides when the query is done, and Close tears the tree down.
type Limit struct {
	stats
	input Operator
	n     int64
	seen  int64
}

// NewLimit builds a limit over input.
func NewLimit(input Operator, n int64) *Limit {
	return &Limit{input: input, n: n}
}

// Open opens the input.
func (l *Limit) Open() error {
	l.seen = 0
	return l.measureOpen(l.input.Open)
}

// Next returns the next row until the cap is reached.
func (l *Limit) Next() (types.Row, bool, error) { return l.measure(l.next) }

func (l *Limit) next() (types.Row, bool, error) {
	if l.seen >= l.n {
		return nil, false, nil
	}
	row, ok, err := l.input.Next()
	if err != nil || !ok {
		return nil, false, err
	}
	l.seen++
	return row, true, nil
}

// Close closes the input.
func (l *Limit) Close() error { return l.input.Close() }

// Schema passes the input's schema through.
func (l *Limit) Schema() *types.Schema { return l.input.Schema() }

// Children returns the input.
func (l *Limit) Children() []Operator { return []Operator{l.input} }

// ChildLabels returns nothing.
func (l *Limit) ChildLabels() []string { return nil }

// Label shows the cap.
func (l *Limit) Label() string { return "Limit(" + strconv.FormatInt(l.n, 10) + ")" }

// errNotOpen is returned when Next is called before Open, which can only
// happen through a bug in the engine rather than through a bad query.
var errNotOpen = errors.New("Next called before Open")
