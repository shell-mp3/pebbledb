package logical

import (
	"fmt"
	"strings"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/catalog"
	"github.com/shell-mp3/pebbledb/internal/eval"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// Build turns a parsed SELECT into a logical plan, resolving every name
// against the catalog as it goes. The plan is assembled bottom up in the
// order SQL evaluates it:
//
//	Scan -> Join* -> Filter(WHERE) -> Aggregate -> Filter(HAVING) -> Sort -> Project -> Limit
//
// Sort sits below Project so that ORDER BY can name a column the select list
// does not output, which is the common case for "cheapest first, show only
// the name".
func Build(sel *ast.Select, cat *catalog.Catalog) (Node, error) {
	node, err := buildFrom(sel, cat)
	if err != nil {
		return nil, err
	}

	if sel.Where != nil {
		if _, err := eval.InferKind(sel.Where, node.Schema()); err != nil {
			return nil, fmt.Errorf("in WHERE: %w", err)
		}
		node = &Filter{Input: node, Predicate: sel.Where}
	}

	items, err := expandSelectList(sel.Items, node.Schema())
	if err != nil {
		return nil, err
	}

	// ORDER BY may name an output alias, so substitute aliases before the
	// aggregate rewrite sees the keys.
	keys := resolveOrderByAliases(sel.OrderBy, items)

	having := sel.Having
	if len(sel.GroupBy) > 0 || hasAggregates(items, having, keys) {
		node, items, having, keys, err = buildAggregate(node, sel.GroupBy, items, having, keys)
		if err != nil {
			return nil, err
		}
	} else if having != nil {
		return nil, fmt.Errorf("HAVING requires GROUP BY or an aggregate in the select list")
	}

	if having != nil {
		if _, err := eval.InferKind(having, node.Schema()); err != nil {
			return nil, fmt.Errorf("in HAVING: %w", err)
		}
		node = &Filter{Input: node, Predicate: having}
	}

	if len(keys) > 0 {
		for _, k := range keys {
			if _, err := eval.InferKind(k.Expr, node.Schema()); err != nil {
				return nil, fmt.Errorf("in ORDER BY: %w", err)
			}
		}
		node = &Sort{Input: node, Keys: keys}
	}

	project, err := newProject(node, items)
	if err != nil {
		return nil, err
	}
	node = project

	if sel.Limit != nil {
		node = &Limit{Input: node, Count: *sel.Limit}
	}
	return node, nil
}

// buildFrom builds the scan and join chain. Joins are left-deep, in the order
// written: there is no join reordering, so the author controls the shape.
func buildFrom(sel *ast.Select, cat *catalog.Catalog) (Node, error) {
	seen := map[string]bool{}
	node, err := buildScan(sel.From, cat, seen)
	if err != nil {
		return nil, err
	}
	for _, j := range sel.Joins {
		right, err := buildScan(j.Table, cat, seen)
		if err != nil {
			return nil, err
		}
		join := &Join{Left: node, Right: right, Type: j.Type, On: j.On}
		if _, err := eval.InferKind(j.On, join.Schema()); err != nil {
			return nil, fmt.Errorf("in JOIN ... ON: %w", err)
		}
		node = join
	}
	return node, nil
}

// buildScan resolves one table reference. Reusing a qualifier is rejected:
// "FROM orders JOIN orders" would make every unqualified name ambiguous and
// every qualified one wrong.
func buildScan(ref *ast.TableRef, cat *catalog.Catalog, seen map[string]bool) (Node, error) {
	tbl, err := cat.Get(ref.Name)
	if err != nil {
		return nil, err
	}
	q := ref.Qualifier()
	if seen[strings.ToLower(q)] {
		return nil, fmt.Errorf("table name %q is used twice; give one of them an alias", q)
	}
	seen[strings.ToLower(q)] = true
	return &Scan{Table: tbl, Alias: q}, nil
}

// expandSelectList turns * and t.* into explicit columns and gives every item
// an output name.
func expandSelectList(items []ast.SelectItem, s *types.Schema) ([]ProjectItem, error) {
	var out []ProjectItem
	for _, item := range items {
		if !item.Star {
			if _, err := eval.InferKind(item.Expr, s); err != nil {
				return nil, err
			}
			name := item.Alias
			if name == "" {
				name = ast.DisplayName(item.Expr)
			}
			out = append(out, ProjectItem{Expr: item.Expr, Name: name})
			continue
		}
		matched := 0
		for _, c := range s.Columns {
			if item.StarTable != "" && !strings.EqualFold(c.Table, item.StarTable) {
				continue
			}
			matched++
			out = append(out, ProjectItem{
				Expr: &ast.ColumnRef{Table: c.Table, Name: c.Name},
				Name: c.Name,
			})
		}
		if matched == 0 {
			if item.StarTable != "" {
				return nil, fmt.Errorf("no table named %q in this query (have %s)", item.StarTable, strings.Join(s.Tables(), ", "))
			}
			return nil, fmt.Errorf("* selected no columns")
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the select list is empty")
	}
	return out, nil
}

// newProject infers each output column's type and builds the node's schema.
func newProject(input Node, items []ProjectItem) (*Project, error) {
	cols := make([]types.Column, len(items))
	for i, item := range items {
		kind, err := eval.InferKind(item.Expr, input.Schema())
		if err != nil {
			return nil, err
		}
		cols[i] = types.Column{Name: item.Name, Kind: kind}
	}
	return &Project{Input: input, Items: items, schema: types.NewSchema(cols...)}, nil
}

// resolveOrderByAliases replaces a bare ORDER BY name that matches an output
// alias with that item's expression, so ORDER BY total works when total is
// defined as SUM(amount) in the select list.
//
// Output names win over input columns, which is what Postgres does: if a
// query says SELECT a AS b ... ORDER BY b, it means the output column.
func resolveOrderByAliases(keys []ast.OrderBy, items []ProjectItem) []SortKey {
	out := make([]SortKey, len(keys))
	for i, k := range keys {
		expr := k.Expr
		if c, ok := expr.(*ast.ColumnRef); ok && c.Table == "" {
			for _, item := range items {
				if strings.EqualFold(item.Name, c.Name) {
					expr = item.Expr
					break
				}
			}
		}
		out[i] = SortKey{Expr: expr, Desc: k.Desc}
	}
	return out
}

func hasAggregates(items []ProjectItem, having ast.Expr, keys []SortKey) bool {
	for _, item := range items {
		if len(ast.Aggregates(item.Expr)) > 0 {
			return true
		}
	}
	if len(ast.Aggregates(having)) > 0 {
		return true
	}
	for _, k := range keys {
		if len(ast.Aggregates(k.Expr)) > 0 {
			return true
		}
	}
	return false
}

// buildAggregate inserts the Aggregate node and rewrites everything above it.
//
// The rewrite is the heart of grouped queries: the Aggregate node produces one
// column per group key and one per distinct aggregate call, and every
// expression above it is rewritten to reference those columns. A select item
// that survives the rewrite still holding a raw column reference is exactly
// the "column must appear in GROUP BY" error, caught here rather than as a
// confusing "no such column" at compile time.
func buildAggregate(
	input Node,
	groupBy []ast.Expr,
	items []ProjectItem,
	having ast.Expr,
	keys []SortKey,
) (Node, []ProjectItem, ast.Expr, []SortKey, error) {
	inSchema := input.Schema()

	// Group keys become output columns. A bare column keeps its qualifier and
	// name so that SELECT c.country still resolves above the Aggregate; any
	// other expression is named by its text.
	cols := make([]types.Column, 0, len(groupBy))
	groupRef := make([]ast.Expr, len(groupBy))
	byKey := map[string]ast.Expr{}
	for i, g := range groupBy {
		kind, err := eval.InferKind(g, inSchema)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("in GROUP BY: %w", err)
		}
		col := types.Column{Name: g.String(), Kind: kind}
		if c, ok := g.(*ast.ColumnRef); ok {
			idx, err := inSchema.Resolve(c.Table, c.Name)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("in GROUP BY: %w", err)
			}
			col.Table = inSchema.Columns[idx].Table
			col.Name = inSchema.Columns[idx].Name
		}
		cols = append(cols, col)
		groupRef[i] = &ast.ColumnRef{Table: col.Table, Name: col.Name}
		byKey[ast.Key(g)] = groupRef[i]
	}

	// Collect the distinct aggregate calls used anywhere above the Aggregate.
	var aggs []*ast.Aggregate
	aggRef := map[string]ast.Expr{}
	collect := func(e ast.Expr) error {
		for _, a := range ast.Aggregates(e) {
			key := ast.Key(a)
			if _, ok := aggRef[key]; ok {
				continue
			}
			kind, err := eval.AggKind(a, inSchema)
			if err != nil {
				return err
			}
			name := a.String()
			aggs = append(aggs, a)
			cols = append(cols, types.Column{Name: name, Kind: kind})
			aggRef[key] = &ast.ColumnRef{Name: name}
		}
		return nil
	}
	for _, item := range items {
		if err := collect(item.Expr); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	if err := collect(having); err != nil {
		return nil, nil, nil, nil, err
	}
	for _, k := range keys {
		if err := collect(k.Expr); err != nil {
			return nil, nil, nil, nil, err
		}
	}

	agg := &Aggregate{
		Input:   input,
		GroupBy: groupBy,
		Aggs:    aggs,
		schema:  types.NewSchema(cols...),
	}

	// Aggregates are matched before group keys so that SUM(qty) is replaced
	// whole, rather than having its qty argument swapped for a group
	// reference.
	rewrite := func(e ast.Expr) ast.Expr {
		if a, ok := e.(*ast.Aggregate); ok {
			if ref, ok := aggRef[ast.Key(a)]; ok {
				return ref
			}
			return nil
		}
		if ref, ok := byKey[ast.Key(e)]; ok {
			return ref
		}
		return nil
	}

	outItems := make([]ProjectItem, len(items))
	for i, item := range items {
		expr := ast.Rewrite(item.Expr, rewrite)
		if err := checkGrouped(expr, agg.schema, "the select list"); err != nil {
			return nil, nil, nil, nil, err
		}
		outItems[i] = ProjectItem{Expr: expr, Name: item.Name}
	}

	var outHaving ast.Expr
	if having != nil {
		outHaving = ast.Rewrite(having, rewrite)
		if err := checkGrouped(outHaving, agg.schema, "HAVING"); err != nil {
			return nil, nil, nil, nil, err
		}
	}

	outKeys := make([]SortKey, len(keys))
	for i, k := range keys {
		expr := ast.Rewrite(k.Expr, rewrite)
		if err := checkGrouped(expr, agg.schema, "ORDER BY"); err != nil {
			return nil, nil, nil, nil, err
		}
		outKeys[i] = SortKey{Expr: expr, Desc: k.Desc}
	}
	return agg, outItems, outHaving, outKeys, nil
}

// checkGrouped reports the column that a grouped query may not reference
// bare. Running it before compilation turns a cryptic resolution failure into
// the error the user expects.
func checkGrouped(e ast.Expr, aggSchema *types.Schema, where string) error {
	for _, c := range ast.Columns(e) {
		if _, err := aggSchema.Resolve(c.Table, c.Name); err != nil {
			return fmt.Errorf("%s in %s must appear in GROUP BY or be used in an aggregate function", c.String(), where)
		}
	}
	return nil
}
