package physical

import (
	"strings"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/eval"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// AggSpec is one compiled aggregate function. Arg is nil for COUNT(*), which
// counts rows rather than values.
type AggSpec struct {
	Func ast.AggFunc
	Arg  *eval.Expr
}

// String renders the call for EXPLAIN.
func (a AggSpec) String() string {
	if a.Arg == nil {
		return a.Func.String() + "(*)"
	}
	return a.Func.String() + "(" + a.Arg.String() + ")"
}

// HashAggregate groups rows and computes aggregates. It is blocking: Open
// consumes the whole input.
//
// Groups are emitted in first-seen order. SQL does not promise any order for
// GROUP BY, but picking a deterministic one makes the golden files stable and
// makes the REPL predictable; a query that cares should say ORDER BY.
type HashAggregate struct {
	stats
	input   Operator
	groupBy []*eval.Expr
	aggs    []AggSpec
	schema  *types.Schema

	groups []*aggGroup
	index  map[string]int
	pos    int
	out    types.Row
}

// aggGroup holds one group's key values and its accumulators.
type aggGroup struct {
	key  types.Row
	accs []accumulator
}

// NewHashAggregate builds an aggregation whose output is the group keys
// followed by the aggregate results, matching schema.
func NewHashAggregate(input Operator, groupBy []*eval.Expr, aggs []AggSpec, schema *types.Schema) *HashAggregate {
	return &HashAggregate{
		input:   input,
		groupBy: groupBy,
		aggs:    aggs,
		schema:  schema,
		out:     make(types.Row, schema.Len()),
	}
}

// Open consumes the input and builds the groups.
func (a *HashAggregate) Open() error {
	return a.measureOpen(func() error {
		if err := a.input.Open(); err != nil {
			return err
		}
		a.groups, a.index, a.pos = nil, map[string]int{}, 0
		keyVals := make(types.Row, len(a.groupBy))
		var buf []byte
		rows := 0
		for {
			row, ok, err := a.input.Next()
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			rows++
			for i, g := range a.groupBy {
				v, err := g.Eval(row)
				if err != nil {
					return err
				}
				keyVals[i] = v
			}
			buf = GroupKey(buf, keyVals)
			idx, ok := a.index[string(buf)]
			if !ok {
				idx = len(a.groups)
				a.index[string(buf)] = idx
				a.groups = append(a.groups, &aggGroup{key: keyVals.Clone(), accs: a.newAccumulators()})
			}
			for i, spec := range a.aggs {
				if spec.Arg == nil {
					// COUNT(*) counts the row, so there is no value to read.
					if err := a.groups[idx].accs[i].add(types.Null); err != nil {
						return err
					}
					continue
				}
				v, err := spec.Arg.Eval(row)
				if err != nil {
					return err
				}
				if err := a.groups[idx].accs[i].add(v); err != nil {
					return err
				}
			}
		}
		// An aggregate with no GROUP BY always produces exactly one row, even
		// over an empty input: COUNT(*) is 0 and SUM is NULL. With a GROUP BY
		// there are no groups, so no rows.
		if rows == 0 && len(a.groupBy) == 0 {
			a.groups = append(a.groups, &aggGroup{key: types.Row{}, accs: a.newAccumulators()})
		}
		return nil
	})
}

func (a *HashAggregate) newAccumulators() []accumulator {
	out := make([]accumulator, len(a.aggs))
	for i, spec := range a.aggs {
		out[i] = newAccumulator(spec.Func, spec.Arg != nil)
	}
	return out
}

// Next returns the next group.
func (a *HashAggregate) Next() (types.Row, bool, error) { return a.measure(a.next) }

func (a *HashAggregate) next() (types.Row, bool, error) {
	if a.index == nil {
		return nil, false, errNotOpen
	}
	if a.pos >= len(a.groups) {
		return nil, false, nil
	}
	g := a.groups[a.pos]
	a.pos++
	n := copy(a.out, g.key)
	for i, acc := range g.accs {
		a.out[n+i] = acc.result()
	}
	return a.out, true, nil
}

// Close releases the groups and closes the input.
func (a *HashAggregate) Close() error {
	a.groups, a.index = nil, nil
	return a.input.Close()
}

// Schema returns the group key columns followed by the aggregate columns.
func (a *HashAggregate) Schema() *types.Schema { return a.schema }

// Children returns the input.
func (a *HashAggregate) Children() []Operator { return []Operator{a.input} }

// ChildLabels returns nothing.
func (a *HashAggregate) ChildLabels() []string { return nil }

// Label lists the group keys and the aggregates.
func (a *HashAggregate) Label() string {
	var parts []string
	if len(a.groupBy) > 0 {
		keys := make([]string, len(a.groupBy))
		for i, g := range a.groupBy {
			keys[i] = g.String()
		}
		parts = append(parts, "group by "+strings.Join(keys, ", "))
	} else {
		parts = append(parts, "no group by")
	}
	aggs := make([]string, len(a.aggs))
	for i, spec := range a.aggs {
		aggs[i] = spec.String()
	}
	if len(aggs) > 0 {
		parts = append(parts, strings.Join(aggs, ", "))
	}
	return "HashAggregate(" + strings.Join(parts, ", ") + ")"
}

// accumulator folds values for one aggregate within one group.
type accumulator interface {
	add(v types.Value) error
	result() types.Value
}

// newAccumulator picks an accumulator. hasArg distinguishes COUNT(expr),
// which skips NULLs, from COUNT(*), which counts every row.
func newAccumulator(fn ast.AggFunc, hasArg bool) accumulator {
	switch fn {
	case ast.AggCount:
		if hasArg {
			return &countNonNullAcc{}
		}
		return &countAcc{}
	case ast.AggSum:
		return &sumAcc{}
	case ast.AggAvg:
		return &avgAcc{}
	case ast.AggMin:
		return &extremeAcc{wantMax: false}
	}
	return &extremeAcc{wantMax: true}
}

// countAcc is COUNT(*): it counts rows, so the value handed to it is ignored.
type countAcc struct{ n int64 }

func (c *countAcc) add(types.Value) error {
	c.n++
	return nil
}

func (c *countAcc) result() types.Value { return types.NewInt(c.n) }

// countNonNullAcc is COUNT(expr): NULLs are not counted.
type countNonNullAcc struct{ n int64 }

func (c *countNonNullAcc) add(v types.Value) error {
	if !v.IsNull() {
		c.n++
	}
	return nil
}

func (c *countNonNullAcc) result() types.Value { return types.NewInt(c.n) }

// sumAcc adds numbers, ignoring NULLs. The result stays an integer while
// every value seen was an integer, and SUM over no values at all is NULL
// rather than 0, which is what SQL says.
type sumAcc struct {
	isum  int64
	fsum  float64
	float bool
	any   bool
}

func (s *sumAcc) add(v types.Value) error {
	if v.IsNull() {
		return nil
	}
	if !v.Kind.IsNumeric() {
		return &types.TypeError{Op: "SUM", Left: v.Kind, Right: types.KindNull}
	}
	if v.Kind == types.KindFloat && !s.float {
		s.float = true
		s.fsum += float64(s.isum)
	}
	if s.float {
		s.fsum += v.Float()
	} else {
		s.isum += v.Int
	}
	s.any = true
	return nil
}

func (s *sumAcc) result() types.Value {
	switch {
	case !s.any:
		return types.Null
	case s.float:
		return types.NewFloat(s.fsum)
	}
	return types.NewInt(s.isum)
}

// avgAcc is always a float, and NULL over no values.
type avgAcc struct {
	sum sumAcc
	n   int64
}

func (a *avgAcc) add(v types.Value) error {
	if v.IsNull() {
		return nil
	}
	if err := a.sum.add(v); err != nil {
		return &types.TypeError{Op: "AVG", Left: v.Kind, Right: types.KindNull}
	}
	a.n++
	return nil
}

func (a *avgAcc) result() types.Value {
	if a.n == 0 {
		return types.Null
	}
	total := a.sum.result()
	return types.NewFloat(total.Float() / float64(a.n))
}

// extremeAcc is MIN or MAX. It ignores NULLs and uses the same total order as
// Sort, so a mixed-type column still has a defined answer.
type extremeAcc struct {
	wantMax bool
	best    types.Value
	any     bool
}

func (e *extremeAcc) add(v types.Value) error {
	if v.IsNull() {
		return nil
	}
	if !e.any {
		e.best, e.any = v, true
		return nil
	}
	c := types.CompareOrdering(v, e.best)
	if (e.wantMax && c > 0) || (!e.wantMax && c < 0) {
		e.best = v
	}
	return nil
}

func (e *extremeAcc) result() types.Value {
	if !e.any {
		return types.Null
	}
	return e.best
}
