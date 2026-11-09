package logical

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/catalog"
	"github.com/shell-mp3/pebbledb/internal/parser"
)

func loadData(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Load([]string{filepath.Join("..", "..", "data")})
	if err != nil {
		t.Fatalf("loading data/: %v", err)
	}
	return cat
}

func plan(t *testing.T, cat *catalog.Catalog, query string) (Node, error) {
	t.Helper()
	stmt, err := parser.Parse(query)
	if err != nil {
		return nil, err
	}
	sel, ok := stmt.(*ast.Select)
	if !ok {
		sel = stmt.(*ast.Explain).Stmt
	}
	return Build(sel, cat)
}

func TestBuildPlanShape(t *testing.T) {
	cat := loadData(t)
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			name:  "star expands to explicit columns",
			query: "SELECT * FROM products",
			want: `
Project(products.sku, products.title, products.category, products.unit_price)
└─ Scan(products) [4/4 cols: sku, title, category, unit_price]`,
		},
		{
			name:  "a qualified star expands only that table",
			query: "SELECT c.* FROM customers c JOIN orders o ON c.id = o.customer_id",
			want: `
Project(c.id, c.name, c.country, c.plan, c.signup_date)
└─ Join(inner, c.id = o.customer_id)
   ├─ Scan(customers AS c) [5/5 cols: id, name, country, plan, signup_date]
   └─ Scan(orders AS o) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
		{
			name:  "clauses stack in evaluation order",
			query: "SELECT sku FROM orders WHERE qty > 1 ORDER BY amount DESC LIMIT 2",
			want: `
Limit(2)
└─ Project(sku)
   └─ Sort(amount DESC)
      └─ Filter(qty > 1)
         └─ Scan(orders) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
		{
			name:  "sort sits below project so ORDER BY can name an unselected column",
			query: "SELECT title FROM products ORDER BY unit_price",
			want: `
Project(title)
└─ Sort(unit_price)
   └─ Scan(products) [4/4 cols: sku, title, category, unit_price]`,
		},
		{
			name:  "ORDER BY an output alias sorts by the aliased expression",
			query: "SELECT unit_price * 2 AS pair FROM products ORDER BY pair DESC",
			want: `
Project(unit_price * 2 AS pair)
└─ Sort(unit_price * 2 DESC)
   └─ Scan(products) [4/4 cols: sku, title, category, unit_price]`,
		},
		{
			name:  "aggregates become references to the aggregate node's columns",
			query: "SELECT category, COUNT(*) AS n FROM products GROUP BY category",
			want: `
Project(products.category, "COUNT(*)" AS n)
└─ Aggregate(group=category aggs=COUNT(*))
   └─ Scan(products) [4/4 cols: sku, title, category, unit_price]`,
		},
		{
			name:  "an aggregate with no GROUP BY still gets an aggregate node",
			query: "SELECT COUNT(*) FROM orders",
			want: `
Project("COUNT(*)")
└─ Aggregate(aggs=COUNT(*))
   └─ Scan(orders) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
		{
			name:  "the same aggregate used twice is computed once",
			query: "SELECT sku, COUNT(*) AS n FROM orders GROUP BY sku HAVING COUNT(*) > 1 ORDER BY COUNT(*) DESC",
			want: `
Project(orders.sku, "COUNT(*)" AS n)
└─ Sort("COUNT(*)" DESC)
   └─ Filter("COUNT(*)" > 1)
      └─ Aggregate(group=sku aggs=COUNT(*))
         └─ Scan(orders) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
		{
			name:  "an aggregate over an expression keeps the expression inside",
			query: "SELECT SUM(qty * 2) AS units FROM orders",
			want: `
Project("SUM(qty * 2)" AS units)
└─ Aggregate(aggs=SUM(qty * 2))
   └─ Scan(orders) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
		{
			name:  "joins are left deep in the order written",
			query: "SELECT o.id FROM orders o JOIN customers c ON o.customer_id = c.id JOIN products p ON o.sku = p.sku",
			want: `
Project(o.id)
└─ Join(inner, o.sku = p.sku)
   ├─ Join(inner, o.customer_id = c.id)
   │  ├─ Scan(orders AS o) [6/6 cols: id, customer_id, sku, qty, amount, order_date]
   │  └─ Scan(customers AS c) [5/5 cols: id, name, country, plan, signup_date]
   └─ Scan(products AS p) [4/4 cols: sku, title, category, unit_price]`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, err := plan(t, cat, tc.query)
			if err != nil {
				t.Fatalf("Build(%q): %v", tc.query, err)
			}
			got := strings.TrimSpace(Format(node))
			if want := strings.TrimSpace(tc.want); got != want {
				t.Errorf("plan\n got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestBuildErrors(t *testing.T) {
	cat := loadData(t)
	tests := []struct {
		name    string
		query   string
		wantMsg string
	}{
		{"unknown table", "SELECT * FROM nope", "no such table: nope"},
		{"unknown column", "SELECT nope FROM orders", "no such column: nope"},
		{"unknown column in where", "SELECT id FROM orders WHERE nope = 1", "in WHERE: no such column: nope"},
		{"unknown column in order by", "SELECT id FROM orders ORDER BY nope", "in ORDER BY: no such column: nope"},
		{"unknown column in group by", "SELECT COUNT(*) FROM orders GROUP BY nope", "in GROUP BY: no such column: nope"},
		{"unknown column in join condition", "SELECT 1 FROM orders o JOIN customers c ON o.nope = c.id", "in JOIN ... ON: no such column: o.nope"},
		{"ambiguous column", "SELECT id FROM orders o JOIN customers c ON o.customer_id = c.id", "ambiguous column: id"},
		{"unknown star qualifier", "SELECT x.* FROM orders", `no table named "x"`},
		{"table used twice", "SELECT 1 FROM orders JOIN orders ON orders.id = orders.id", "is used twice"},
		{"ungrouped column", "SELECT sku, COUNT(*) FROM orders GROUP BY qty", "sku in the select list must appear in GROUP BY"},
		{"ungrouped column with no group by", "SELECT sku, COUNT(*) FROM orders", "sku in the select list must appear in GROUP BY"},
		{"ungrouped column in having", "SELECT COUNT(*) FROM orders GROUP BY sku HAVING qty > 1", "qty in HAVING must appear in GROUP BY"},
		{"ungrouped column in order by", "SELECT COUNT(*) FROM orders GROUP BY sku ORDER BY qty", "qty in ORDER BY must appear in GROUP BY"},
		{"having without grouping", "SELECT sku FROM orders HAVING sku = 'x'", "HAVING requires GROUP BY"},
		{"sum of a string", "SELECT SUM(sku) FROM orders", "SUM requires a numeric argument"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := plan(t, cat, tc.query)
			if err == nil {
				t.Fatalf("Build(%q) succeeded, want an error", tc.query)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

// TestGroupByExpressionMatchingIsStructural documents a real limitation: a
// select item is matched to a GROUP BY key by comparing the expressions'
// canonical text, so the same value written two ways is not recognised.
func TestGroupByExpressionMatchingIsStructural(t *testing.T) {
	cat := loadData(t)
	if _, err := plan(t, cat, "SELECT qty * 2, COUNT(*) FROM orders GROUP BY qty * 2"); err != nil {
		t.Errorf("identical expressions should match: %v", err)
	}
	if _, err := plan(t, cat, "SELECT 2 * qty, COUNT(*) FROM orders GROUP BY qty * 2"); err == nil {
		t.Error("2 * qty matched GROUP BY qty * 2; matching is supposed to be structural, so update the README if this now works")
	}
	// Case and whitespace do not matter, because matching is on the
	// canonical lower-cased rendering rather than on the source text.
	if _, err := plan(t, cat, "SELECT QTY  *  2, COUNT(*) FROM orders GROUP BY qty*2"); err != nil {
		t.Errorf("case and spacing should not affect matching: %v", err)
	}
}

func TestScanAllColumnsIsIndependentOfPruning(t *testing.T) {
	cat := loadData(t)
	node, err := plan(t, cat, "SELECT sku FROM orders")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	scan := node.(*Project).Input.(*Scan)
	if got := len(scan.AllColumns()); got != 6 {
		t.Errorf("an unpruned scan reports %d columns, want 6", got)
	}
	scan.Columns = []int{2}
	if got := scan.Schema().Len(); got != 1 {
		t.Errorf("Schema did not follow the pruned column list: %d columns", got)
	}
	if got := scan.Schema().Columns[0].Name; got != "sku" {
		t.Errorf("pruned schema column = %q, want sku", got)
	}
	scan.Columns = []int{}
	if got := scan.Schema().Len(); got != 0 {
		t.Errorf("an empty column list should produce an empty schema, got %d", got)
	}
}
