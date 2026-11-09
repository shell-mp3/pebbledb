package eval

import (
	"strings"
	"testing"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/parser"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// testSchema is a two-table schema with a deliberate name collision on "id"
// so ambiguity is exercised, plus a nullable column.
func testSchema() *types.Schema {
	return types.NewSchema(
		types.Column{Table: "c", Name: "id", Kind: types.KindInt},
		types.Column{Table: "c", Name: "name", Kind: types.KindString},
		types.Column{Table: "c", Name: "score", Kind: types.KindFloat},
		types.Column{Table: "c", Name: "note", Kind: types.KindString},
		types.Column{Table: "o", Name: "id", Kind: types.KindInt},
	)
}

// testRow matches testSchema: note is NULL.
func testRow() types.Row {
	return types.Row{
		types.NewInt(7),
		types.NewString("Ada"),
		types.NewFloat(2.5),
		types.Null,
		types.NewInt(99),
	}
}

// compileExpr parses a bare expression by wrapping it in a SELECT.
func compileExpr(t *testing.T, src string) *Expr {
	t.Helper()
	stmt, err := parser.Parse("SELECT " + src + " FROM c")
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	e, err := Compile(stmt.(*ast.Select).Items[0].Expr, testSchema())
	if err != nil {
		t.Fatalf("Compile(%q): %v", src, err)
	}
	return e
}

func TestEval(t *testing.T) {
	tests := []struct {
		src  string
		want types.Value
	}{
		// Literals and columns.
		{"1", types.NewInt(1)},
		{"'x'", types.NewString("x")},
		{"null", types.Null},
		{"c.id", types.NewInt(7)},
		{"name", types.NewString("Ada")},
		{"note", types.Null},

		// Arithmetic.
		{"c.id + 1", types.NewInt(8)},
		{"c.id * 2 - 4", types.NewInt(10)},
		{"c.id / 2", types.NewFloat(3.5)},
		{"c.id % 4", types.NewInt(3)},
		{"score * 2", types.NewFloat(5)},
		{"c.id + score", types.NewFloat(9.5)},
		{"-c.id", types.NewInt(-7)},
		{"note + 1", types.Null},

		// Comparison, including the NULL cases.
		{"c.id = 7", types.True},
		{"c.id <> 7", types.False},
		{"c.id < 7", types.False},
		{"c.id <= 7", types.True},
		{"score > 2", types.True},
		{"c.id = score", types.False},
		{"note = 'x'", types.Null},
		{"note <> 'x'", types.Null},

		// Three-valued logic.
		{"c.id = 7 and score = 2.5", types.True},
		{"c.id = 7 and note = 'x'", types.Null},
		{"c.id = 0 and note = 'x'", types.False},
		{"c.id = 7 or note = 'x'", types.True},
		{"not c.id = 7", types.False},
		{"not note = 'x'", types.Null},

		// IS NULL.
		{"note is null", types.True},
		{"note is not null", types.False},
		{"name is null", types.False},

		// IN.
		{"c.id in (6, 7, 8)", types.True},
		{"c.id in (1, 2)", types.False},
		{"c.id not in (1, 2)", types.True},
		{"c.id in (1, null)", types.Null},
		{"c.id in (7, null)", types.True},
		{"note in (1, 2)", types.Null},

		// LIKE.
		{"name like 'Ada'", types.True},
		{"name like 'A%'", types.True},
		{"name like '%a'", types.True},
		{"name like 'A_a'", types.True},
		{"name like 'a%'", types.False},
		{"name not like 'B%'", types.True},
		{"note like '%'", types.Null},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			got, err := compileExpr(t, tc.src).Eval(testRow())
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got != tc.want {
				t.Errorf("%s = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

func TestCompileErrors(t *testing.T) {
	tests := []struct {
		src     string
		wantMsg string
	}{
		{"nope", "no such column: nope"},
		{"c.nope", "no such column: c.nope"},
		{"id", "ambiguous column: id"},
		{"x.id", "no such column: x.id"},
		{"count(*)", "aggregates are only allowed"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			stmt, err := parser.Parse("SELECT " + tc.src + " FROM c")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			_, err = Compile(stmt.(*ast.Select).Items[0].Expr, testSchema())
			if err == nil {
				t.Fatalf("Compile(%q) succeeded, want an error", tc.src)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

func TestEvalErrors(t *testing.T) {
	tests := []struct {
		src     string
		wantMsg string
	}{
		{"name + 1", "cannot apply operator +"},
		{"name > 1", "cannot apply comparison"},
		{"c.id / 0", "division by zero"},
		{"c.id % 0", "division by zero"},
		{"c.id like 'x'", "LIKE requires strings"},
		{"c.id in ('x')", "cannot apply comparison"},
		{"c.id and c.id", "boolean context"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			_, err := compileExpr(t, tc.src).Eval(testRow())
			if err == nil {
				t.Fatalf("Eval(%q) succeeded, want an error", tc.src)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

func TestInferKindMatchesEval(t *testing.T) {
	tests := []struct {
		src  string
		want types.Kind
	}{
		{"1", types.KindInt},
		{"1.5", types.KindFloat},
		{"'x'", types.KindString},
		{"c.id", types.KindInt},
		{"score", types.KindFloat},
		{"c.id + 1", types.KindInt},
		{"c.id + score", types.KindFloat},
		{"c.id / 2", types.KindFloat},
		{"c.id % 2", types.KindInt},
		{"-c.id", types.KindInt},
		{"c.id = 1", types.KindBool},
		{"c.id is null", types.KindBool},
		{"not c.id = 1", types.KindBool},
		{"c.id in (1)", types.KindBool},
		{"name like 'x'", types.KindBool},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			stmt, err := parser.Parse("SELECT " + tc.src + " FROM c")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			e := stmt.(*ast.Select).Items[0].Expr
			inferred, err := InferKind(e, testSchema())
			if err != nil {
				t.Fatalf("InferKind: %v", err)
			}
			if inferred != tc.want {
				t.Errorf("InferKind(%q) = %v, want %v", tc.src, inferred, tc.want)
			}
			// The compiled expression must agree, since the planner labels
			// columns from one and executes the other.
			if compiled := compileExpr(t, tc.src).Kind(); compiled != inferred {
				t.Errorf("Compile(%q).Kind() = %v but InferKind said %v", tc.src, compiled, inferred)
			}
		})
	}
}

func TestAggKind(t *testing.T) {
	tests := []struct {
		src     string
		want    types.Kind
		wantErr bool
	}{
		{"count(*)", types.KindInt, false},
		{"count(name)", types.KindInt, false},
		{"sum(c.id)", types.KindInt, false},
		{"sum(score)", types.KindFloat, false},
		{"avg(c.id)", types.KindFloat, false},
		{"min(name)", types.KindString, false},
		{"max(score)", types.KindFloat, false},
		{"sum(name)", types.KindNull, true},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			stmt, err := parser.Parse("SELECT " + tc.src + " FROM c")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			agg := stmt.(*ast.Select).Items[0].Expr.(*ast.Aggregate)
			got, err := AggKind(agg, testSchema())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("AggKind(%q) succeeded, want an error", tc.src)
				}
				return
			}
			if err != nil {
				t.Fatalf("AggKind(%q): %v", tc.src, err)
			}
			if got != tc.want {
				t.Errorf("AggKind(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

func TestStringRoundTrip(t *testing.T) {
	e := compileExpr(t, "c.id + 1 > 2 and name like 'A%'")
	if want := "c.id + 1 > 2 AND name LIKE 'A%'"; e.String() != want {
		t.Errorf("String() = %q, want %q", e.String(), want)
	}
}

func TestLike(t *testing.T) {
	tests := []struct {
		s, pattern string
		want       bool
	}{
		{"", "", true},
		{"", "%", true},
		{"", "%%%", true},
		{"", "_", false},
		{"a", "", false},
		{"abc", "abc", true},
		{"abc", "ABC", false},
		{"abc", "a%", true},
		{"abc", "%c", true},
		{"abc", "%b%", true},
		{"abc", "a_c", true},
		{"abc", "a_", false},
		{"abc", "_%_", true},
		{"abc", "%", true},
		{"aaa", "%a", true},
		{"aaa", "a%a%a", true},
		{"aaa", "a%a%a%a", false},
		{"banana", "b%n%n%", true},
		{"banana", "%ana%", true},
		{"banana", "%xyz%", false},
		// Pathological backtracking: must finish rather than blow up.
		{strings.Repeat("a", 60) + "b", strings.Repeat("%a", 30) + "c", false},
	}
	for _, tc := range tests {
		if got := Like(tc.s, tc.pattern); got != tc.want {
			t.Errorf("Like(%q, %q) = %v, want %v", tc.s, tc.pattern, got, tc.want)
		}
	}
}
