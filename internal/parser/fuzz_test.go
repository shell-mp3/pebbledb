package parser

import (
	"strings"
	"testing"

	"github.com/shell-mp3/pebbledb/internal/ast"
)

// seeds give the fuzzer a starting corpus that already reaches every grammar
// production, plus the malformed shapes that broke the parser while it was
// being written: a bare dot, an unbalanced paren, deeply nested parens.
var seeds = []string{
	"",
	" ",
	";",
	"select",
	"select *",
	"select * from t",
	"SELECT a, b AS c FROM t AS u WHERE a > 1 AND b IS NOT NULL ORDER BY a DESC LIMIT 5",
	"select c.name, sum(o.amount) as total from customers c join orders o on c.id = o.customer_id group by c.name having sum(o.amount) > 100 order by total desc limit 3",
	"select * from a left join b on a.id = b.id",
	"explain analyze select count(*) from t",
	"select a from t where a in (1, 2, 3) or a not like '%x_'",
	"select -1 + 2 * (3 - 4) / 5 % 6 from t",
	"select \"quoted col\" from t -- comment\n",
	"select /* block */ 1 from t",
	"select a from t where a = 'it''s'",
	".",
	"(",
	"select ((((((((((1)))))))))) from t",
	"select 1e",
	"select 'unterminated",
	"select a from t group by",
	"select a from t where a is",
	"select count(count(a)) from t",
	"\x00",
	"select 99999999999999999999 from t",
}

// FuzzParse asserts the parser's only contract against hostile input: it
// returns either a statement or an error, and never panics. The parser is the
// one part of the engine that sees arbitrary user text, so a panic here is a
// crash in the REPL.
func FuzzParse(f *testing.F) {
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		stmt, err := Parse(src)
		if err != nil {
			if stmt != nil {
				t.Fatalf("Parse(%q) returned both a statement and an error %v", src, err)
			}
			if !strings.HasPrefix(err.Error(), "line ") {
				t.Fatalf("Parse(%q) error %q does not carry a position", src, err)
			}
			return
		}
		if stmt == nil {
			t.Fatalf("Parse(%q) returned no statement and no error", src)
		}

		// Anything that parsed must also render, and the rendered form must
		// parse back to the same text. A re-parse failure means String is
		// producing SQL the parser cannot read, which would silently corrupt
		// EXPLAIN output.
		printed := stmtString(stmt)
		reparsed, err := Parse(printed)
		if err != nil {
			t.Fatalf("Parse(%q) printed as %q, which does not re-parse: %v", src, printed, err)
		}
		if again := stmtString(reparsed); again != printed {
			t.Fatalf("printing is not stable for %q:\nfirst:  %s\nsecond: %s", src, printed, again)
		}

		// Walking and rewriting must terminate and leave the tree intact.
		sel, ok := stmt.(*ast.Select)
		if !ok {
			sel = stmt.(*ast.Explain).Stmt
		}
		for _, item := range sel.Items {
			if item.Expr == nil {
				continue
			}
			ast.Walk(item.Expr, func(ast.Expr) bool { return true })
			ast.Rewrite(item.Expr, func(ast.Expr) ast.Expr { return nil })
		}
	})
}
