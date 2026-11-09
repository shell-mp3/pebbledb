// Package engine is the front door: text in, rows or a plan out.
//
// It owns the pipeline and nothing else:
//
//	SQL -> lexer -> parser -> logical plan -> optimizer -> physical plan -> rows
//
// Each stage lives in its own package and knows nothing about the next one,
// so the engine is where they are wired together and where the choice to skip
// the optimizer is made.
package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/catalog"
	"github.com/shell-mp3/pebbledb/internal/logical"
	"github.com/shell-mp3/pebbledb/internal/optimizer"
	"github.com/shell-mp3/pebbledb/internal/parser"
	"github.com/shell-mp3/pebbledb/internal/physical"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// Engine executes queries against a catalog.
type Engine struct {
	Catalog *catalog.Catalog
	// Optimize turns the rewrite rules on. It exists so the REPL and the
	// golden tests can show the same query planned both ways, which is the
	// whole point of the EXPLAIN output in the README.
	Optimize bool
	// MaxRows caps how many rows a query may return, as a guard against a
	// query that would exhaust memory. Zero means no cap.
	MaxRows int
}

// New returns an engine with optimization enabled.
func New(cat *catalog.Catalog) *Engine {
	return &Engine{Catalog: cat, Optimize: true}
}

// Result is what a query produced.
type Result struct {
	// Columns are the output column names, in order.
	Columns []string
	// Kinds are the inferred types of those columns.
	Kinds []types.Kind
	// Rows is the result set. It is nil for an EXPLAIN that was not analyzed.
	Rows []types.Row
	// Plan is the rendered physical plan. It is set only for EXPLAIN.
	Plan string
	// Rules names the optimizer rules that changed the plan.
	Rules []string
	// Elapsed is the wall time spent executing, excluding planning.
	Elapsed time.Duration
}

// IsExplain reports whether the result is a plan rather than a row set.
func (r *Result) IsExplain() bool { return r.Plan != "" }

// Query parses, plans and runs one statement.
func (e *Engine) Query(sql string) (*Result, error) {
	stmt, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}

	var (
		sel     *ast.Select
		explain *ast.Explain
	)
	switch n := stmt.(type) {
	case *ast.Select:
		sel = n
	case *ast.Explain:
		explain, sel = n, n.Stmt
	default:
		return nil, fmt.Errorf("internal error: cannot execute %T", stmt)
	}

	root, rules, err := e.plan(sel)
	if err != nil {
		return nil, err
	}

	// A plain EXPLAIN never runs the query, so the operator tree is torn down
	// without being opened. EXPLAIN ANALYZE runs it and throws the rows away,
	// keeping only the counters the operators recorded.
	if explain != nil && !explain.Analyze {
		defer root.Close()
		return &Result{Plan: physical.Explain(root, false), Rules: rules}, nil
	}

	res, err := e.run(root)
	if err != nil {
		return nil, err
	}
	res.Rules = rules
	if explain != nil {
		res.Plan = physical.Explain(root, true)
		res.Rows, res.Columns, res.Kinds = nil, nil, nil
	}
	return res, nil
}

// Plan returns the rendered plan for a query without running it, which is how
// the golden tests capture the before and after trees for the same SQL.
func (e *Engine) Plan(sql string) (string, []string, error) {
	stmt, err := parser.Parse(sql)
	if err != nil {
		return "", nil, err
	}
	var sel *ast.Select
	switch n := stmt.(type) {
	case *ast.Select:
		sel = n
	case *ast.Explain:
		sel = n.Stmt
	default:
		return "", nil, fmt.Errorf("internal error: cannot plan %T", stmt)
	}
	root, rules, err := e.plan(sel)
	if err != nil {
		return "", nil, err
	}
	defer root.Close()
	return physical.Explain(root, false), rules, nil
}

// plan builds the physical plan, optionally optimizing it first.
func (e *Engine) plan(sel *ast.Select) (physical.Operator, []string, error) {
	plan, err := logical.Build(sel, e.Catalog)
	if err != nil {
		return nil, nil, err
	}
	var rules []string
	if e.Optimize {
		// The rules rewrite in place, so this is the only plan that exists
		// afterwards. Callers wanting both versions build twice.
		plan, rules = optimizer.Optimize(plan)
	}
	root, err := physical.Build(plan)
	if err != nil {
		return nil, nil, err
	}
	return root, rules, nil
}

// run opens the plan, drains it and closes it.
func (e *Engine) run(root physical.Operator) (*Result, error) {
	schema := root.Schema()
	res := &Result{
		Columns: make([]string, schema.Len()),
		Kinds:   make([]types.Kind, schema.Len()),
	}
	for i, c := range schema.Columns {
		res.Columns[i], res.Kinds[i] = c.Name, c.Kind
	}

	start := time.Now()
	defer func() { res.Elapsed = time.Since(start) }()

	if err := root.Open(); err != nil {
		root.Close()
		return nil, err
	}
	for {
		row, ok, err := root.Next()
		if err != nil {
			root.Close()
			return nil, err
		}
		if !ok {
			break
		}
		// Rows are borrowed from the operator, so they must be copied before
		// the next call overwrites them.
		res.Rows = append(res.Rows, row.Clone())
		if e.MaxRows > 0 && len(res.Rows) > e.MaxRows {
			root.Close()
			return nil, fmt.Errorf("query returned more than %d rows; add a LIMIT", e.MaxRows)
		}
	}
	if err := root.Close(); err != nil {
		return nil, err
	}
	return res, nil
}

// Schemas renders every loaded table, for the REPL's .schema command.
func (e *Engine) Schemas() string {
	var sb strings.Builder
	for _, t := range e.Catalog.Tables() {
		fmt.Fprintf(&sb, "%s (%d rows, %s)\n", t.Name, t.Rows, t.Path)
		for _, c := range t.Schema.Columns {
			fmt.Fprintf(&sb, "    %-16s %s\n", c.Name, c.Kind)
		}
	}
	return sb.String()
}
