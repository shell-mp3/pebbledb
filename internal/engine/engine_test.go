package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/catalog"
	"github.com/shell-mp3/pebbledb/internal/logical"
	"github.com/shell-mp3/pebbledb/internal/optimizer"
	"github.com/shell-mp3/pebbledb/internal/parser"
	"github.com/shell-mp3/pebbledb/internal/physical"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// newTestEngine loads data/ with the optimizer on.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	return New(loadSampleData(t))
}

func TestExplainAnalyzeReportsActualRows(t *testing.T) {
	e := newTestEngine(t)
	res, err := e.Query("EXPLAIN ANALYZE SELECT c.name FROM customers c JOIN orders o ON c.id = o.customer_id WHERE o.amount > 100")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !res.IsExplain() {
		t.Fatal("EXPLAIN ANALYZE did not return a plan")
	}
	if res.Rows != nil {
		t.Error("EXPLAIN ANALYZE should not return rows")
	}

	// Each line should carry a row count and a duration.
	for _, line := range strings.Split(res.Plan, "\n") {
		if !regexp.MustCompile(`\(rows=\d+, [0-9.]+(ns|us|ms|s)\)$`).MatchString(line) {
			t.Errorf("line has no analyze annotation: %q", line)
		}
	}
	// Two orders are over 100, so the join and everything above it produced
	// two rows, while the pushed-down filter on orders saw all 30.
	if !strings.Contains(res.Plan, "Project(c.name)  (rows=2,") {
		t.Errorf("expected the projection to report 2 rows:\n%s", res.Plan)
	}
	if !strings.Contains(res.Plan, "Filter(o.amount > 100)  (rows=2,") {
		t.Errorf("expected the pushed-down filter to report 2 surviving rows:\n%s", res.Plan)
	}
}

// TestLimitStopsReadingEarly is the claim the volcano model exists to make:
// a LIMIT does not read the whole file. It is checked through the scan's own
// row counter rather than by timing, so it cannot flake.
func TestLimitStopsReadingEarly(t *testing.T) {
	e := newTestEngine(t)
	res, err := e.Query("EXPLAIN ANALYZE SELECT id FROM orders LIMIT 4")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !strings.Contains(res.Plan, "4 rows") && !strings.Contains(res.Plan, "(rows=4,") {
		t.Fatalf("plan does not report row counts:\n%s", res.Plan)
	}
	for _, line := range strings.Split(res.Plan, "\n") {
		if !strings.Contains(line, "Scan(") {
			continue
		}
		if !strings.Contains(line, "(rows=4,") {
			t.Errorf("Scan read more than the 4 rows the LIMIT needed, so execution is not pull-based:\n%s", line)
		}
	}
}

// TestOptimizationDoesNotChangeResults re-checks the golden suite's central
// invariant over a wider set of queries, including ones whose plans the
// optimizer leaves alone.
func TestOptimizationDoesNotChangeResults(t *testing.T) {
	cat := loadSampleData(t)
	plain := &Engine{Catalog: cat, Optimize: false}
	optimized := &Engine{Catalog: cat, Optimize: true}

	queries := []string{
		"SELECT * FROM customers",
		"SELECT * FROM orders WHERE amount IS NULL",
		"SELECT c.name, o.amount FROM customers c JOIN orders o ON c.id = o.customer_id",
		"SELECT c.name, o.amount FROM customers c LEFT JOIN orders o ON c.id = o.customer_id",
		"SELECT c.country, SUM(o.amount) AS t FROM customers c JOIN orders o ON c.id = o.customer_id GROUP BY c.country ORDER BY t DESC, c.country",
		"SELECT sku, COUNT(*) FROM orders GROUP BY sku ORDER BY sku",
		"SELECT o.id FROM orders o JOIN products p ON o.sku = p.sku WHERE p.category = 'bath' AND o.qty > 1 ORDER BY o.id",
		"SELECT c.name FROM customers c LEFT JOIN orders o ON c.id = o.customer_id WHERE c.country = 'CA' ORDER BY c.name",
		"SELECT COUNT(*) FROM customers c JOIN orders o ON c.id = o.customer_id JOIN products p ON o.sku = p.sku",
		"SELECT id FROM orders ORDER BY amount DESC, id LIMIT 7",
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			a, errA := plain.Query(q)
			b, errB := optimized.Query(q)
			if errA != nil || errB != nil {
				t.Fatalf("errors: without=%v with=%v", errA, errB)
			}
			if got, want := FormatResult(b), FormatResult(a); got != want {
				t.Errorf("optimization changed the result\n--- with ---\n%s\n--- without ---\n%s", got, want)
			}
		})
	}
}

// TestPruningKeepsFilterColumns is the ordering dependency between the two
// rules: pushdown runs first, so pruning sees the predicates on the scans and
// keeps the columns they read. Running them the other way round would prune
// away a column a filter still needs.
func TestPruningKeepsFilterColumns(t *testing.T) {
	stmt, err := parser.Parse("SELECT c.name FROM customers c JOIN orders o ON c.id = o.customer_id WHERE o.amount > 10")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := logical.Build(stmt.(*ast.Select), loadSampleData(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	plan, rules := optimizer.Optimize(plan)
	if len(rules) != 2 {
		t.Fatalf("expected both rules to fire, got %v", rules)
	}
	op, err := physical.Build(plan)
	if err != nil {
		t.Fatalf("physical.Build: %v", err)
	}
	defer op.Close()

	got := physical.Explain(op, false)
	// orders must still deliver amount (for the filter) and customer_id (for
	// the join), but not sku, qty, id or order_date.
	if !strings.Contains(got, "Scan(orders AS o) reads 2 of 6 columns [customer_id, amount]") {
		t.Errorf("orders scan did not keep exactly the columns still in use:\n%s", got)
	}
}

func TestQueryErrors(t *testing.T) {
	e := newTestEngine(t)
	tests := []struct {
		query   string
		wantMsg string
	}{
		{"", "empty statement"},
		{"DELETE FROM orders", "only SELECT and EXPLAIN are supported"},
		{"SELECT * FROM orders WHERE", "unexpected"},
		{"SELECT * FROM nothing", "no such table"},
		{"SELECT amount FROM orders WHERE sku > 1", "cannot apply comparison"},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			_, err := e.Query(tc.query)
			if err == nil {
				t.Fatalf("Query(%q) succeeded, want an error", tc.query)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

func TestMaxRows(t *testing.T) {
	e := newTestEngine(t)
	e.MaxRows = 5
	if _, err := e.Query("SELECT * FROM orders"); err == nil {
		t.Error("expected the row cap to stop a 30-row query")
	}
	if _, err := e.Query("SELECT * FROM orders LIMIT 5"); err != nil {
		t.Errorf("a query at the cap should still run: %v", err)
	}
}

func TestFormatResultAlignment(t *testing.T) {
	e := newTestEngine(t)
	res, err := e.Query("SELECT sku, unit_price FROM products ORDER BY unit_price LIMIT 2")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	want := strings.Join([]string{
		"sku   | unit_price",
		"------+-----------",
		"B-415 |       4.25",
		"C-500 |          7",
		"(2 rows)",
	}, "\n")
	if got := FormatResult(res); got != want {
		t.Errorf("FormatResult\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatResultEmpty(t *testing.T) {
	e := newTestEngine(t)
	res, err := e.Query("SELECT sku FROM products WHERE sku = 'nope'")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := FormatResult(res); !strings.HasSuffix(got, "(0 rows)") {
		t.Errorf("empty result should still print a header and a count, got:\n%s", got)
	}
}

func TestSchemas(t *testing.T) {
	e := newTestEngine(t)
	got := e.Schemas()
	for _, want := range []string{"customers (12 rows", "orders (30 rows", "products (8 rows", "unit_price       float"} {
		if !strings.Contains(got, want) {
			t.Errorf(".schema output is missing %q:\n%s", want, got)
		}
	}
}

// TestScanReopenIsRepeatable checks that running the same query twice gives
// the same answer, which only holds because Scan re-reads the file rather
// than caching it.
func TestScanReopenIsRepeatable(t *testing.T) {
	e := newTestEngine(t)
	var first string
	for i := 0; i < 3; i++ {
		res, err := e.Query("SELECT COUNT(*), SUM(amount) FROM orders")
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		got := FormatResult(res)
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Errorf("run %d differs:\n%s\nvs\n%s", i+1, got, first)
		}
	}
}

// TestMissingFileIsReportedAtOpen covers the case where a file disappears
// between cataloguing and execution, which is the one way a planned query
// can still fail on a file error.
func TestMissingFileIsReportedAtOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.csv")
	if err := os.WriteFile(path, []byte("a\n1\n2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Load([]string{path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cat).Query("SELECT a FROM t"); err == nil {
		t.Error("querying a deleted file succeeded")
	}
}

func TestKindsAreReported(t *testing.T) {
	e := newTestEngine(t)
	res, err := e.Query("SELECT id, amount, sku FROM orders LIMIT 1")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	want := []types.Kind{types.KindInt, types.KindFloat, types.KindString}
	if len(res.Kinds) != len(want) {
		t.Fatalf("got %d kinds, want %d", len(res.Kinds), len(want))
	}
	for i := range want {
		if res.Kinds[i] != want[i] {
			t.Errorf("column %d kind = %v, want %v", i, res.Kinds[i], want[i])
		}
	}
}
