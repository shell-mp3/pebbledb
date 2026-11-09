// Package ast defines the syntax tree produced by the parser.
//
// The AST is deliberately dumb: it records what was written, not what it
// means. Nothing here knows about schemas or column indices. Resolution
// happens later, in the logical planner and the expression compiler, which is
// what lets the optimizer rewrite expressions by pattern rather than by
// re-resolving them.
package ast

import (
	"strconv"
	"strings"

	"github.com/shell-mp3/pebbledb/internal/lexer"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// Statement is a complete parsed query.
type Statement interface {
	statement()
}

// Select is a SELECT query.
type Select struct {
	Items   []SelectItem
	From    *TableRef
	Joins   []Join
	Where   Expr
	GroupBy []Expr
	Having  Expr
	OrderBy []OrderBy
	// Limit is nil when the query has no LIMIT clause.
	Limit *int64
}

func (*Select) statement() {}

// Explain wraps a query whose plan should be printed instead of executed.
// With Analyze set, the query is also run and the plan is annotated with the
// row counts each operator actually produced.
type Explain struct {
	Analyze bool
	Stmt    *Select
}

func (*Explain) statement() {}

// SelectItem is one entry in the select list.
type SelectItem struct {
	// Star is set for "*" and "t.*". StarTable is the qualifier, if any.
	Star      bool
	StarTable string
	Expr      Expr
	// Alias is the AS name, or "" if none was given.
	Alias string
}

// TableRef names a table in FROM or JOIN, with an optional alias.
type TableRef struct {
	Name  string
	Alias string
}

// Name returns the name the table's columns are qualified by, which is the
// alias when one is present.
func (t *TableRef) Qualifier() string {
	if t.Alias != "" {
		return t.Alias
	}
	return t.Name
}

// JoinType distinguishes the join flavours pebbledb supports.
type JoinType uint8

const (
	// InnerJoin keeps only matching pairs.
	InnerJoin JoinType = iota
	// LeftJoin keeps every left row, null-extending unmatched ones.
	LeftJoin
)

func (j JoinType) String() string {
	if j == LeftJoin {
		return "left"
	}
	return "inner"
}

// Join is a single JOIN clause. On is always present: pebbledb has no cross
// join syntax, so a join without ON is a parse error.
type Join struct {
	Type  JoinType
	Table *TableRef
	On    Expr
}

// OrderBy is one ORDER BY key.
type OrderBy struct {
	Expr Expr
	Desc bool
}

// Expr is a scalar expression.
type Expr interface {
	expr()
	// String renders the expression back to SQL-ish text. EXPLAIN output and
	// default column headers are built from it, and the planner uses it as a
	// structural key when matching GROUP BY expressions.
	String() string
}

// ColumnRef is a possibly qualified column reference. Case is preserved as
// written; resolution is case-insensitive.
type ColumnRef struct {
	Table string
	Name  string
}

func (*ColumnRef) expr() {}

func (c *ColumnRef) String() string {
	return types.Ref(QuoteIdent(c.Table), QuoteIdent(c.Name))
}

// QuoteIdent renders an identifier so that re-parsing it yields the same name.
// A name that is a reserved word, or that contains anything outside
// [A-Za-z0-9_], has to be double-quoted. CSV headers are not under the
// author's control, so this is not a rare case.
func QuoteIdent(name string) string {
	if name == "" {
		return ""
	}
	safe := true
	for i := 0; i < len(name); i++ {
		c := name[i]
		alpha := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if alpha || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		safe = false
		break
	}
	if safe && !lexer.IsKeyword(name) {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Literal is a constant.
type Literal struct {
	Value types.Value
}

func (*Literal) expr() {}

func (l *Literal) String() string { return l.Value.SQL() }

// Binary is an infix operator: arithmetic, comparison, AND, OR or LIKE.
type Binary struct {
	Op    string
	Left  Expr
	Right Expr
}

func (*Binary) expr() {}

func (b *Binary) String() string {
	p := precOf(b)
	// Left-associative: an operand of equal precedence needs parentheses only
	// on the right, which is also what keeps a - (b - c) from printing as
	// a - b - c.
	return sub(b.Left, p) + " " + b.Op + " " + sub(b.Right, p+1)
}

// Unary is a prefix operator: NOT or unary minus.
type Unary struct {
	Op   string
	Expr Expr
}

func (*Unary) expr() {}

func (u *Unary) String() string {
	if u.Op == "NOT" {
		return "NOT " + sub(u.Expr, precNot)
	}
	operand := sub(u.Expr, precNeg)
	// "--" starts a line comment, so a negated negative needs parentheses
	// even though precedence does not call for them.
	if strings.HasPrefix(operand, "-") {
		operand = "(" + operand + ")"
	}
	return "-" + operand
}

// IsNull is "x IS NULL" or, with Negate, "x IS NOT NULL".
type IsNull struct {
	Expr   Expr
	Negate bool
}

func (*IsNull) expr() {}

func (i *IsNull) String() string {
	if i.Negate {
		return sub(i.Expr, precAdd) + " IS NOT NULL"
	}
	return sub(i.Expr, precAdd) + " IS NULL"
}

// In is "x IN (a, b, c)" or, with Negate, "x NOT IN (...)". Only explicit
// value lists are supported; there are no subqueries.
type In struct {
	Expr   Expr
	List   []Expr
	Negate bool
}

func (*In) expr() {}

func (i *In) String() string {
	parts := make([]string, len(i.List))
	for k, e := range i.List {
		parts[k] = e.String()
	}
	op := " IN ("
	if i.Negate {
		op = " NOT IN ("
	}
	return sub(i.Expr, precAdd) + op + strings.Join(parts, ", ") + ")"
}

// AggFunc identifies an aggregate function.
type AggFunc uint8

const (
	// AggCount is COUNT(expr) or COUNT(*).
	AggCount AggFunc = iota
	// AggSum is SUM(expr).
	AggSum
	// AggAvg is AVG(expr).
	AggAvg
	// AggMin is MIN(expr).
	AggMin
	// AggMax is MAX(expr).
	AggMax
)

func (a AggFunc) String() string {
	switch a {
	case AggCount:
		return "COUNT"
	case AggSum:
		return "SUM"
	case AggAvg:
		return "AVG"
	case AggMin:
		return "MIN"
	case AggMax:
		return "MAX"
	}
	return "?"
}

// LookupAgg maps a function name to its AggFunc. Names are case-insensitive.
func LookupAgg(name string) (AggFunc, bool) {
	switch strings.ToUpper(name) {
	case "COUNT":
		return AggCount, true
	case "SUM":
		return AggSum, true
	case "AVG":
		return AggAvg, true
	case "MIN":
		return AggMin, true
	case "MAX":
		return AggMax, true
	}
	return 0, false
}

// Aggregate is a call to an aggregate function. Star is set for COUNT(*),
// in which case Arg is nil. pebbledb has no scalar functions, so every
// function call in a query is one of these.
type Aggregate struct {
	Func AggFunc
	Arg  Expr
	Star bool
}

func (*Aggregate) expr() {}

func (a *Aggregate) String() string {
	if a.Star {
		return a.Func.String() + "(*)"
	}
	return a.Func.String() + "(" + a.Arg.String() + ")"
}

// Key returns a canonical, case-insensitive form of e, used to decide whether
// two expressions are "the same" when matching a select item against a GROUP
// BY key. It is a structural comparison, not a semantic one: a + b and b + a
// have different keys.
func Key(e Expr) string { return strings.ToLower(e.String()) }

// DisplayName is the column header used for a select item that has no alias.
// A bare column keeps its own name; anything else is labelled with the
// expression text, the way sqlite does it.
func DisplayName(e Expr) string {
	if c, ok := e.(*ColumnRef); ok {
		return c.Name
	}
	return e.String()
}

// Precedence levels, one per level of the parser's method chain. String uses
// them to add parentheses only where they change meaning, which is the
// difference between EXPLAIN printing
//
//	Filter(o.amount > 500 AND c.country = 'CA')
//
// and the unreadable
//
//	Filter(((o.amount > 500) AND (c.country = 'CA')))
const (
	precOr      = 1
	precAnd     = 2
	precNot     = 3
	precCompare = 4
	precAdd     = 5
	precMul     = 6
	precNeg     = 7
	precAtom    = 8
)

// precOf reports how tightly e binds.
func precOf(e Expr) int {
	switch n := e.(type) {
	case *Binary:
		switch n.Op {
		case "OR":
			return precOr
		case "AND":
			return precAnd
		case "+", "-":
			return precAdd
		case "*", "/", "%":
			return precMul
		}
		// Comparisons and LIKE.
		return precCompare
	case *Unary:
		if n.Op == "NOT" {
			return precNot
		}
		return precNeg
	case *IsNull, *In:
		return precCompare
	}
	return precAtom
}

// sub renders a subexpression, parenthesising it when it binds more loosely
// than its position allows.
func sub(e Expr, minPrec int) string {
	s := e.String()
	if precOf(e) < minPrec {
		return "(" + s + ")"
	}
	return s
}

// Parens renders e fully parenthesized. Nothing in the engine uses it: it
// exists so the parser tests can assert on tree shape, which String
// deliberately hides.
func Parens(e Expr) string {
	switch n := e.(type) {
	case *Binary:
		return "(" + Parens(n.Left) + " " + n.Op + " " + Parens(n.Right) + ")"
	case *Unary:
		if n.Op == "NOT" {
			return "(NOT " + Parens(n.Expr) + ")"
		}
		return "(" + n.Op + Parens(n.Expr) + ")"
	case *IsNull:
		if n.Negate {
			return "(" + Parens(n.Expr) + " IS NOT NULL)"
		}
		return "(" + Parens(n.Expr) + " IS NULL)"
	case *In:
		parts := make([]string, len(n.List))
		for i, item := range n.List {
			parts[i] = Parens(item)
		}
		op := " IN ("
		if n.Negate {
			op = " NOT IN ("
		}
		return "(" + Parens(n.Expr) + op + strings.Join(parts, ", ") + "))"
	case *Aggregate:
		if n.Star {
			return n.Func.String() + "(*)"
		}
		return n.Func.String() + "(" + Parens(n.Arg) + ")"
	}
	return e.String()
}

// Walk calls fn for e and every subexpression, depth first. If fn returns
// false the subtree below that node is skipped.
func Walk(e Expr, fn func(Expr) bool) {
	if e == nil || !fn(e) {
		return
	}
	switch n := e.(type) {
	case *Binary:
		Walk(n.Left, fn)
		Walk(n.Right, fn)
	case *Unary:
		Walk(n.Expr, fn)
	case *IsNull:
		Walk(n.Expr, fn)
	case *In:
		Walk(n.Expr, fn)
		for _, item := range n.List {
			Walk(item, fn)
		}
	case *Aggregate:
		Walk(n.Arg, fn)
	}
}

// Rewrite rebuilds e bottom up, replacing any node for which fn returns a
// non-nil expression. The original tree is never mutated, so the unoptimized
// plan stays printable alongside the optimized one.
func Rewrite(e Expr, fn func(Expr) Expr) Expr {
	if e == nil {
		return nil
	}
	if replacement := fn(e); replacement != nil {
		return replacement
	}
	switch n := e.(type) {
	case *Binary:
		return &Binary{Op: n.Op, Left: Rewrite(n.Left, fn), Right: Rewrite(n.Right, fn)}
	case *Unary:
		return &Unary{Op: n.Op, Expr: Rewrite(n.Expr, fn)}
	case *IsNull:
		return &IsNull{Expr: Rewrite(n.Expr, fn), Negate: n.Negate}
	case *In:
		list := make([]Expr, len(n.List))
		for i, item := range n.List {
			list[i] = Rewrite(item, fn)
		}
		return &In{Expr: Rewrite(n.Expr, fn), List: list, Negate: n.Negate}
	case *Aggregate:
		if n.Star {
			return n
		}
		return &Aggregate{Func: n.Func, Arg: Rewrite(n.Arg, fn), Star: n.Star}
	}
	return e
}

// Columns returns every column reference in e, in order of appearance.
func Columns(e Expr) []*ColumnRef {
	var out []*ColumnRef
	Walk(e, func(n Expr) bool {
		if c, ok := n.(*ColumnRef); ok {
			out = append(out, c)
		}
		return true
	})
	return out
}

// Aggregates returns every aggregate call in e. Nested aggregates are not
// possible, because the parser rejects an aggregate inside an aggregate.
func Aggregates(e Expr) []*Aggregate {
	var out []*Aggregate
	Walk(e, func(n Expr) bool {
		if a, ok := n.(*Aggregate); ok {
			out = append(out, a)
		}
		return true
	})
	return out
}

// SplitConjuncts flattens an AND tree into its top-level conjuncts, which is
// the form predicate pushdown works on.
func SplitConjuncts(e Expr) []Expr {
	if e == nil {
		return nil
	}
	if b, ok := e.(*Binary); ok && b.Op == "AND" {
		return append(SplitConjuncts(b.Left), SplitConjuncts(b.Right)...)
	}
	return []Expr{e}
}

// JoinConjuncts is the inverse of SplitConjuncts. It returns nil for an empty
// slice, which callers read as "no predicate left, drop the Filter node".
func JoinConjuncts(parts []Expr) Expr {
	if len(parts) == 0 {
		return nil
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out = &Binary{Op: "AND", Left: out, Right: p}
	}
	return out
}

// IntString formats an int64 for plan output.
func IntString(i int64) string { return strconv.FormatInt(i, 10) }
