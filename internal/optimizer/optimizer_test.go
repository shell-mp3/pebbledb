package optimizer

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/catalog"
	"github.com/shell-mp3/pebbledb/internal/logical"
	"github.com/shell-mp3/pebbledb/internal/parser"
)

// build parses a query and builds a fresh logical plan. A fresh one each
// time matters: the rules rewrite in place, so a shared plan would carry one
// test's rewrites into the next.
func build(t *testing.T, cat *catalog.Catalog, query string) logical.Node {
	t.Helper()
	stmt, err := parser.Parse(query)
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	sel, ok := stmt.(*ast.Select)
	if !ok {
		sel = stmt.(*ast.Explain).Stmt
	}
	plan, err := logical.Build(sel, cat)
	if err != nil {
		t.Fatalf("Build(%q): %v", query, err)
	}
	return plan
}

func loadData(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Load([]string{filepath.Join("..", "..", "data")})
	if err != nil {
		t.Fatalf("loading data/: %v", err)
	}
	return cat
}

// tree renders a plan with its leading and trailing blank lines removed, so
// test expectations can be written as readable raw strings.
func tree(n logical.Node) string { return strings.TrimSpace(logical.Format(n)) }

func TestPredicatePushdown(t *testing.T) {
	cat := loadData(t)
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			name:  "a predicate on one table moves below the join",
			query: "SELECT c.name FROM customers c JOIN orders o ON c.id = o.customer_id WHERE c.country = 'CA'",
			want: `
Project(c.name)
└─ Join(inner, c.id = o.customer_id)
   ├─ Filter(c.country = 'CA')
   │  └─ Scan(customers AS c) [5/5 cols: id, name, country, plan, signup_date]
   └─ Scan(orders AS o) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
		{
			name:  "each conjunct goes to the side that can evaluate it",
			query: "SELECT c.name FROM customers c JOIN orders o ON c.id = o.customer_id WHERE c.country = 'CA' AND o.qty > 1",
			want: `
Project(c.name)
└─ Join(inner, c.id = o.customer_id)
   ├─ Filter(c.country = 'CA')
   │  └─ Scan(customers AS c) [5/5 cols: id, name, country, plan, signup_date]
   └─ Filter(o.qty > 1)
      └─ Scan(orders AS o) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
		{
			name:  "two conjuncts for the same table merge into one filter",
			query: "SELECT c.name FROM customers c JOIN orders o ON c.id = o.customer_id WHERE c.country = 'CA' AND c.plan = 'pro'",
			want: `
Project(c.name)
└─ Join(inner, c.id = o.customer_id)
   ├─ Filter(c.country = 'CA' AND c.plan = 'pro')
   │  └─ Scan(customers AS c) [5/5 cols: id, name, country, plan, signup_date]
   └─ Scan(orders AS o) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
		{
			name:  "a predicate spanning both tables stays above the join",
			query: "SELECT c.id FROM customers c JOIN orders o ON c.id = o.customer_id WHERE c.id < o.qty",
			want: `
Project(c.id)
└─ Filter(c.id < o.qty)
   └─ Join(inner, c.id = o.customer_id)
      ├─ Scan(customers AS c) [5/5 cols: id, name, country, plan, signup_date]
      └─ Scan(orders AS o) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
		{
			name:  "a mixed predicate is split, pushing only the parts it can",
			query: "SELECT c.id FROM customers c JOIN orders o ON c.id = o.customer_id WHERE c.id < o.qty AND c.country = 'CA'",
			want: `
Project(c.id)
└─ Filter(c.id < o.qty)
   └─ Join(inner, c.id = o.customer_id)
      ├─ Filter(c.country = 'CA')
      │  └─ Scan(customers AS c) [5/5 cols: id, name, country, plan, signup_date]
      └─ Scan(orders AS o) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
		{
			name:  "an OR over both tables is not split",
			query: "SELECT c.id FROM customers c JOIN orders o ON c.id = o.customer_id WHERE c.country = 'CA' OR o.qty > 1",
			want: `
Project(c.id)
└─ Filter(c.country = 'CA' OR o.qty > 1)
   └─ Join(inner, c.id = o.customer_id)
      ├─ Scan(customers AS c) [5/5 cols: id, name, country, plan, signup_date]
      └─ Scan(orders AS o) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
		{
			name:  "the preserved side of a left join accepts a pushed predicate",
			query: "SELECT c.name FROM customers c LEFT JOIN orders o ON c.id = o.customer_id WHERE c.country = 'CA'",
			want: `
Project(c.name)
└─ Join(left, c.id = o.customer_id)
   ├─ Filter(c.country = 'CA')
   │  └─ Scan(customers AS c) [5/5 cols: id, name, country, plan, signup_date]
   └─ Scan(orders AS o) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
		{
			name:  "the inner side of a left join does not",
			query: "SELECT c.name FROM customers c LEFT JOIN orders o ON c.id = o.customer_id WHERE o.qty > 1",
			want: `
Project(c.name)
└─ Filter(o.qty > 1)
   └─ Join(left, c.id = o.customer_id)
      ├─ Scan(customers AS c) [5/5 cols: id, name, country, plan, signup_date]
      └─ Scan(orders AS o) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
		{
			name:  "a predicate reaches the far side of a three-way join",
			query: "SELECT o.id FROM orders o JOIN customers c ON o.customer_id = c.id JOIN products p ON o.sku = p.sku WHERE p.category = 'bath'",
			want: `
Project(o.id)
└─ Join(inner, o.sku = p.sku)
   ├─ Join(inner, o.customer_id = c.id)
   │  ├─ Scan(orders AS o) [6/6 cols: id, customer_id, sku, qty, amount, order_date]
   │  └─ Scan(customers AS c) [5/5 cols: id, name, country, plan, signup_date]
   └─ Filter(p.category = 'bath')
      └─ Scan(products AS p) [4/4 cols: sku, title, category, unit_price]`,
		},
		{
			name:  "a HAVING filter is not pushed through the aggregate",
			query: "SELECT sku, COUNT(*) AS n FROM orders GROUP BY sku HAVING COUNT(*) > 2",
			want: `
Project(orders.sku, "COUNT(*)" AS n)
└─ Filter("COUNT(*)" > 2)
   └─ Aggregate(group=sku aggs=COUNT(*))
      └─ Scan(orders) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
		{
			name:  "a WHERE filter stops above the scan it belongs to",
			query: "SELECT sku FROM orders WHERE qty > 1",
			want: `
Project(sku)
└─ Filter(qty > 1)
   └─ Scan(orders) [6/6 cols: id, customer_id, sku, qty, amount, order_date]`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tree(PushDownPredicates(build(t, cat, tc.query)))
			if want := strings.TrimSpace(tc.want); got != want {
				t.Errorf("plan\n got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestProjectionPruning(t *testing.T) {
	cat := loadData(t)
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			name:  "only the projected columns are read",
			query: "SELECT sku, qty FROM orders",
			want: `
Project(sku, qty)
└─ Scan(orders) [2/6 cols: sku, qty]`,
		},
		{
			name:  "a filter's columns are kept even when not projected",
			query: "SELECT sku FROM orders WHERE amount > 10",
			want: `
Project(sku)
└─ Filter(amount > 10)
   └─ Scan(orders) [2/6 cols: sku, amount]`,
		},
		{
			name:  "a sort key's columns are kept even when not projected",
			query: "SELECT sku FROM orders ORDER BY amount",
			want: `
Project(sku)
└─ Sort(amount)
   └─ Scan(orders) [2/6 cols: sku, amount]`,
		},
		{
			name:  "join keys are kept on both sides",
			query: "SELECT c.name FROM customers c JOIN orders o ON c.id = o.customer_id",
			want: `
Project(c.name)
└─ Join(inner, c.id = o.customer_id)
   ├─ Scan(customers AS c) [2/5 cols: id, name]
   └─ Scan(orders AS o) [1/6 cols: customer_id]`,
		},
		{
			name:  "an aggregate only needs its group keys and arguments",
			query: "SELECT sku, SUM(amount) FROM orders GROUP BY sku",
			want: `
Project(orders.sku, "SUM(amount)")
└─ Aggregate(group=sku aggs=SUM(amount))
   └─ Scan(orders) [2/6 cols: sku, amount]`,
		},
		{
			name:  "COUNT(*) needs no columns at all",
			query: "SELECT COUNT(*) FROM orders",
			want: `
Project("COUNT(*)")
└─ Aggregate(aggs=COUNT(*))
   └─ Scan(orders) [0/6 cols: none]`,
		},
		{
			name:  "a star projection keeps everything",
			query: "SELECT * FROM products",
			want: `
Project(products.sku, products.title, products.category, products.unit_price)
└─ Scan(products) [4/4 cols: sku, title, category, unit_price]`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tree(PruneProjections(build(t, cat, tc.query)))
			if want := strings.TrimSpace(tc.want); got != want {
				t.Errorf("plan\n got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// TestRuleOrderMatters is the reason Rules() lists pushdown first. Pruning a
// plan whose predicates are still sitting above the join keeps exactly the
// same columns, but running pruning first and pushdown second would leave the
// pushed-down filter reading a column the scan no longer produces.
func TestRuleOrderMatters(t *testing.T) {
	cat := loadData(t)
	query := "SELECT c.name FROM customers c JOIN orders o ON c.id = o.customer_id WHERE o.amount > 10"

	correct := tree(PruneProjections(PushDownPredicates(build(t, cat, query))))
	if !strings.Contains(correct, "Filter(o.amount > 10)") {
		t.Fatalf("pushdown did not fire:\n%s", correct)
	}
	if !strings.Contains(correct, "Scan(orders AS o) [2/6 cols: customer_id, amount]") {
		t.Errorf("the pushed-down filter's column was pruned away:\n%s", correct)
	}

	// Both orders must still produce a plan whose scans provide every column
	// the filters above them read; the check below is what would catch a
	// regression if Rules() were reordered.
	reversed := tree(PushDownPredicates(PruneProjections(build(t, cat, query))))
	for _, plan := range []string{correct, reversed} {
		if strings.Contains(plan, "Filter(o.amount > 10)") && !strings.Contains(plan, "amount]") {
			t.Errorf("plan filters on a column its scan does not read:\n%s", plan)
		}
	}
}

func TestOptimizeReportsRules(t *testing.T) {
	cat := loadData(t)
	tests := []struct {
		query string
		want  []string
	}{
		{
			query: "SELECT c.name FROM customers c JOIN orders o ON c.id = o.customer_id WHERE c.country = 'CA'",
			want:  []string{"predicate pushdown", "projection pruning"},
		},
		{
			query: "SELECT sku FROM orders",
			want:  []string{"projection pruning"},
		},
		{
			query: "SELECT * FROM products",
			want:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			_, rules := Optimize(build(t, cat, tc.query))
			if strings.Join(rules, ",") != strings.Join(tc.want, ",") {
				t.Errorf("rules = %v, want %v", rules, tc.want)
			}
		})
	}
}

// TestOptimizeIsIdempotent checks that a second pass finds nothing left to
// do, which is what makes the applied-rules report trustworthy.
func TestOptimizeIsIdempotent(t *testing.T) {
	cat := loadData(t)
	for _, query := range []string{
		"SELECT c.name FROM customers c JOIN orders o ON c.id = o.customer_id WHERE c.country = 'CA' AND o.qty > 1",
		"SELECT sku, COUNT(*) FROM orders GROUP BY sku HAVING COUNT(*) > 1 ORDER BY sku",
		"SELECT * FROM products WHERE unit_price > 1",
	} {
		t.Run(query, func(t *testing.T) {
			plan, _ := Optimize(build(t, cat, query))
			once := tree(plan)
			plan, rules := Optimize(plan)
			if twice := tree(plan); twice != once {
				t.Errorf("a second pass changed the plan (rules: %v)\n got:\n%s\nwant:\n%s", rules, twice, once)
			}
			if len(rules) != 0 {
				t.Errorf("a second pass reported rules %v, want none", rules)
			}
		})
	}
}
