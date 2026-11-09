package engine

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/shell-mp3/pebbledb/internal/catalog"
)

// The committed sample data is 30 rows, which is the right size for golden
// files and the wrong size for measuring anything: a whole query finishes
// inside the clock's resolution. These benchmarks generate a larger dataset
// in a temp directory so the numbers in the README are measured rather than
// guessed.
const (
	benchCustomers = 5000
	benchOrders    = 200000
)

var (
	benchOnce sync.Once
	benchCat  *catalog.Catalog
	benchErr  error
	benchDir  string
)

// benchCatalog generates the dataset once per test binary.
func benchCatalog(tb testing.TB) *catalog.Catalog {
	tb.Helper()
	benchOnce.Do(func() {
		benchDir, benchErr = os.MkdirTemp("", "pebbledb-bench-")
		if benchErr != nil {
			return
		}
		if benchErr = writeCustomers(filepath.Join(benchDir, "customers.csv")); benchErr != nil {
			return
		}
		if benchErr = writeOrders(filepath.Join(benchDir, "orders.csv")); benchErr != nil {
			return
		}
		benchCat, benchErr = catalog.Load([]string{benchDir})
	})
	if benchErr != nil {
		tb.Fatalf("generating the benchmark data: %v", benchErr)
	}
	return benchCat
}

// TestMain removes the generated dataset after the benchmarks have run.
func TestMain(m *testing.M) {
	code := m.Run()
	if benchDir != "" {
		os.RemoveAll(benchDir)
	}
	os.Exit(code)
}

var benchCountries = []string{"CA", "US", "MX", "BR", "FI", "EG", "JP", "GH", "CZ", "JO"}

func writeCustomers(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "id,name,country,plan,signup_date")
	for i := 1; i <= benchCustomers; i++ {
		fmt.Fprintf(w, "%d,customer %d,%s,%s,2023-01-01\n",
			i, i, benchCountries[i%len(benchCountries)], []string{"free", "pro", "team"}[i%3])
	}
	return w.Flush()
}

func writeOrders(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "id,customer_id,sku,qty,amount,order_date")
	for i := 1; i <= benchOrders; i++ {
		fmt.Fprintf(w, "%d,%d,SKU-%03d,%d,%d.%02d,2023-06-01\n",
			i, (i%benchCustomers)+1, i%500, (i%7)+1, i%900, i%100)
	}
	return w.Flush()
}

// runBench executes a query repeatedly, reporting rows per second so results
// are comparable across machines.
func runBench(b *testing.B, optimize bool, query string) {
	cat := benchCatalog(b)
	e := &Engine{Catalog: cat, Optimize: optimize, MaxRows: 0}
	// One run outside the timer, both to warm the page cache and to fail
	// loudly on a broken query rather than reporting a suspiciously fast one.
	res, err := e.Query(query)
	if err != nil {
		b.Fatalf("Query: %v", err)
	}
	rows := len(res.Rows)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Query(query); err != nil {
			b.Fatalf("Query: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(rows), "result_rows")
}

// BenchmarkScan measures the floor: reading and typing every field of the
// 200k-row file with nothing else going on.
func BenchmarkScan(b *testing.B) {
	runBench(b, true, "SELECT COUNT(*) FROM orders")
}

// BenchmarkScanAllColumns is the same scan with every field parsed, which is
// what projection pruning is avoiding.
func BenchmarkScanAllColumns(b *testing.B) {
	runBench(b, true, "SELECT MAX(id), MAX(customer_id), MAX(sku), MAX(qty), MAX(amount), MAX(order_date) FROM orders")
}

// BenchmarkFilter measures a selective filter over the whole file.
func BenchmarkFilter(b *testing.B) {
	runBench(b, true, "SELECT COUNT(*) FROM orders WHERE amount > 800")
}

// BenchmarkJoin and its pair are the measurement the README quotes: the same
// join, planned with and without the rewrite rules.
func BenchmarkJoinOptimized(b *testing.B) {
	runBench(b, true, benchJoinQuery)
}

func BenchmarkJoinUnoptimized(b *testing.B) {
	runBench(b, false, benchJoinQuery)
}

const benchJoinQuery = `SELECT c.country, COUNT(*) AS n, SUM(o.amount) AS total
FROM customers c JOIN orders o ON c.id = o.customer_id
WHERE c.country = 'CA' AND o.amount > 800
GROUP BY c.country`

// BenchmarkGroupBy measures hash aggregation over 500 distinct groups.
func BenchmarkGroupBy(b *testing.B) {
	runBench(b, true, "SELECT sku, COUNT(*) AS n, SUM(amount) AS total FROM orders GROUP BY sku")
}

// BenchmarkSortAndLimit checks that a top-N query pays for the sort but not
// for materialising the output.
func BenchmarkSortAndLimit(b *testing.B) {
	runBench(b, true, "SELECT id, amount FROM orders ORDER BY amount DESC LIMIT 10")
}

// TestBenchmarkDataIsQueryable keeps the generator honest: if it ever writes
// a file the catalog cannot type, the benchmarks would silently measure the
// wrong thing.
func TestBenchmarkDataIsQueryable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping dataset generation in short mode")
	}
	cat := benchCatalog(t)
	orders, err := cat.Get("orders")
	if err != nil {
		t.Fatalf("Get(orders): %v", err)
	}
	if orders.Rows != benchOrders {
		t.Errorf("generated %d orders, want %d", orders.Rows, benchOrders)
	}
	if got := orders.Schema.String(); got != "orders.id int, orders.customer_id int, orders.sku string, orders.qty int, orders.amount float, orders.order_date string" {
		t.Errorf("unexpected generated schema: %s", got)
	}
	res, err := New(cat).Query(benchJoinQuery)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("got %d rows, want 1:\n%s", len(res.Rows), FormatResult(res))
	}
}
