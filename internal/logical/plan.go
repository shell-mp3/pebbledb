// Package logical is the relational algebra layer: what the query asks for,
// with names resolved to tables but no decision yet about how to execute it.
//
// Logical nodes still carry AST expressions rather than compiled ones. That
// is what makes the optimizer possible: pushing a predicate down means moving
// an ast.Expr to a different node, and the expression is only compiled
// against a schema once the plan has stopped changing.
package logical

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/catalog"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// Node is one relational operator.
type Node interface {
	// Schema is the node's output columns.
	Schema() *types.Schema
	// Children returns the inputs, left to right.
	Children() []Node
	// Label is a one-line description used by Format.
	Label() string
}

// Scan reads a CSV table.
type Scan struct {
	Table *catalog.Table
	// Alias is the name columns are qualified by: the AS name, or the table
	// name when there is none.
	Alias string
	// Columns selects which of the table's columns to produce, by index. A nil
	// slice means all of them; projection pruning narrows it. An empty but
	// non-nil slice is legitimate and means the query needs no column values
	// at all, as in SELECT COUNT(*).
	Columns []int
}

// AllColumns returns the column indices the scan produces.
func (s *Scan) AllColumns() []int {
	if s.Columns != nil {
		return s.Columns
	}
	idx := make([]int, s.Table.Schema.Len())
	for i := range idx {
		idx[i] = i
	}
	return idx
}

// Schema is recomputed rather than cached, because projection pruning edits
// Columns in place after the plan is built.
func (s *Scan) Schema() *types.Schema {
	idx := s.AllColumns()
	cols := make([]types.Column, len(idx))
	for i, j := range idx {
		c := s.Table.Schema.Columns[j]
		c.Table = s.Alias
		cols[i] = c
	}
	return types.NewSchema(cols...)
}

// Children returns nothing: Scan is a leaf.
func (s *Scan) Children() []Node { return nil }

// Label describes the scan, including how much of the file it reads.
func (s *Scan) Label() string {
	name := s.Table.Name
	if !strings.EqualFold(s.Alias, s.Table.Name) {
		name += " AS " + s.Alias
	}
	idx := s.AllColumns()
	names := make([]string, len(idx))
	for i, j := range idx {
		names[i] = s.Table.Schema.Columns[j].Name
	}
	cols := "none"
	if len(names) > 0 {
		cols = strings.Join(names, ", ")
	}
	return fmt.Sprintf("Scan(%s) [%d/%d cols: %s]", name, len(idx), s.Table.Schema.Len(), cols)
}

// Filter drops rows for which Predicate is not TRUE.
type Filter struct {
	Input     Node
	Predicate ast.Expr
}

// Schema passes the input through unchanged.
func (f *Filter) Schema() *types.Schema { return f.Input.Schema() }

// Children returns the input.
func (f *Filter) Children() []Node { return []Node{f.Input} }

// Label describes the predicate.
func (f *Filter) Label() string { return "Filter(" + f.Predicate.String() + ")" }

// ProjectItem is one output column of a Project.
type ProjectItem struct {
	Expr ast.Expr
	// Name is the alias, or the expression text when there is no alias.
	Name string
}

// Project computes the select list.
type Project struct {
	Input Node
	Items []ProjectItem
	// schema is fixed at build time: the optimizer never changes an item's
	// name or type, only where its inputs come from.
	schema *types.Schema
}

// Schema returns the projected columns.
func (p *Project) Schema() *types.Schema { return p.schema }

// Children returns the input.
func (p *Project) Children() []Node { return []Node{p.Input} }

// Label lists the output columns.
func (p *Project) Label() string {
	parts := make([]string, len(p.Items))
	for i, item := range p.Items {
		parts[i] = item.Expr.String()
		if item.Name != ast.DisplayName(item.Expr) {
			parts[i] += " AS " + item.Name
		}
	}
	return "Project(" + strings.Join(parts, ", ") + ")"
}

// Join combines two inputs on a condition.
type Join struct {
	Left  Node
	Right Node
	Type  ast.JoinType
	On    ast.Expr
}

// Schema is the left columns followed by the right columns.
func (j *Join) Schema() *types.Schema { return j.Left.Schema().Concat(j.Right.Schema()) }

// Children returns the left then the right input.
func (j *Join) Children() []Node { return []Node{j.Left, j.Right} }

// Label describes the join type and condition.
func (j *Join) Label() string {
	return fmt.Sprintf("Join(%s, %s)", j.Type, j.On.String())
}

// Aggregate groups its input and computes aggregate functions.
//
// Its output columns are the group keys followed by the aggregate results.
// Everything above it refers to those columns by name, which is why the
// builder rewrites SUM(o.amount) in the select list into a reference to the
// column this node calls "SUM(o.amount)".
type Aggregate struct {
	Input   Node
	GroupBy []ast.Expr
	Aggs    []*ast.Aggregate
	schema  *types.Schema
}

// Schema returns the group key columns followed by the aggregate columns.
func (a *Aggregate) Schema() *types.Schema { return a.schema }

// Children returns the input.
func (a *Aggregate) Children() []Node { return []Node{a.Input} }

// Label lists the group keys and the aggregates.
func (a *Aggregate) Label() string {
	var parts []string
	if len(a.GroupBy) > 0 {
		keys := make([]string, len(a.GroupBy))
		for i, g := range a.GroupBy {
			keys[i] = g.String()
		}
		parts = append(parts, "group="+strings.Join(keys, ", "))
	}
	aggs := make([]string, len(a.Aggs))
	for i, g := range a.Aggs {
		aggs[i] = g.String()
	}
	parts = append(parts, "aggs="+strings.Join(aggs, ", "))
	return "Aggregate(" + strings.Join(parts, " ") + ")"
}

// SortKey is one ORDER BY key.
type SortKey struct {
	Expr ast.Expr
	Desc bool
}

// Sort orders its input.
type Sort struct {
	Input Node
	Keys  []SortKey
}

// Schema passes the input through unchanged.
func (s *Sort) Schema() *types.Schema { return s.Input.Schema() }

// Children returns the input.
func (s *Sort) Children() []Node { return []Node{s.Input} }

// Label lists the sort keys and their directions.
func (s *Sort) Label() string {
	parts := make([]string, len(s.Keys))
	for i, k := range s.Keys {
		parts[i] = k.Expr.String()
		if k.Desc {
			parts[i] += " DESC"
		}
	}
	return "Sort(" + strings.Join(parts, ", ") + ")"
}

// Limit stops after Count rows.
type Limit struct {
	Input Node
	Count int64
}

// Schema passes the input through unchanged.
func (l *Limit) Schema() *types.Schema { return l.Input.Schema() }

// Children returns the input.
func (l *Limit) Children() []Node { return []Node{l.Input} }

// Label shows the row cap.
func (l *Limit) Label() string { return "Limit(" + strconv.FormatInt(l.Count, 10) + ")" }

// Format renders a plan as an indented tree. The physical plan has its own
// printer with more to say; this one exists so optimizer tests can assert on
// a whole plan shape in one string.
func Format(n Node) string {
	var sb strings.Builder
	format(&sb, n, "", true, true)
	return sb.String()
}

func format(sb *strings.Builder, n Node, prefix string, last, root bool) {
	if !root {
		sb.WriteString(prefix)
		if last {
			sb.WriteString("└─ ")
			prefix += "   "
		} else {
			sb.WriteString("├─ ")
			prefix += "│  "
		}
	}
	sb.WriteString(n.Label())
	sb.WriteString("\n")
	kids := n.Children()
	for i, kid := range kids {
		format(sb, kid, prefix, i == len(kids)-1, false)
	}
}

// RequiredColumns returns the set of columns a list of expressions needs, as
// lower-cased "table.name" keys resolved against s. The second result is
// false when some reference does not resolve, which tells the optimizer to
// leave that part of the plan alone rather than guess.
func RequiredColumns(exprs []ast.Expr, s *types.Schema) (map[string]bool, bool) {
	out := map[string]bool{}
	for _, e := range exprs {
		for _, c := range ast.Columns(e) {
			idx, err := s.Resolve(c.Table, c.Name)
			if err != nil {
				return nil, false
			}
			out[ColumnKey(s.Columns[idx])] = true
		}
	}
	return out, true
}

// ColumnKey is the identity of a column within a plan: its qualifier and
// name, lower-cased.
func ColumnKey(c types.Column) string {
	return strings.ToLower(c.Table + "." + c.Name)
}
