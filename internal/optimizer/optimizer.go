// Package optimizer rewrites a logical plan into a cheaper logical plan.
//
// There are two rules, both classics, and both chosen because their effect is
// visible in EXPLAIN:
//
//   - Predicate pushdown moves each WHERE conjunct as close to the scan that
//     can evaluate it as the join semantics allow, so rows are discarded
//     before they are copied into a hash table.
//   - Projection pruning tells each scan which columns anybody above it
//     actually reads, so the CSV parser does less work per row.
//
// There is no cost model and no statistics beyond the row counts the catalog
// collected: every rewrite here is one that is cheaper in every case, never a
// bet. Join order is left exactly as written.
package optimizer

import (
	"strings"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/logical"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// Rule is one rewrite pass over a whole plan.
type Rule struct {
	Name  string
	Apply func(logical.Node) logical.Node
}

// Rules are applied in order. Pushdown runs first on purpose: it moves
// predicates down to the scans, and pruning then sees that those predicates
// need their columns, so a filtered column is not pruned away.
func Rules() []Rule {
	return []Rule{
		{Name: "predicate pushdown", Apply: PushDownPredicates},
		{Name: "projection pruning", Apply: PruneProjections},
	}
}

// Optimize applies every rule and reports which ones changed the plan. The
// report is what "EXPLAIN" uses to say why the plan looks the way it does.
func Optimize(n logical.Node) (logical.Node, []string) {
	var applied []string
	for _, rule := range Rules() {
		before := logical.Format(n)
		n = rule.Apply(n)
		if logical.Format(n) != before {
			applied = append(applied, rule.Name)
		}
	}
	return n, applied
}

// PushDownPredicates moves filter conjuncts as far down the tree as they can
// go.
func PushDownPredicates(n logical.Node) logical.Node {
	switch node := n.(type) {
	case *logical.Filter:
		// Push the input down first, then try to absorb this filter's
		// conjuncts into it.
		input := PushDownPredicates(node.Input)
		var kept []ast.Expr
		for _, conjunct := range ast.SplitConjuncts(node.Predicate) {
			if pushed, ok := pushInto(input, conjunct); ok {
				input = pushed
				continue
			}
			kept = append(kept, conjunct)
		}
		if remaining := ast.JoinConjuncts(kept); remaining != nil {
			return &logical.Filter{Input: input, Predicate: remaining}
		}
		// Everything moved down, so this node has nothing left to do.
		return input

	case *logical.Project:
		node.Input = PushDownPredicates(node.Input)
		return node
	case *logical.Aggregate:
		node.Input = PushDownPredicates(node.Input)
		return node
	case *logical.Sort:
		node.Input = PushDownPredicates(node.Input)
		return node
	case *logical.Limit:
		node.Input = PushDownPredicates(node.Input)
		return node
	case *logical.Join:
		node.Left = PushDownPredicates(node.Left)
		node.Right = PushDownPredicates(node.Right)
		return node
	}
	return n
}

// pushInto tries to attach one conjunct below n, returning the rewritten
// subtree. It reports false when the move would be unsafe or pointless, and
// the caller then leaves the conjunct where it was.
func pushInto(n logical.Node, conjunct ast.Expr) (logical.Node, bool) {
	switch node := n.(type) {
	case *logical.Join:
		tables, ok := tablesOf(conjunct, node.Schema())
		if !ok {
			return nil, false
		}
		if subsetOf(tables, node.Left.Schema()) {
			// Safe for both join types: restricting the left input of a left
			// join only removes rows that would have been dropped above.
			if pushed, ok := pushInto(node.Left, conjunct); ok {
				node.Left = pushed
				return node, true
			}
			node.Left = &logical.Filter{Input: node.Left, Predicate: conjunct}
			return node, true
		}
		if subsetOf(tables, node.Right.Schema()) {
			// Not safe for a left join: dropping right rows would turn a
			// matched row into a null-extended one instead of removing it.
			if node.Type != ast.InnerJoin {
				return nil, false
			}
			if pushed, ok := pushInto(node.Right, conjunct); ok {
				node.Right = pushed
				return node, true
			}
			node.Right = &logical.Filter{Input: node.Right, Predicate: conjunct}
			return node, true
		}
		// The conjunct spans both sides, so it has to stay above the join.
		return nil, false

	case *logical.Filter:
		// Merge into the filter already there rather than stacking another.
		node.Predicate = ast.JoinConjuncts([]ast.Expr{node.Predicate, conjunct})
		return node, true

	case *logical.Scan:
		return &logical.Filter{Input: node, Predicate: conjunct}, true
	}
	// Aggregate, Sort, Limit and Project are not pushed through: below an
	// Aggregate the predicate would see pre-grouping rows, and below a Limit
	// it would change which rows the limit keeps.
	return nil, false
}

// tablesOf returns the qualifiers the conjunct reads from, resolving
// unqualified names against s. It reports false if any reference does not
// resolve, since an unresolvable predicate must not be moved.
func tablesOf(e ast.Expr, s *types.Schema) (map[string]bool, bool) {
	out := map[string]bool{}
	for _, c := range ast.Columns(e) {
		idx, err := s.Resolve(c.Table, c.Name)
		if err != nil {
			return nil, false
		}
		out[strings.ToLower(s.Columns[idx].Table)] = true
	}
	return out, true
}

// subsetOf reports whether every table in tables appears in s. A conjunct
// over no tables at all (a constant predicate) is not pushed, because there
// is nothing to gain and it would print confusingly deep in the plan.
func subsetOf(tables map[string]bool, s *types.Schema) bool {
	if len(tables) == 0 {
		return false
	}
	have := map[string]bool{}
	for _, c := range s.Columns {
		have[strings.ToLower(c.Table)] = true
	}
	for t := range tables {
		if !have[t] {
			return false
		}
	}
	return true
}

// PruneProjections walks down the plan carrying the set of columns the nodes
// above it need, and narrows each Scan to just those columns.
func PruneProjections(n logical.Node) logical.Node {
	return prune(n, map[string]bool{})
}

// prune rewrites n given the columns its parent requires. required holds
// logical.ColumnKey values.
func prune(n logical.Node, required map[string]bool) logical.Node {
	switch node := n.(type) {
	case *logical.Project:
		// Project defines its own requirements; whatever the parent wanted is
		// satisfied by this node's outputs.
		need, ok := logical.RequiredColumns(exprsOfItems(node.Items), node.Input.Schema())
		if !ok {
			return node
		}
		node.Input = prune(node.Input, need)
		return node

	case *logical.Aggregate:
		// Same: everything above an Aggregate reads its output columns, which
		// are computed from the group keys and the aggregate arguments.
		exprs := append([]ast.Expr{}, node.GroupBy...)
		for _, a := range node.Aggs {
			if a.Arg != nil {
				exprs = append(exprs, a.Arg)
			}
		}
		need, ok := logical.RequiredColumns(exprs, node.Input.Schema())
		if !ok {
			return node
		}
		node.Input = prune(node.Input, need)
		return node

	case *logical.Filter:
		need, ok := logical.RequiredColumns([]ast.Expr{node.Predicate}, node.Input.Schema())
		if !ok {
			return node
		}
		node.Input = prune(node.Input, union(required, need))
		return node

	case *logical.Sort:
		exprs := make([]ast.Expr, len(node.Keys))
		for i, k := range node.Keys {
			exprs[i] = k.Expr
		}
		need, ok := logical.RequiredColumns(exprs, node.Input.Schema())
		if !ok {
			return node
		}
		node.Input = prune(node.Input, union(required, need))
		return node

	case *logical.Join:
		need, ok := logical.RequiredColumns([]ast.Expr{node.On}, node.Schema())
		if !ok {
			return node
		}
		all := union(required, need)
		node.Left = prune(node.Left, all)
		node.Right = prune(node.Right, all)
		return node

	case *logical.Limit:
		node.Input = prune(node.Input, required)
		return node

	case *logical.Scan:
		// Keep the columns somebody asked for, in file order. An empty result
		// is correct and useful: SELECT COUNT(*) reads no fields at all.
		var keep []int
		for _, i := range node.AllColumns() {
			c := node.Table.Schema.Columns[i]
			c.Table = node.Alias
			if required[logical.ColumnKey(c)] {
				keep = append(keep, i)
			}
		}
		if keep == nil {
			keep = []int{}
		}
		node.Columns = keep
		return node
	}
	return n
}

func exprsOfItems(items []logical.ProjectItem) []ast.Expr {
	out := make([]ast.Expr, len(items))
	for i, item := range items {
		out[i] = item.Expr
	}
	return out
}

func union(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}
