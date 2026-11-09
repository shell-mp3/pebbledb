package types

import "strings"

// Compare orders a and b, returning -1, 0 or 1. Neither argument may be NULL:
// callers handle NULL before reaching here, because a SQL comparison
// involving NULL is UNKNOWN rather than ordered.
//
// Ints and floats compare numerically across kinds, so `amount > 500` works
// whether amount was inferred as int or float. Any other mix is a type error.
func Compare(a, b Value) (int, error) {
	switch {
	case a.Kind.IsNumeric() && b.Kind.IsNumeric():
		if a.Kind == KindInt && b.Kind == KindInt {
			return cmpInt(a.Int, b.Int), nil
		}
		return cmpFloat(a.Float(), b.Float()), nil
	case a.Kind == KindString && b.Kind == KindString:
		return strings.Compare(a.Str, b.Str), nil
	case a.Kind == KindBool && b.Kind == KindBool:
		return cmpInt(b2i(a.Bool), b2i(b.Bool)), nil
	}
	return 0, &TypeError{Op: "comparison", Left: a.Kind, Right: b.Kind}
}

// CompareOrdering is the total order used by Sort and by MIN/MAX. Unlike
// Compare it never fails: NULL sorts before every value, and values of
// different kinds fall back to kind order, so a mixed column still has a
// deterministic ordering instead of aborting the query.
func CompareOrdering(a, b Value) int {
	if a.IsNull() || b.IsNull() {
		switch {
		case a.IsNull() && b.IsNull():
			return 0
		case a.IsNull():
			return -1
		default:
			return 1
		}
	}
	if c, err := Compare(a, b); err == nil {
		return c
	}
	return cmpInt(int64(a.Kind), int64(b.Kind))
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
