package physical

import (
	"fmt"
	"strings"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/eval"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// HashJoin joins two inputs on one or more equalities.
//
// The right input is always the build side: it is read in full during Open
// into a hash table keyed by the join columns, and the left input is then
// streamed through as the probe side. There is no cost-based choice of which
// side to build, so the author's join order decides which table is
// materialised.
//
// Any part of the ON condition that is not an equality between the two sides
// becomes a residual predicate, checked on each candidate pair after the hash
// lookup.
type HashJoin struct {
	stats
	left, right Operator
	joinType    ast.JoinType
	leftKeys    []*eval.Expr
	rightKeys   []*eval.Expr
	residual    *eval.Expr
	schema      *types.Schema

	// table maps an encoded key to the build rows that share it. buildOrder
	// is not needed: rows within a bucket keep insertion order, and probe
	// order follows the left input, so output order is deterministic.
	table map[string][]types.Row

	// Probe state.
	opened   bool
	probeRow types.Row
	bucket   []types.Row
	bucketAt int
	matched  bool
	keyBuf   []byte
	leftVals types.Row
	out      types.Row
	rightPad int
}

// NewHashJoin builds a hash join. leftKeys and rightKeys must be the same
// length and are compared pairwise.
func NewHashJoin(left, right Operator, joinType ast.JoinType, leftKeys, rightKeys []*eval.Expr, residual *eval.Expr) (*HashJoin, error) {
	if len(leftKeys) != len(rightKeys) {
		return nil, fmt.Errorf("internal error: %d left join keys but %d right", len(leftKeys), len(rightKeys))
	}
	if len(leftKeys) == 0 {
		return nil, fmt.Errorf("internal error: hash join needs at least one equality")
	}
	schema := left.Schema().Concat(right.Schema())
	return &HashJoin{
		left:      left,
		right:     right,
		joinType:  joinType,
		leftKeys:  leftKeys,
		rightKeys: rightKeys,
		residual:  residual,
		schema:    schema,
		leftVals:  make(types.Row, len(leftKeys)),
		out:       make(types.Row, schema.Len()),
		rightPad:  right.Schema().Len(),
	}, nil
}

// Open builds the hash table from the right input, then opens the left.
func (h *HashJoin) Open() error {
	return h.measureOpen(func() error {
		if err := h.right.Open(); err != nil {
			return err
		}
		h.table = map[string][]types.Row{}
		keyVals := make(types.Row, len(h.rightKeys))
		var buf []byte
		for {
			row, ok, err := h.right.Next()
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			for i, k := range h.rightKeys {
				v, err := k.Eval(row)
				if err != nil {
					return err
				}
				keyVals[i] = v
			}
			buf, ok = JoinKey(buf, keyVals)
			if !ok {
				// A NULL key can never match, so this row is dropped from the
				// build side entirely.
				continue
			}
			key := string(buf)
			h.table[key] = append(h.table[key], row.Clone())
		}
		if err := h.left.Open(); err != nil {
			return err
		}
		h.opened = true
		h.bucket, h.bucketAt, h.probeRow = nil, 0, nil
		return nil
	})
}

// Next returns the next joined row.
func (h *HashJoin) Next() (types.Row, bool, error) { return h.measure(h.next) }

func (h *HashJoin) next() (types.Row, bool, error) {
	if !h.opened {
		return nil, false, errNotOpen
	}
	for {
		// Finish the current probe row's bucket before pulling another.
		for h.bucketAt < len(h.bucket) {
			build := h.bucket[h.bucketAt]
			h.bucketAt++
			row := h.combine(h.probeRow, build)
			if h.residual != nil {
				v, err := h.residual.Eval(row)
				if err != nil {
					return nil, false, err
				}
				pass, err := types.IsTrue(v)
				if err != nil {
					return nil, false, err
				}
				if !pass {
					continue
				}
			}
			h.matched = true
			return row, true, nil
		}
		// A left join emits the unmatched probe row once, null-extended.
		if h.probeRow != nil && !h.matched && h.joinType == ast.LeftJoin {
			h.matched = true
			return h.combine(h.probeRow, nil), true, nil
		}

		row, ok, err := h.left.Next()
		if err != nil || !ok {
			return nil, false, err
		}
		for i, k := range h.leftKeys {
			v, err := k.Eval(row)
			if err != nil {
				return nil, false, err
			}
			h.leftVals[i] = v
		}
		// The probe row is reused by the left input, so keep a copy for the
		// duration of its bucket.
		h.probeRow = row.Clone()
		h.matched = false
		h.bucket, h.bucketAt = nil, 0
		if h.keyBuf, ok = JoinKey(h.keyBuf, h.leftVals); ok {
			h.bucket = h.table[string(h.keyBuf)]
		}
	}
}

// combine writes probe ++ build into the reused output row. A nil build row
// means null-extend, for an unmatched left join row.
func (h *HashJoin) combine(probe, build types.Row) types.Row {
	n := copy(h.out, probe)
	if build == nil {
		for i := 0; i < h.rightPad; i++ {
			h.out[n+i] = types.Null
		}
		return h.out
	}
	copy(h.out[n:], build)
	return h.out
}

// Close releases the hash table and closes both inputs.
func (h *HashJoin) Close() error {
	h.table = nil
	h.bucket = nil
	h.opened = false
	return closeAll(h.left, h.right)
}

// Schema returns the left columns followed by the right columns.
func (h *HashJoin) Schema() *types.Schema { return h.schema }

// Children returns the probe side then the build side.
func (h *HashJoin) Children() []Operator { return []Operator{h.left, h.right} }

// ChildLabels names the two sides, since which one is materialised is the
// most consequential thing about a hash join.
func (h *HashJoin) ChildLabels() []string { return []string{"probe", "build"} }

// Label shows the join type and the equalities.
func (h *HashJoin) Label() string {
	parts := make([]string, len(h.leftKeys))
	for i := range h.leftKeys {
		parts[i] = h.leftKeys[i].String() + " = " + h.rightKeys[i].String()
	}
	out := fmt.Sprintf("HashJoin(%s, on %s", h.joinType, strings.Join(parts, " AND "))
	if h.residual != nil {
		out += ", residual " + h.residual.String()
	}
	return out + ")"
}
