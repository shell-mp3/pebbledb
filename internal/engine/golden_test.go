package engine

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shell-mp3/pebbledb/internal/catalog"
)

// update rewrites the golden files instead of comparing against them:
//
//	go test ./internal/engine -update
//
// Every regenerated file should then be read in the diff, because a golden
// suite that is updated without being read asserts nothing.
var update = flag.Bool("update", false, "rewrite the golden files from the engine's current output")

// Golden file format. Each file holds one query and everything the engine
// produces for it: the plan before optimization, the plan after, which rules
// fired, and the rows. Asserting on the plan and the rows together is the
// point, because it is what catches an optimization that changes an answer.
const (
	secQuery   = "-- query"
	secPlan    = "-- plan (unoptimized)"
	secPlanOpt = "-- plan (optimized)"
	secResult  = "-- result"
	secError   = "-- error"
)

var sections = []string{secQuery, secPlan, secPlanOpt, secResult, secError}

func goldenDir() string { return filepath.Join("testdata", "golden") }

func TestGolden(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(goldenDir(), "*.golden"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no golden files in %s", goldenDir())
	}
	cat := loadSampleData(t)

	for _, path := range files {
		name := strings.TrimSuffix(filepath.Base(path), ".golden")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			query := section(string(raw), secQuery)
			if query == "" {
				t.Fatalf("%s has no %q section", path, secQuery)
			}
			got := generate(t, cat, query)

			if *update {
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return
			}
			if want := normalize(string(raw)); got != want {
				t.Errorf("%s is out of date; rerun with -update and read the diff\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
			}
		})
	}
}

// loadSampleData loads data/ once per run.
func loadSampleData(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Load([]string{filepath.Join("..", "..", "data")})
	if err != nil {
		t.Fatalf("loading data/: %v", err)
	}
	return cat
}

// generate runs a query both ways and renders the golden content.
//
// It also enforces the invariant the whole optimizer rests on: optimizing a
// query must never change its answer. Both plans are executed and their
// rendered results compared, so a rewrite that drops or reorders rows fails
// the suite even if the plan it produced looks plausible.
func generate(t *testing.T, cat *catalog.Catalog, query string) string {
	t.Helper()
	plain := &Engine{Catalog: cat, Optimize: false}
	optimized := &Engine{Catalog: cat, Optimize: true}

	resPlain, errPlain := plain.Query(query)
	resOpt, errOpt := optimized.Query(query)

	switch {
	case errPlain != nil && errOpt == nil:
		t.Fatalf("query failed only without the optimizer: %v", errPlain)
	case errPlain == nil && errOpt != nil:
		t.Fatalf("query failed only with the optimizer: %v", errOpt)
	case errPlain != nil:
		if errPlain.Error() != errOpt.Error() {
			t.Fatalf("different errors with and without the optimizer:\n without: %v\n with:    %v", errPlain, errOpt)
		}
		return join(
			secQuery, query,
			secError, errOpt.Error(),
		)
	}

	// An EXPLAIN query's own output is the plan, so printing the plan
	// sections as well would just repeat it. It is also the one case where
	// the two engines are expected to disagree, since showing that
	// difference is the point of EXPLAIN.
	if resOpt.IsExplain() {
		return join(secQuery, query, secResult, FormatResult(resOpt))
	}

	if a, b := FormatResult(resPlain), FormatResult(resOpt); a != b {
		t.Fatalf("optimization changed the result\n--- without ---\n%s\n--- with ---\n%s", a, b)
	}

	planPlain, _, err := plain.Plan(query)
	if err != nil {
		t.Fatalf("Plan without the optimizer: %v", err)
	}
	planOpt, rules, err := optimized.Plan(query)
	if err != nil {
		t.Fatalf("Plan with the optimizer: %v", err)
	}

	optSection := planOpt
	if planOpt == planPlain {
		optSection = "(unchanged)"
	}
	if len(rules) > 0 {
		optSection += "\n\nrules applied: " + strings.Join(rules, ", ")
	}
	return join(
		secQuery, query,
		secPlan, planPlain,
		secPlanOpt, optSection,
		secResult, FormatResult(resOpt),
	)
}

// join assembles alternating section headers and bodies.
func join(parts ...string) string {
	var sb strings.Builder
	for i := 0; i < len(parts); i += 2 {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "%s\n%s\n", parts[i], strings.TrimRight(parts[i+1], "\n"))
	}
	return sb.String()
}

// section extracts one section's body. Only the exact header strings count as
// markers, so a SQL line comment inside a query is not mistaken for one.
func section(content, header string) string {
	lines := strings.Split(normalize(content), "\n")
	var out []string
	inside := false
	for _, line := range lines {
		if isSectionHeader(line) {
			if inside {
				break
			}
			inside = line == header
			continue
		}
		if inside {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func isSectionHeader(line string) bool {
	for _, s := range sections {
		if line == s {
			return true
		}
	}
	return false
}

// normalize makes comparison insensitive to the line endings git may have
// handed us on a Windows checkout.
func normalize(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// loadCatalogForFuzz loads data/ outside a *testing.T, for the fuzz target's
// one-time setup.
func loadCatalogForFuzz() (*catalog.Catalog, error) {
	return catalog.Load([]string{filepath.Join("..", "..", "data")})
}
