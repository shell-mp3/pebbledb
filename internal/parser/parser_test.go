package parser

import (
	"fmt"
	"strings"
	"testing"

	"github.com/shell-mp3/pebbledb/internal/ast"
)

// stmtString renders a parsed statement back to canonical SQL. Comparing
// canonical text is a stronger check than it looks: parenthesisation is
// generated from the tree shape, so an operator-precedence bug changes the
// output.
func stmtString(s ast.Statement) string {
	switch n := s.(type) {
	case *ast.Select:
		return n.String()
	case *ast.Explain:
		return n.String()
	}
	return fmt.Sprintf("%T", s)
}

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "star",
			src:  "select * from t",
			want: "SELECT * FROM t",
		},
		{
			name: "qualified star",
			src:  "SELECT c.* FROM customers c",
			want: "SELECT c.* FROM customers AS c",
		},
		{
			name: "aliases with and without AS",
			src:  "select a as x, b y from t as u",
			want: "SELECT a AS x, b AS y FROM t AS u",
		},
		{
			name: "trailing semicolon is optional",
			src:  "select a from t;",
			want: "SELECT a FROM t",
		},
		{
			name: "case insensitive keywords",
			src:  "SeLeCt a FrOm t WhErE a > 1",
			want: "SELECT a FROM t WHERE a > 1",
		},
		{
			name: "AND binds tighter than OR",
			src:  "select * from t where a or b and c",
			want: "SELECT * FROM t WHERE a OR (b AND c)",
		},
		{
			name: "NOT binds looser than comparison",
			src:  "select * from t where not a = 1",
			want: "SELECT * FROM t WHERE NOT (a = 1)",
		},
		{
			name: "arithmetic precedence",
			src:  "select 1 + 2 * 3 from t",
			want: "SELECT 1 + (2 * 3) FROM t",
		},
		{
			name: "parentheses override precedence",
			src:  "select (1 + 2) * 3 from t",
			want: "SELECT (1 + 2) * 3 FROM t",
		},
		{
			name: "comparison is left associative",
			src:  "select * from t where a - b - c > 0",
			want: "SELECT * FROM t WHERE ((a - b) - c) > 0",
		},
		{
			name: "unary minus folds into a literal",
			src:  "select -1 from t",
			want: "SELECT -1 FROM t",
		},
		{
			name: "unary minus on a column stays an operator",
			src:  "select -a from t",
			want: "SELECT -a FROM t",
		},
		{
			name: "bang equals normalises to <>",
			src:  "select * from t where a != 1",
			want: "SELECT * FROM t WHERE a <> 1",
		},
		{
			name: "is null",
			src:  "select * from t where a is null and b is not null",
			want: "SELECT * FROM t WHERE (a IS NULL) AND (b IS NOT NULL)",
		},
		{
			name: "like and not like",
			src:  "select * from t where a like 'x%' or b not like '_y'",
			want: "SELECT * FROM t WHERE (a LIKE 'x%') OR (NOT (b LIKE '_y'))",
		},
		{
			name: "in list",
			src:  "select * from t where a in (1, 2, 3)",
			want: "SELECT * FROM t WHERE a IN (1, 2, 3)",
		},
		{
			name: "not in list",
			src:  "select * from t where a not in ('x')",
			want: "SELECT * FROM t WHERE a NOT IN ('x')",
		},
		{
			name: "aggregates",
			src:  "select count(*), count(a), sum(b), avg(b), min(b), max(b) from t",
			want: "SELECT COUNT(*), COUNT(a), SUM(b), AVG(b), MIN(b), MAX(b) FROM t",
		},
		{
			name: "aggregate over an expression",
			src:  "select sum(qty * price) as revenue from t",
			want: "SELECT SUM(qty * price) AS revenue FROM t",
		},
		{
			name: "group by with having",
			src:  "select a, count(*) from t group by a having count(*) > 1",
			want: "SELECT a, COUNT(*) FROM t GROUP BY a HAVING COUNT(*) > 1",
		},
		{
			name: "group by several keys",
			src:  "select a, b from t group by a, b",
			want: "SELECT a, b FROM t GROUP BY a, b",
		},
		{
			name: "order by with directions",
			src:  "select a from t order by a desc, b asc, c",
			want: "SELECT a FROM t ORDER BY a DESC, b, c",
		},
		{
			name: "limit",
			src:  "select a from t limit 10",
			want: "SELECT a FROM t LIMIT 10",
		},
		{
			name: "bare join defaults to inner",
			src:  "select * from a join b on a.id = b.id",
			want: "SELECT * FROM a INNER JOIN b ON a.id = b.id",
		},
		{
			name: "inner join",
			src:  "select * from a inner join b on a.id = b.id",
			want: "SELECT * FROM a INNER JOIN b ON a.id = b.id",
		},
		{
			name: "left outer join",
			src:  "select * from a left outer join b on a.id = b.id",
			want: "SELECT * FROM a LEFT JOIN b ON a.id = b.id",
		},
		{
			name: "two joins",
			src:  "select * from a join b on a.id = b.a_id left join c on c.id = b.c_id",
			want: "SELECT * FROM a INNER JOIN b ON a.id = b.a_id LEFT JOIN c ON c.id = b.c_id",
		},
		{
			name: "explain",
			src:  "explain select a from t",
			want: "EXPLAIN SELECT a FROM t",
		},
		{
			name: "explain analyze",
			src:  "explain analyze select a from t",
			want: "EXPLAIN ANALYZE SELECT a FROM t",
		},
		{
			name: "quoted identifier with a space",
			src:  `select "order date" from t`,
			want: `SELECT "order date" FROM t`,
		},
		{
			name: "comments are ignored",
			src:  "select a -- the column\nfrom t /* the table */",
			want: "SELECT a FROM t",
		},
		{
			name: "booleans and null literals",
			src:  "select true, false, null from t",
			want: "SELECT true, false, NULL FROM t",
		},
		{
			name: "a column may be named like an aggregate",
			src:  "select count from t",
			want: "SELECT count FROM t",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmt, err := Parse(tc.src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.src, err)
			}
			if got := stmtString(stmt); got != tc.want {
				t.Errorf("Parse(%q)\n got: %s\nwant: %s", tc.src, got, tc.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantMsg string
	}{
		{"empty", "", "empty statement"},
		{"not a statement", "insert into t values (1)", "only SELECT and EXPLAIN are supported"},
		{"missing from", "select a", "expected FROM"},
		{"missing table", "select a from", "expected a table name"},
		{"trailing garbage", "select a from t extra junk", "unexpected"},
		{"two statements", "select a from t; select b from t", "after end of statement"},
		{"unclosed paren", "select (1 from t", `expected ")"`},
		{"empty select list", "select from t", "unexpected keyword FROM"},
		{"join without on", "select * from a join b", "expected ON after JOIN"},
		{"right join", "select * from a right join b on a.id = b.id", "RIGHT JOIN is not supported"},
		{"full join", "select * from a full join b on a.id = b.id", "FULL JOIN is not supported"},
		{"cross join", "select * from a cross join b", "CROSS JOIN is not supported"},
		{"distinct", "select distinct a from t", "SELECT DISTINCT is not supported"},
		{"offset", "select a from t limit 1 offset 2", "OFFSET is not supported"},
		{"union", "select a from t union select a from u", "UNION is not supported"},
		{"subquery", "select a from t where a in (select b from u)", "subqueries are not supported"},
		{"unknown function", "select upper(a) from t", "unknown function upper"},
		{"nested aggregate", "select sum(count(a)) from t", "cannot be nested"},
		{"count distinct", "select count(distinct a) from t", "COUNT(DISTINCT ...) is not supported"},
		{"sum star", "select sum(*) from t", "only COUNT(*) is"},
		{"two aggregate args", "select sum(a, b) from t", "exactly one argument"},
		{"aggregate in where", "select a from t where count(*) > 1", "not allowed in WHERE"},
		{"aggregate in group by", "select a from t group by count(*)", "not allowed in GROUP BY"},
		{"aggregate in join condition", "select * from a join b on count(*) = 1", "not allowed in a join condition"},
		{"group without by", "select a from t group a", "expected BY"},
		{"order without by", "select a from t order a", "expected BY"},
		{"non integer limit", "select a from t limit 'x'", "expected an integer after LIMIT"},
		{"is not a value", "select a from t where a is 1", "only IS NULL and IS NOT NULL"},
		{"star in an expression", "select 1 + * from t", "* is only allowed"},
		{"reserved word in an expression", "select from from t", "unexpected keyword FROM"},
		{"reserved word as a table name", "select a from order", "reserved word ORDER"},
		{"reserved word as an alias", "select a as group from t", "reserved word GROUP"},
		{"dangling dot", "select t. from t", "expected a column name after t."},
		{"unterminated string", "select 'a from t", "unterminated string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.src)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want an error", tc.src)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("Parse(%q) error = %q, want it to mention %q", tc.src, err, tc.wantMsg)
			}
			if !strings.HasPrefix(err.Error(), "line ") {
				t.Errorf("error %q does not start with a position", err)
			}
		})
	}
}

// TestRewritePreservesOriginal guards the assumption the optimizer depends on:
// rewriting an expression must build a new tree rather than mutate the one the
// unoptimized plan still points at.
func TestRewritePreservesOriginal(t *testing.T) {
	stmt, err := Parse("select * from t where a = 1 and b = 2")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sel := stmt.(*ast.Select)
	before := sel.Where.String()

	rewritten := ast.Rewrite(sel.Where, func(e ast.Expr) ast.Expr {
		if c, ok := e.(*ast.ColumnRef); ok && c.Name == "a" {
			return &ast.ColumnRef{Name: "z"}
		}
		return nil
	})
	if got := sel.Where.String(); got != before {
		t.Errorf("Rewrite mutated the original: %s", got)
	}
	if want := "((z = 1) AND (b = 2))"; rewritten.String() != want {
		t.Errorf("rewritten = %s, want %s", rewritten.String(), want)
	}
}

func TestSplitAndJoinConjuncts(t *testing.T) {
	stmt, err := Parse("select * from t where a = 1 and b = 2 and (c = 3 or d = 4)")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	parts := ast.SplitConjuncts(stmt.(*ast.Select).Where)
	if len(parts) != 3 {
		t.Fatalf("SplitConjuncts returned %d parts, want 3: %v", len(parts), parts)
	}
	if got := ast.SplitConjuncts(nil); got != nil {
		t.Errorf("SplitConjuncts(nil) = %v, want nil", got)
	}
	if got := ast.JoinConjuncts(nil); got != nil {
		t.Errorf("JoinConjuncts(nil) = %v, want nil so the Filter node can be dropped", got)
	}
	if got := ast.Unparenthesize(ast.JoinConjuncts(parts).String()); got != "((a = 1) AND (b = 2)) AND ((c = 3) OR (d = 4))" {
		t.Errorf("round trip = %s", got)
	}
}

func TestKeyIsCaseInsensitive(t *testing.T) {
	a, err := Parse("select A.B from t")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := Parse("select a.b from t")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ka := ast.Key(a.(*ast.Select).Items[0].Expr)
	kb := ast.Key(b.(*ast.Select).Items[0].Expr)
	if ka != kb {
		t.Errorf("Key is case sensitive: %q vs %q", ka, kb)
	}
}
