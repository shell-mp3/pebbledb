package eval

import (
	"fmt"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// compileLike builds a LIKE matcher. A literal pattern is translated once at
// compile time; a pattern that depends on the row is matched directly.
func compileLike(n *ast.Binary, left, right *Expr) (*Expr, error) {
	eval := func(row types.Row) (types.Value, error) {
		subject, err := left.Eval(row)
		if err != nil {
			return types.Null, err
		}
		pattern, err := right.Eval(row)
		if err != nil {
			return types.Null, err
		}
		if subject.IsNull() || pattern.IsNull() {
			return types.Null, nil
		}
		if subject.Kind != types.KindString || pattern.Kind != types.KindString {
			return types.Null, fmt.Errorf("LIKE requires strings, got %s and %s", subject.Kind, pattern.Kind)
		}
		return types.NewBool(Like(subject.Str, pattern.Str)), nil
	}
	return &Expr{src: n, kind: types.KindBool, fn: eval}, nil
}

// Like reports whether s matches a SQL LIKE pattern, where % matches any run
// of characters (including none) and _ matches exactly one.
//
// This is a backtracking matcher rather than a translation to regexp: it
// keeps the dependency list empty, and the only backtracking point is a %, so
// the worst case stays linear in the number of % in the pattern.
//
// The comparison is case-sensitive and byte-oriented. _ therefore matches one
// byte, not one rune, which is wrong for multi-byte characters; see the
// README's list of what is not implemented.
func Like(s, pattern string) bool {
	si, pi := 0, 0
	// star and sm record the most recent % and where matching resumed, so a
	// failure can backtrack to it instead of restarting.
	star, sm := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '_' || pattern[pi] == s[si]):
			si++
			pi++
		case pi < len(pattern) && pattern[pi] == '%':
			star = pi
			pi++
			sm = si
		case star >= 0:
			// Let the remembered % swallow one more character.
			pi = star + 1
			sm++
			si = sm
		default:
			return false
		}
	}
	// Trailing % in the pattern can match the empty remainder.
	for pi < len(pattern) && pattern[pi] == '%' {
		pi++
	}
	return pi == len(pattern)
}
