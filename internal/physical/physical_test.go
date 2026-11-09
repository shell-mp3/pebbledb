package physical

import (
	"errors"
	"strings"
	"testing"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/eval"
	"github.com/shell-mp3/pebbledb/internal/parser"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// source is an in-memory Operator, so operator behaviour can be tested
// without a CSV file. It deliberately reuses one output row, the way Scan
// does, which is how it catches an operator that buffers without cloning.
type source struct {
	stats
	schema  *types.Schema
	rows    []types.Row
	pos     int
	opens   int
	closes  int
	out     types.Row
	failAt  int
	failErr error
}

func newSource(schema *types.Schema, rows ...types.Row) *source {
	return &source{schema: schema, rows: rows, out: make(types.Row, schema.Len()), failAt: -1}
}

func (s *source) Open() error {
	s.opens++
	s.pos = 0
	return nil
}

func (s *source) Next() (types.Row, bool, error) {
	return s.measure(func() (types.Row, bool, error) {
		if s.failAt >= 0 && s.pos == s.failAt {
			return nil, false, s.failErr
		}
		if s.pos >= len(s.rows) {
			return nil, false, nil
		}
		copy(s.out, s.rows[s.pos])
		s.pos++
		return s.out, true, nil
	})
}

func (s *source) Close() error {
	s.closes++
	return nil
}

func (s *source) Schema() *types.Schema { return s.schema }
func (s *source) Children() []Operator  { return nil }
func (s *source) ChildLabels() []string { return nil }
func (s *source) Label() string         { return "Source" }

// drain opens an operator, collects every row and closes it.
func drain(t *testing.T, op Operator) []types.Row {
	t.Helper()
	if err := op.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}
	var out []types.Row
	for {
		row, ok, err := op.Next()
		if err != nil {
			op.Close()
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		out = append(out, row.Clone())
	}
	if err := op.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out
}

// render turns rows into one comparable string.
func render(rows []types.Row) string {
	parts := make([]string, len(rows))
	for i, row := range rows {
		cells := make([]string, len(row))
		for j, v := range row {
			cells[j] = v.String()
		}
		parts[i] = strings.Join(cells, ",")
	}
	return strings.Join(parts, " | ")
}

// compile parses and compiles a bare expression against a schema.
func compile(t *testing.T, src string, s *types.Schema) *eval.Expr {
	t.Helper()
	stmt, err := parser.Parse("SELECT " + src + " FROM t")
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	e, err := eval.Compile(stmt.(*ast.Select).Items[0].Expr, s)
	if err != nil {
		t.Fatalf("Compile(%q): %v", src, err)
	}
	return e
}

func intSchema(table string, names ...string) *types.Schema {
	cols := make([]types.Column, len(names))
	for i, n := range names {
		cols[i] = types.Column{Table: table, Name: n, Kind: types.KindInt}
	}
	return types.NewSchema(cols...)
}

func row(vals ...int64) types.Row {
	out := make(types.Row, len(vals))
	for i, v := range vals {
		out[i] = types.NewInt(v)
	}
	return out
}

func TestFilterDropsUnknown(t *testing.T) {
	s := intSchema("t", "a")
	src := newSource(s, types.Row{types.NewInt(1)}, types.Row{types.Null}, types.Row{types.NewInt(3)})
	f := NewFilter(src, compile(t, "a > 0", s))
	if got, want := render(drain(t, f)), "1 | 3"; got != want {
		t.Errorf("Filter kept %q, want %q (a NULL comparison is UNKNOWN, not TRUE)", got, want)
	}
	if src.closes != 1 {
		t.Errorf("input closed %d times, want 1", src.closes)
	}
}

func TestFilterPropagatesEvalErrors(t *testing.T) {
	s := types.NewSchema(types.Column{Table: "t", Name: "a", Kind: types.KindString})
	src := newSource(s, types.Row{types.NewString("x")})
	f := NewFilter(src, compile(t, "a > 1", s))
	if err := f.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	if _, _, err := f.Next(); err == nil {
		t.Error("comparing a string to an int should fail at runtime")
	}
}

func TestProject(t *testing.T) {
	s := intSchema("t", "a", "b")
	src := newSource(s, row(1, 2), row(3, 4))
	p := NewProject(src,
		[]*eval.Expr{compile(t, "b", s), compile(t, "a + b", s)},
		types.NewSchema(
			types.Column{Name: "b", Kind: types.KindInt},
			types.Column{Name: "sum", Kind: types.KindInt},
		))
	if got, want := render(drain(t, p)), "2,3 | 4,7"; got != want {
		t.Errorf("Project produced %q, want %q", got, want)
	}
}

func TestLimitIsLazy(t *testing.T) {
	s := intSchema("t", "a")
	src := newSource(s, row(1), row(2), row(3), row(4), row(5))
	l := NewLimit(src, 2)
	if got, want := render(drain(t, l)), "1 | 2"; got != want {
		t.Errorf("Limit produced %q, want %q", got, want)
	}
	// The whole point of the pull model: the input was asked for two rows,
	// not five.
	if got := src.Stats().Rows; got != 2 {
		t.Errorf("input produced %d rows, want 2; Limit is not pulling lazily", got)
	}
}

func TestLimitZero(t *testing.T) {
	s := intSchema("t", "a")
	src := newSource(s, row(1), row(2))
	if got := drain(t, NewLimit(src, 0)); len(got) != 0 {
		t.Errorf("LIMIT 0 returned %d rows", len(got))
	}
	if got := src.Stats().Rows; got != 0 {
		t.Errorf("LIMIT 0 still read %d rows from the input", got)
	}
}

func TestSortIsStableAndPutsNullsFirst(t *testing.T) {
	s := types.NewSchema(
		types.Column{Table: "t", Name: "k", Kind: types.KindInt},
		types.Column{Table: "t", Name: "tag", Kind: types.KindInt},
	)
	src := newSource(s,
		types.Row{types.NewInt(2), types.NewInt(1)},
		types.Row{types.Null, types.NewInt(2)},
		types.Row{types.NewInt(1), types.NewInt(3)},
		types.Row{types.NewInt(2), types.NewInt(4)},
	)
	asc := NewSort(src, []SortKey{{Expr: compile(t, "k", s)}})
	if got, want := render(drain(t, asc)), "NULL,2 | 1,3 | 2,1 | 2,4"; got != want {
		t.Errorf("ascending sort = %q, want %q", got, want)
	}

	src.pos = 0
	desc := NewSort(src, []SortKey{{Expr: compile(t, "k", s), Desc: true}})
	if got, want := render(drain(t, desc)), "2,1 | 2,4 | 1,3 | NULL,2"; got != want {
		t.Errorf("descending sort = %q, want %q (ties must keep input order)", got, want)
	}
}

func TestSortTwoKeys(t *testing.T) {
	s := intSchema("t", "a", "b")
	src := newSource(s, row(1, 2), row(2, 1), row(1, 1), row(2, 2))
	keys := []SortKey{
		{Expr: compile(t, "a", s)},
		{Expr: compile(t, "b", s), Desc: true},
	}
	if got, want := render(drain(t, NewSort(src, keys))), "1,2 | 1,1 | 2,2 | 2,1"; got != want {
		t.Errorf("sort = %q, want %q", got, want)
	}
}

// joinFixture builds a hash join over two in-memory inputs.
func joinFixture(t *testing.T, jt ast.JoinType, left, right *source, cond string) *HashJoin {
	t.Helper()
	joined := left.Schema().Concat(right.Schema())
	stmt, err := parser.Parse("SELECT 1 FROM t WHERE " + cond)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	leftKeys, rightKeys, residual, err := splitJoinCondition(stmt.(*ast.Select).Where, left.Schema(), right.Schema())
	if err != nil {
		t.Fatalf("splitJoinCondition(%q): %v", cond, err)
	}
	var lc, rc []*eval.Expr
	for i := range leftKeys {
		l, err := eval.Compile(leftKeys[i], left.Schema())
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		r, err := eval.Compile(rightKeys[i], right.Schema())
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		lc, rc = append(lc, l), append(rc, r)
	}
	var res *eval.Expr
	if residual != nil {
		if res, err = eval.Compile(residual, joined); err != nil {
			t.Fatalf("Compile residual: %v", err)
		}
	}
	j, err := NewHashJoin(left, right, jt, lc, rc, res)
	if err != nil {
		t.Fatalf("NewHashJoin: %v", err)
	}
	return j
}

func TestInnerJoin(t *testing.T) {
	left := newSource(intSchema("l", "id"), row(1), row(2), row(3))
	right := newSource(intSchema("r", "id", "v"), row(2, 20), row(3, 30), row(3, 31), row(4, 40))
	j := joinFixture(t, ast.InnerJoin, left, right, "l.id = r.id")
	// Probe order follows the left input, and rows within a key keep build
	// order, so the output is deterministic.
	if got, want := render(drain(t, j)), "2,2,20 | 3,3,30 | 3,3,31"; got != want {
		t.Errorf("inner join = %q, want %q", got, want)
	}
}

func TestLeftJoinNullExtendsUnmatched(t *testing.T) {
	left := newSource(intSchema("l", "id"), row(1), row(2))
	right := newSource(intSchema("r", "id", "v"), row(2, 20))
	j := joinFixture(t, ast.LeftJoin, left, right, "l.id = r.id")
	if got, want := render(drain(t, j)), "1,NULL,NULL | 2,2,20"; got != want {
		t.Errorf("left join = %q, want %q", got, want)
	}
}

// TestJoinNullKeysNeverMatch is the SQL rule that trips people up: NULL is
// not equal to NULL, so a NULL key matches nothing, not even another NULL.
func TestJoinNullKeysNeverMatch(t *testing.T) {
	left := newSource(intSchema("l", "id"), types.Row{types.Null}, row(1))
	right := newSource(intSchema("r", "id", "v"), types.Row{types.Null, types.NewInt(9)}, row(1, 10))

	inner := joinFixture(t, ast.InnerJoin, left, right, "l.id = r.id")
	if got, want := render(drain(t, inner)), "1,1,10"; got != want {
		t.Errorf("inner join with NULL keys = %q, want %q", got, want)
	}

	left.pos, right.pos = 0, 0
	outer := joinFixture(t, ast.LeftJoin, left, right, "l.id = r.id")
	if got, want := render(drain(t, outer)), "NULL,NULL,NULL | 1,1,10"; got != want {
		t.Errorf("left join with NULL keys = %q, want %q", got, want)
	}
}

func TestJoinResidualCondition(t *testing.T) {
	left := newSource(intSchema("l", "id"), row(1), row(2))
	right := newSource(intSchema("r", "id", "v"), row(1, 5), row(1, 50), row(2, 5))
	j := joinFixture(t, ast.InnerJoin, left, right, "l.id = r.id AND r.v > 10")
	if got, want := render(drain(t, j)), "1,1,50"; got != want {
		t.Errorf("join with residual = %q, want %q", got, want)
	}
	if !strings.Contains(j.Label(), "residual r.v > 10") {
		t.Errorf("Label does not mention the residual: %s", j.Label())
	}
}

func TestJoinOnTwoColumns(t *testing.T) {
	left := newSource(intSchema("l", "a", "b"), row(1, 1), row(1, 2))
	right := newSource(intSchema("r", "a", "b", "v"), row(1, 2, 99), row(1, 3, 98))
	j := joinFixture(t, ast.InnerJoin, left, right, "l.a = r.a AND l.b = r.b")
	if got, want := render(drain(t, j)), "1,2,1,2,99"; got != want {
		t.Errorf("two-column join = %q, want %q", got, want)
	}
}

func TestJoinWithoutEqualityIsRejected(t *testing.T) {
	left := intSchema("l", "id")
	right := intSchema("r", "id")
	stmt, err := parser.Parse("SELECT 1 FROM t WHERE l.id < r.id")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, _, _, err = splitJoinCondition(stmt.(*ast.Select).Where, left, right)
	if err == nil {
		t.Fatal("a join with no equality was accepted")
	}
	if !strings.Contains(err.Error(), "no equality between the two tables") {
		t.Errorf("error = %q, want it to explain that only a hash join exists", err)
	}
}

func TestJoinConditionOrientationIsNormalised(t *testing.T) {
	left := intSchema("l", "id")
	right := intSchema("r", "id")
	stmt, err := parser.Parse("SELECT 1 FROM t WHERE r.id = l.id")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	lk, rk, residual, err := splitJoinCondition(stmt.(*ast.Select).Where, left, right)
	if err != nil {
		t.Fatalf("splitJoinCondition: %v", err)
	}
	if residual != nil {
		t.Errorf("unexpected residual %s", residual)
	}
	if lk[0].String() != "l.id" || rk[0].String() != "r.id" {
		t.Errorf("keys not normalised to left/right order: %s = %s", lk[0], rk[0])
	}
}

// aggFixture builds a HashAggregate over an in-memory input.
func aggFixture(t *testing.T, src *source, groupBy []string, specs []string) *HashAggregate {
	t.Helper()
	var groups []*eval.Expr
	var cols []types.Column
	for _, g := range groupBy {
		e := compile(t, g, src.Schema())
		groups = append(groups, e)
		cols = append(cols, types.Column{Name: g, Kind: e.Kind()})
	}
	var aggs []AggSpec
	for _, spec := range specs {
		stmt, err := parser.Parse("SELECT " + spec + " FROM t")
		if err != nil {
			t.Fatalf("Parse(%q): %v", spec, err)
		}
		call := stmt.(*ast.Select).Items[0].Expr.(*ast.Aggregate)
		s := AggSpec{Func: call.Func}
		if !call.Star {
			s.Arg = compile(t, call.Arg.String(), src.Schema())
		}
		aggs = append(aggs, s)
		kind, err := eval.AggKind(call, src.Schema())
		if err != nil {
			t.Fatalf("AggKind(%q): %v", spec, err)
		}
		cols = append(cols, types.Column{Name: spec, Kind: kind})
	}
	return NewHashAggregate(src, groups, aggs, types.NewSchema(cols...))
}

func TestAggregateFunctions(t *testing.T) {
	s := types.NewSchema(types.Column{Table: "t", Name: "v", Kind: types.KindInt})
	rows := []types.Row{
		{types.NewInt(1)}, {types.Null}, {types.NewInt(5)}, {types.NewInt(3)},
	}
	src := newSource(s, rows...)
	a := aggFixture(t, src, nil, []string{"COUNT(*)", "COUNT(v)", "SUM(v)", "AVG(v)", "MIN(v)", "MAX(v)"})
	// COUNT(*) counts rows, COUNT(v) skips the NULL, and the rest ignore it.
	if got, want := render(drain(t, a)), "4,3,9,3,1,5"; got != want {
		t.Errorf("aggregates = %q, want %q", got, want)
	}
}

func TestAggregateOverEmptyInput(t *testing.T) {
	s := types.NewSchema(types.Column{Table: "t", Name: "v", Kind: types.KindInt})

	// Without GROUP BY there is always exactly one output row.
	noGroup := aggFixture(t, newSource(s), nil, []string{"COUNT(*)", "SUM(v)", "AVG(v)", "MIN(v)"})
	if got, want := render(drain(t, noGroup)), "0,NULL,NULL,NULL"; got != want {
		t.Errorf("empty aggregate = %q, want %q", got, want)
	}

	// With GROUP BY there are no groups, so no rows.
	grouped := aggFixture(t, newSource(s), []string{"v"}, []string{"COUNT(*)"})
	if got := drain(t, grouped); len(got) != 0 {
		t.Errorf("grouped aggregate over an empty input returned %d rows, want 0", len(got))
	}
}

func TestAggregateSumKeepsIntegersIntegral(t *testing.T) {
	ints := types.NewSchema(types.Column{Table: "t", Name: "v", Kind: types.KindInt})
	a := aggFixture(t, newSource(ints, types.Row{types.NewInt(1)}, types.Row{types.NewInt(2)}), nil, []string{"SUM(v)", "AVG(v)"})
	if got, want := render(drain(t, a)), "3,1.5"; got != want {
		t.Errorf("= %q, want %q (SUM of ints is an int, AVG is always a float)", got, want)
	}

	floats := types.NewSchema(types.Column{Table: "t", Name: "v", Kind: types.KindFloat})
	b := aggFixture(t, newSource(floats, types.Row{types.NewFloat(1.5)}, types.Row{types.NewFloat(2)}), nil, []string{"SUM(v)"})
	if got, want := render(drain(t, b)), "3.5"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
}

func TestAggregateGroupsInFirstSeenOrder(t *testing.T) {
	s := types.NewSchema(types.Column{Table: "t", Name: "k", Kind: types.KindString})
	src := newSource(s,
		types.Row{types.NewString("b")},
		types.Row{types.NewString("a")},
		types.Row{types.NewString("b")},
		types.Row{types.Null},
		types.Row{types.NewString("a")},
		types.Row{types.Null},
	)
	a := aggFixture(t, src, []string{"k"}, []string{"COUNT(*)"})
	// NULL is a group of its own, and groups come out in the order they were
	// first seen, which is what keeps the golden files reproducible.
	if got, want := render(drain(t, a)), "b,2 | a,2 | NULL,2"; got != want {
		t.Errorf("groups = %q, want %q", got, want)
	}
}

func TestAggregateRejectsNonNumericSum(t *testing.T) {
	s := types.NewSchema(types.Column{Table: "t", Name: "v", Kind: types.KindString})
	src := newSource(s, types.Row{types.NewString("x")})
	a := NewHashAggregate(src,
		nil,
		[]AggSpec{{Func: ast.AggSum, Arg: compile(t, "v", s)}},
		types.NewSchema(types.Column{Name: "SUM(v)", Kind: types.KindString}))
	if err := a.Open(); err == nil {
		a.Close()
		t.Error("SUM over strings succeeded")
	}
}

func TestNextBeforeOpenIsAnError(t *testing.T) {
	s := intSchema("t", "a")
	agg := aggFixture(t, newSource(s), nil, []string{"COUNT(*)"})
	if _, _, err := agg.Next(); !errors.Is(err, errNotOpen) {
		t.Errorf("HashAggregate.Next before Open gave %v, want errNotOpen", err)
	}
	left, right := newSource(intSchema("l", "id")), newSource(intSchema("r", "id"))
	j := joinFixture(t, ast.InnerJoin, left, right, "l.id = r.id")
	if _, _, err := j.Next(); !errors.Is(err, errNotOpen) {
		t.Errorf("HashJoin.Next before Open gave %v, want errNotOpen", err)
	}
}

func TestExplainTree(t *testing.T) {
	s := intSchema("t", "a")
	src := newSource(s, row(1), row(2), row(3))
	plan := NewLimit(NewFilter(src, compile(t, "a > 1", s)), 1)
	want := strings.Join([]string{
		"Limit(1)",
		"└─ Filter(a > 1)",
		"   └─ Source",
	}, "\n")
	if got := Explain(plan, false); got != want {
		t.Errorf("Explain\n got:\n%s\nwant:\n%s", got, want)
	}

	drain(t, plan)
	analyzed := Explain(plan, true)
	if !strings.Contains(analyzed, "Limit(1)  (rows=1,") {
		t.Errorf("EXPLAIN ANALYZE did not annotate the root:\n%s", analyzed)
	}
}

func TestExplainLabelsJoinSides(t *testing.T) {
	left := newSource(intSchema("l", "id"), row(1))
	right := newSource(intSchema("r", "id"), row(1))
	j := joinFixture(t, ast.InnerJoin, left, right, "l.id = r.id")
	got := Explain(j, false)
	if !strings.Contains(got, "├─ probe: Source") || !strings.Contains(got, "└─ build: Source") {
		t.Errorf("join children are not labelled:\n%s", got)
	}
}

func TestJoinKeyNormalisesNumbers(t *testing.T) {
	asInt, ok := JoinKey(nil, types.Row{types.NewInt(1)})
	if !ok {
		t.Fatal("an integer key should be usable")
	}
	asFloat, ok := JoinKey(nil, types.Row{types.NewFloat(1)})
	if !ok {
		t.Fatal("an integral float key should be usable")
	}
	if string(asInt) != string(asFloat) {
		t.Errorf("1 and 1.0 hash differently (%q vs %q), so an int column would not join a float one", asInt, asFloat)
	}
	if _, ok := JoinKey(nil, types.Row{types.Null}); ok {
		t.Error("a NULL join key should be reported as unusable")
	}
}

func TestKeysAreUnambiguous(t *testing.T) {
	// Adjacent values must not run together into the same encoding.
	cases := [][]types.Value{
		{types.NewInt(1), types.NewInt(23)},
		{types.NewInt(12), types.NewInt(3)},
		{types.NewString("a"), types.NewString("bc")},
		{types.NewString("ab"), types.NewString("c")},
		{types.NewString("i1"), types.NewString("")},
		{types.Null, types.NewInt(1)},
		{types.NewInt(1), types.Null},
		{types.NewFloat(1.5), types.NewFloat(1.5)},
		{types.True, types.False},
	}
	seen := map[string][]types.Value{}
	for _, vals := range cases {
		key := string(GroupKey(nil, vals))
		if prev, ok := seen[key]; ok {
			t.Errorf("%v and %v encode identically as %q", prev, vals, key)
		}
		seen[key] = vals
	}
}
