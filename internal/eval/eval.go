// Package eval compiles AST expressions against a schema.
//
// Compilation happens once per query, not once per row: an expression becomes
// a tree of closures with column references already resolved to indices, so
// the inner loop never walks the AST and never looks a name up in a map.
//
// It runs after optimization, because projection pruning changes which
// columns a Scan produces and therefore what index a name resolves to.
package eval

import (
	"fmt"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// Expr is a compiled expression.
type Expr struct {
	src  ast.Expr
	kind types.Kind
	fn   func(types.Row) (types.Value, error)
}

// Eval evaluates the expression against a row.
func (e *Expr) Eval(row types.Row) (types.Value, error) { return e.fn(row) }

// Kind is the inferred result type, used to label output columns.
func (e *Expr) Kind() types.Kind { return e.kind }

// String renders the source expression, which is what EXPLAIN prints.
func (e *Expr) String() string { return e.src.String() }

// Source returns the AST the expression was compiled from.
func (e *Expr) Source() ast.Expr { return e.src }

// CompileAll compiles a list of expressions against the same schema.
func CompileAll(exprs []ast.Expr, s *types.Schema) ([]*Expr, error) {
	out := make([]*Expr, len(exprs))
	for i, e := range exprs {
		c, err := Compile(e, s)
		if err != nil {
			return nil, err
		}
		out[i] = c
	}
	return out, nil
}

// Compile resolves names in e against s and returns an evaluable expression.
func Compile(e ast.Expr, s *types.Schema) (*Expr, error) {
	switch n := e.(type) {
	case *ast.Literal:
		v := n.Value
		return &Expr{src: n, kind: v.Kind, fn: func(types.Row) (types.Value, error) { return v, nil }}, nil

	case *ast.ColumnRef:
		idx, err := s.Resolve(n.Table, n.Name)
		if err != nil {
			return nil, err
		}
		kind := s.Columns[idx].Kind
		ref := n.String()
		return &Expr{src: n, kind: kind, fn: func(row types.Row) (types.Value, error) {
			if idx >= len(row) {
				return types.Null, fmt.Errorf("internal error: column %s resolved to index %d of a %d-column row", ref, idx, len(row))
			}
			return row[idx], nil
		}}, nil

	case *ast.Binary:
		return compileBinary(n, s)

	case *ast.Unary:
		return compileUnary(n, s)

	case *ast.IsNull:
		inner, err := Compile(n.Expr, s)
		if err != nil {
			return nil, err
		}
		negate := n.Negate
		return &Expr{src: n, kind: types.KindBool, fn: func(row types.Row) (types.Value, error) {
			v, err := inner.Eval(row)
			if err != nil {
				return types.Null, err
			}
			return types.NewBool(v.IsNull() != negate), nil
		}}, nil

	case *ast.In:
		return compileIn(n, s)

	case *ast.Aggregate:
		// The planner replaces every aggregate with a reference to a column of
		// the Aggregate node's output before compiling, so reaching this point
		// means a select item was not matched to an aggregate.
		return nil, fmt.Errorf("%s cannot be used here: aggregates are only allowed in the select list, HAVING and ORDER BY of a grouped query", n)
	}
	return nil, fmt.Errorf("internal error: cannot compile %T", e)
}

var comparisons = map[string]func(int) bool{
	"=":  func(c int) bool { return c == 0 },
	"<>": func(c int) bool { return c != 0 },
	"<":  func(c int) bool { return c < 0 },
	"<=": func(c int) bool { return c <= 0 },
	">":  func(c int) bool { return c > 0 },
	">=": func(c int) bool { return c >= 0 },
}

func compileBinary(n *ast.Binary, s *types.Schema) (*Expr, error) {
	left, err := Compile(n.Left, s)
	if err != nil {
		return nil, err
	}
	right, err := Compile(n.Right, s)
	if err != nil {
		return nil, err
	}

	if test, ok := comparisons[n.Op]; ok {
		op := n.Op
		return &Expr{src: n, kind: types.KindBool, fn: func(row types.Row) (types.Value, error) {
			a, err := left.Eval(row)
			if err != nil {
				return types.Null, err
			}
			b, err := right.Eval(row)
			if err != nil {
				return types.Null, err
			}
			// A comparison with NULL is UNKNOWN, not false.
			if a.IsNull() || b.IsNull() {
				return types.Null, nil
			}
			c, err := types.Compare(a, b)
			if err != nil {
				return types.Null, fmt.Errorf("%s %s %s: %w", left, op, right, err)
			}
			return types.NewBool(test(c)), nil
		}}, nil
	}

	switch n.Op {
	case "AND", "OR":
		combine := types.And
		if n.Op == "OR" {
			combine = types.Or
		}
		return &Expr{src: n, kind: types.KindBool, fn: func(row types.Row) (types.Value, error) {
			a, err := left.Eval(row)
			if err != nil {
				return types.Null, err
			}
			b, err := right.Eval(row)
			if err != nil {
				return types.Null, err
			}
			return combine(a, b)
		}}, nil

	case "LIKE":
		return compileLike(n, left, right)

	case "+", "-", "*", "/", "%":
		op := n.Op
		kind := arithKind(op, left.kind, right.kind)
		return &Expr{src: n, kind: kind, fn: func(row types.Row) (types.Value, error) {
			a, err := left.Eval(row)
			if err != nil {
				return types.Null, err
			}
			b, err := right.Eval(row)
			if err != nil {
				return types.Null, err
			}
			return types.Arith(op, a, b)
		}}, nil
	}
	return nil, fmt.Errorf("internal error: unknown binary operator %q", n.Op)
}

func compileUnary(n *ast.Unary, s *types.Schema) (*Expr, error) {
	inner, err := Compile(n.Expr, s)
	if err != nil {
		return nil, err
	}
	switch n.Op {
	case "NOT":
		return &Expr{src: n, kind: types.KindBool, fn: func(row types.Row) (types.Value, error) {
			v, err := inner.Eval(row)
			if err != nil {
				return types.Null, err
			}
			return types.Not(v)
		}}, nil
	case "-":
		return &Expr{src: n, kind: inner.kind, fn: func(row types.Row) (types.Value, error) {
			v, err := inner.Eval(row)
			if err != nil {
				return types.Null, err
			}
			return types.Neg(v)
		}}, nil
	}
	return nil, fmt.Errorf("internal error: unknown unary operator %q", n.Op)
}

func compileIn(n *ast.In, s *types.Schema) (*Expr, error) {
	left, err := Compile(n.Expr, s)
	if err != nil {
		return nil, err
	}
	list, err := CompileAll(n.List, s)
	if err != nil {
		return nil, err
	}
	negate := n.Negate
	return &Expr{src: n, kind: types.KindBool, fn: func(row types.Row) (types.Value, error) {
		probe, err := left.Eval(row)
		if err != nil {
			return types.Null, err
		}
		if probe.IsNull() {
			return types.Null, nil
		}
		// SQL semantics: a match wins outright, but a NULL in the list makes a
		// non-match UNKNOWN rather than FALSE, because the missing value might
		// have been the one that matched.
		sawNull := false
		for _, item := range list {
			v, err := item.Eval(row)
			if err != nil {
				return types.Null, err
			}
			if v.IsNull() {
				sawNull = true
				continue
			}
			c, err := types.Compare(probe, v)
			if err != nil {
				return types.Null, fmt.Errorf("%s IN (...): %w", left, err)
			}
			if c == 0 {
				return types.NewBool(!negate), nil
			}
		}
		if sawNull {
			return types.Null, nil
		}
		return types.NewBool(negate), nil
	}}, nil
}

// arithKind mirrors the promotion rules in types.Arith so output columns can
// be labelled without running the query.
func arithKind(op string, a, b types.Kind) types.Kind {
	if op == "/" {
		return types.KindFloat
	}
	if a == types.KindInt && b == types.KindInt {
		return types.KindInt
	}
	if a.IsNumeric() && b.IsNumeric() {
		return types.KindFloat
	}
	return types.KindNull
}

// InferKind reports the result type of e without compiling it, which is what
// the logical planner uses to label a projection's output columns. It mirrors
// Compile's kind rules; a name that does not resolve is an error here too.
func InferKind(e ast.Expr, s *types.Schema) (types.Kind, error) {
	switch n := e.(type) {
	case *ast.Literal:
		return n.Value.Kind, nil
	case *ast.ColumnRef:
		idx, err := s.Resolve(n.Table, n.Name)
		if err != nil {
			return types.KindNull, err
		}
		return s.Columns[idx].Kind, nil
	case *ast.IsNull, *ast.In:
		return types.KindBool, nil
	case *ast.Unary:
		if n.Op == "NOT" {
			return types.KindBool, nil
		}
		return InferKind(n.Expr, s)
	case *ast.Binary:
		switch n.Op {
		case "AND", "OR", "LIKE", "=", "<>", "<", "<=", ">", ">=":
			return types.KindBool, nil
		}
		a, err := InferKind(n.Left, s)
		if err != nil {
			return types.KindNull, err
		}
		b, err := InferKind(n.Right, s)
		if err != nil {
			return types.KindNull, err
		}
		return arithKind(n.Op, a, b), nil
	case *ast.Aggregate:
		return AggKind(n, s)
	}
	return types.KindNull, fmt.Errorf("internal error: cannot infer the type of %T", e)
}

// AggKind reports the result type of an aggregate call. COUNT is always an
// integer and AVG always a float; SUM, MIN and MAX keep their argument's type.
func AggKind(a *ast.Aggregate, s *types.Schema) (types.Kind, error) {
	switch a.Func {
	case ast.AggCount:
		return types.KindInt, nil
	case ast.AggAvg:
		return types.KindFloat, nil
	}
	argKind, err := InferKind(a.Arg, s)
	if err != nil {
		return types.KindNull, err
	}
	if a.Func == ast.AggSum && !argKind.IsNumeric() && argKind != types.KindNull {
		return types.KindNull, fmt.Errorf("SUM requires a numeric argument, but %s is %s", a.Arg.String(), argKind)
	}
	return argKind, nil
}
