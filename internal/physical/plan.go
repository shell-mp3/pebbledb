package physical

import (
	"fmt"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/eval"
	"github.com/shell-mp3/pebbledb/internal/logical"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// Build turns a logical plan into an operator tree, compiling every
// expression against the schema its input actually produces.
//
// This is the last stage, and it is where a logical choice becomes a physical
// one: a logical Join becomes a HashJoin with a designated build side, and a
// logical Aggregate becomes a HashAggregate with concrete accumulators.
// pebbledb has exactly one physical operator per logical one, so there is
// nothing to choose between; a real planner would cost several.
func Build(n logical.Node) (Operator, error) {
	switch node := n.(type) {
	case *logical.Scan:
		return NewScan(node.Table, node.Alias, node.AllColumns()), nil

	case *logical.Filter:
		input, err := Build(node.Input)
		if err != nil {
			return nil, err
		}
		pred, err := eval.Compile(node.Predicate, input.Schema())
		if err != nil {
			return nil, closeOnError(input, err)
		}
		return NewFilter(input, pred), nil

	case *logical.Project:
		input, err := Build(node.Input)
		if err != nil {
			return nil, err
		}
		exprs := make([]*eval.Expr, len(node.Items))
		for i, item := range node.Items {
			e, err := eval.Compile(item.Expr, input.Schema())
			if err != nil {
				return nil, closeOnError(input, err)
			}
			exprs[i] = e
		}
		return NewProject(input, exprs, node.Schema()), nil

	case *logical.Sort:
		input, err := Build(node.Input)
		if err != nil {
			return nil, err
		}
		keys := make([]SortKey, len(node.Keys))
		for i, k := range node.Keys {
			e, err := eval.Compile(k.Expr, input.Schema())
			if err != nil {
				return nil, closeOnError(input, err)
			}
			keys[i] = SortKey{Expr: e, Desc: k.Desc}
		}
		return NewSort(input, keys), nil

	case *logical.Limit:
		input, err := Build(node.Input)
		if err != nil {
			return nil, err
		}
		return NewLimit(input, node.Count), nil

	case *logical.Aggregate:
		return buildAggregate(node)

	case *logical.Join:
		return buildJoin(node)
	}
	return nil, fmt.Errorf("internal error: no physical operator for %T", n)
}

func buildAggregate(node *logical.Aggregate) (Operator, error) {
	input, err := Build(node.Input)
	if err != nil {
		return nil, err
	}
	groupBy, err := eval.CompileAll(node.GroupBy, input.Schema())
	if err != nil {
		return nil, closeOnError(input, err)
	}
	specs := make([]AggSpec, len(node.Aggs))
	for i, a := range node.Aggs {
		spec := AggSpec{Func: a.Func}
		if !a.Star {
			arg, err := eval.Compile(a.Arg, input.Schema())
			if err != nil {
				return nil, closeOnError(input, err)
			}
			spec.Arg = arg
		}
		specs[i] = spec
	}
	return NewHashAggregate(input, groupBy, specs, node.Schema()), nil
}

func buildJoin(node *logical.Join) (Operator, error) {
	left, err := Build(node.Left)
	if err != nil {
		return nil, err
	}
	right, err := Build(node.Right)
	if err != nil {
		return nil, closeOnError(left, err)
	}

	leftKeys, rightKeys, residual, err := splitJoinCondition(node.On, left.Schema(), right.Schema())
	if err != nil {
		return nil, closeOnError(left, closeOnError(right, err))
	}

	var leftComp, rightComp []*eval.Expr
	for i := range leftKeys {
		l, err := eval.Compile(leftKeys[i], left.Schema())
		if err != nil {
			return nil, closeOnError(left, closeOnError(right, err))
		}
		r, err := eval.Compile(rightKeys[i], right.Schema())
		if err != nil {
			return nil, closeOnError(left, closeOnError(right, err))
		}
		leftComp = append(leftComp, l)
		rightComp = append(rightComp, r)
	}

	var residualComp *eval.Expr
	if residual != nil {
		joined := left.Schema().Concat(right.Schema())
		if residualComp, err = eval.Compile(residual, joined); err != nil {
			return nil, closeOnError(left, closeOnError(right, err))
		}
	}
	join, err := NewHashJoin(left, right, node.Type, leftComp, rightComp, residualComp)
	if err != nil {
		return nil, closeOnError(left, closeOnError(right, err))
	}
	return join, nil
}

// splitJoinCondition separates the ON condition into equi-join key pairs and
// a residual predicate.
//
// A conjunct counts as a key pair when it is an equality whose two sides draw
// from exactly one input each. Everything else, including a comparison that
// is not an equality and a condition over one side only, becomes residual.
//
// At least one key pair is required: pebbledb has no nested loop join, so a
// condition like ON a.x > b.y would silently become a cross product, and
// refusing it is more honest than computing it slowly.
func splitJoinCondition(on ast.Expr, leftSchema, rightSchema *types.Schema) (leftKeys, rightKeys []ast.Expr, residual ast.Expr, err error) {
	var leftovers []ast.Expr
	for _, conjunct := range ast.SplitConjuncts(on) {
		bin, ok := conjunct.(*ast.Binary)
		if !ok || bin.Op != "=" {
			leftovers = append(leftovers, conjunct)
			continue
		}
		lSide, lOK := sideOf(bin.Left, leftSchema, rightSchema)
		rSide, rOK := sideOf(bin.Right, leftSchema, rightSchema)
		switch {
		case lOK && rOK && lSide == sideLeft && rSide == sideRight:
			leftKeys = append(leftKeys, bin.Left)
			rightKeys = append(rightKeys, bin.Right)
		case lOK && rOK && lSide == sideRight && rSide == sideLeft:
			leftKeys = append(leftKeys, bin.Right)
			rightKeys = append(rightKeys, bin.Left)
		default:
			leftovers = append(leftovers, conjunct)
		}
	}
	if len(leftKeys) == 0 {
		return nil, nil, nil, fmt.Errorf(
			"join condition %s has no equality between the two tables; pebbledb only has a hash join, so it cannot evaluate this",
			on.String())
	}
	return leftKeys, rightKeys, ast.JoinConjuncts(leftovers), nil
}

type joinSide uint8

const (
	sideConstant joinSide = iota
	sideLeft
	sideRight
)

// sideOf reports which input an expression reads from. A constant expression
// belongs to neither side and cannot serve as a key, because hashing it would
// put every row in one bucket.
func sideOf(e ast.Expr, leftSchema, rightSchema *types.Schema) (joinSide, bool) {
	side := sideConstant
	for _, c := range ast.Columns(e) {
		var found joinSide
		switch {
		case resolves(leftSchema, c):
			found = sideLeft
		case resolves(rightSchema, c):
			found = sideRight
		default:
			return sideConstant, false
		}
		if side != sideConstant && side != found {
			return sideConstant, false
		}
		side = found
	}
	return side, side != sideConstant
}

func resolves(s *types.Schema, c *ast.ColumnRef) bool {
	_, err := s.Resolve(c.Table, c.Name)
	return err == nil
}

// closeOnError releases an operator that will never be opened, keeping the
// original error. A build that fails halfway must not leak the file handles
// of the operators it already created.
func closeOnError(op Operator, err error) error {
	if op == nil {
		return err
	}
	_ = op.Close()
	return err
}
