// Package types defines the value and schema model shared by every stage of
// the engine: the parser produces literals as Values, the planner reasons
// about Schemas, and the volcano iterators pass Rows between each other.
package types

import (
	"fmt"
	"strconv"
	"strings"
)

// Kind is the dynamic type of a Value. pebbledb has no boolean column type:
// Bool values only ever come from comparisons and literals in a query.
type Kind uint8

const (
	// KindNull is the type of the SQL NULL value.
	KindNull Kind = iota
	// KindInt is a 64-bit signed integer.
	KindInt
	// KindFloat is a 64-bit IEEE-754 float.
	KindFloat
	// KindString is a UTF-8 string.
	KindString
	// KindBool is a boolean, which has no CSV representation.
	KindBool
)

func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindInt:
		return "int"
	case KindFloat:
		return "float"
	case KindString:
		return "string"
	case KindBool:
		return "bool"
	}
	return "unknown"
}

// IsNumeric reports whether k participates in arithmetic.
func (k Kind) IsNumeric() bool { return k == KindInt || k == KindFloat }

// Value is a single SQL datum. It is a tagged union stored by value so that a
// Row is one allocation rather than one per cell.
type Value struct {
	Kind Kind
	Int  int64
	Flt  float64
	Str  string
	Bool bool
}

// Null is the SQL NULL value.
var Null = Value{Kind: KindNull}

// True and False are the boolean constants.
var (
	True  = Value{Kind: KindBool, Bool: true}
	False = Value{Kind: KindBool, Bool: false}
)

// NewInt returns an integer Value.
func NewInt(i int64) Value { return Value{Kind: KindInt, Int: i} }

// NewFloat returns a float Value.
func NewFloat(f float64) Value { return Value{Kind: KindFloat, Flt: f} }

// NewString returns a string Value.
func NewString(s string) Value { return Value{Kind: KindString, Str: s} }

// NewBool returns a boolean Value.
func NewBool(b bool) Value {
	if b {
		return True
	}
	return False
}

// IsNull reports whether v is SQL NULL.
func (v Value) IsNull() bool { return v.Kind == KindNull }

// Float returns v as a float64. It is only meaningful for numeric kinds.
func (v Value) Float() float64 {
	switch v.Kind {
	case KindInt:
		return float64(v.Int)
	case KindFloat:
		return v.Flt
	}
	return 0
}

// String renders v the way the REPL and the golden files print it.
func (v Value) String() string {
	switch v.Kind {
	case KindNull:
		return "NULL"
	case KindInt:
		return strconv.FormatInt(v.Int, 10)
	case KindFloat:
		return strconv.FormatFloat(v.Flt, 'f', -1, 64)
	case KindString:
		return v.Str
	case KindBool:
		return strconv.FormatBool(v.Bool)
	}
	return "?"
}

// SQL renders v the way it would be written inside a query, so that EXPLAIN
// output can be copied back into the REPL.
//
// Unlike String, a float always keeps a decimal point: 1.0 displayed as "1"
// would be read back as an integer, and -0.0 as "-0" would be read back as a
// negated integer. The parser fuzz target checks exactly this round trip.
func (v Value) SQL() string {
	switch v.Kind {
	case KindString:
		return "'" + strings.ReplaceAll(v.Str, "'", "''") + "'"
	case KindFloat:
		s := v.String()
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return s
	}
	return v.String()
}

// Row is one tuple flowing through the iterator tree.
type Row []Value

// Clone copies r. Operators that buffer rows (Sort, and the build side of
// HashJoin) must clone, because Scan reuses its output slice between calls.
func (r Row) Clone() Row {
	out := make(Row, len(r))
	copy(out, r)
	return out
}

// TypeError is returned when an operation is applied to incompatible kinds.
// It is a query error rather than a bug, so it is reported to the user.
type TypeError struct {
	Op    string
	Left  Kind
	Right Kind
}

func (e *TypeError) Error() string {
	if e.Right == KindNull {
		return fmt.Sprintf("cannot apply %s to %s", e.Op, e.Left)
	}
	return fmt.Sprintf("cannot apply %s to %s and %s", e.Op, e.Left, e.Right)
}
