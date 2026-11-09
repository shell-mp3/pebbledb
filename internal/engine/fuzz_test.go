package engine

import (
	"strings"
	"testing"
)

// planSeeds reach every plan shape the engine can build, over the real sample
// tables, so the fuzzer starts from queries that get past the parser and
// exercise the planner rather than bouncing off a syntax error.
var planSeeds = []string{
	"SELECT * FROM orders",
	"SELECT o.sku, o.amount FROM orders o WHERE o.amount > 10 ORDER BY o.amount DESC LIMIT 2",
	"SELECT c.name, o.amount FROM customers c JOIN orders o ON c.id = o.customer_id",
	"SELECT c.name, o.amount FROM customers c LEFT JOIN orders o ON c.id = o.customer_id WHERE c.country = 'CA'",
	"SELECT sku, COUNT(*), SUM(qty), AVG(amount), MIN(amount), MAX(amount) FROM orders GROUP BY sku HAVING COUNT(*) > 1 ORDER BY sku",
	"SELECT amount > 50 AS big, COUNT(*) FROM orders GROUP BY amount > 50",
	"EXPLAIN SELECT COUNT(*) FROM orders",
	"EXPLAIN ANALYZE SELECT COUNT(*) FROM orders",
	"SELECT c.name, p.title FROM orders o JOIN customers c ON o.customer_id = c.id JOIN products p ON o.sku = p.sku",
	"SELECT qty / 0 FROM orders",
	"SELECT sku FROM orders WHERE sku LIKE '%-1_0' AND qty IN (1, 2, 3) AND amount IS NOT NULL",
	"SELECT id FROM customers c JOIN orders o ON c.id = o.customer_id",
	"SELECT nope FROM orders",
	"SELECT COUNT(*) FROM orders GROUP BY nope",
	"SELECT sku, COUNT(*) FROM orders",
	"SELECT 1 FROM orders o JOIN customers c ON o.customer_id < c.id",
	"SELECT * FROM orders LIMIT 0",
}

// FuzzQuery drives the whole pipeline, from text to rows, over the real
// sample data. The parser has its own fuzz target; this one exists for
// everything after it, where a mis-resolved column index or a plan the
// optimizer rewrote into an inconsistent shape would panic rather than
// return an error.
//
// It also re-checks the optimizer's central invariant on whatever the fuzzer
// invents: a query must give the same answer with the rules on and off.
func FuzzQuery(f *testing.F) {
	for _, s := range planSeeds {
		f.Add(s)
	}
	cat, err := loadCatalogForFuzz()
	if err != nil {
		f.Fatalf("loading data/: %v", err)
	}
	plain := &Engine{Catalog: cat, Optimize: false, MaxRows: 5000}
	optimized := &Engine{Catalog: cat, Optimize: true, MaxRows: 5000}

	f.Fuzz(func(t *testing.T, query string) {
		resPlain, errPlain := plain.Query(query)
		resOpt, errOpt := optimized.Query(query)

		switch {
		case errPlain == nil && errOpt == nil:
			// EXPLAIN is the one case where the two engines are meant to
			// disagree: showing that difference is what it is for.
			if resOpt.IsExplain() {
				return
			}
			if a, b := FormatResult(resPlain), FormatResult(resOpt); a != b {
				t.Fatalf("optimization changed the result of %q\n--- without ---\n%s\n--- with ---\n%s", query, a, b)
			}
		case errPlain != nil && errOpt != nil:
			// Both failed. The messages may differ in principle, but any
			// difference is worth knowing about, since it means the rules
			// changed which error the user sees.
			if errPlain.Error() != errOpt.Error() {
				t.Fatalf("optimization changed the error for %q\n without: %v\n with:    %v", query, errPlain, errOpt)
			}
		case errPlain != nil:
			t.Fatalf("%q failed only without the optimizer: %v", query, errPlain)
		default:
			t.Fatalf("%q failed only with the optimizer: %v", query, errOpt)
		}

		// An error must be a message, not a crash dressed up as one.
		if errOpt != nil && strings.TrimSpace(errOpt.Error()) == "" {
			t.Fatalf("%q failed with an empty error message", query)
		}
	})
}
