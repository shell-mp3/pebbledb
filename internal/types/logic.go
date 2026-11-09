package types

// Three-valued logic. A SQL predicate evaluates to TRUE, FALSE or UNKNOWN,
// and only TRUE passes a WHERE clause. UNKNOWN is represented as NULL.

// Truth converts a predicate result to a definite boolean, reporting whether
// the value was a known truth value at all.
func Truth(v Value) (val bool, known bool, err error) {
	switch v.Kind {
	case KindNull:
		return false, false, nil
	case KindBool:
		return v.Bool, true, nil
	}
	return false, false, &TypeError{Op: "boolean context", Left: v.Kind, Right: KindNull}
}

// IsTrue reports whether v passes a filter: TRUE passes, FALSE and UNKNOWN do
// not.
func IsTrue(v Value) (bool, error) {
	val, known, err := Truth(v)
	if err != nil {
		return false, err
	}
	return known && val, nil
}

// And implements SQL AND, including UNKNOWN AND FALSE = FALSE.
func And(a, b Value) (Value, error) {
	av, aok, err := Truth(a)
	if err != nil {
		return Null, err
	}
	bv, bok, err := Truth(b)
	if err != nil {
		return Null, err
	}
	if (aok && !av) || (bok && !bv) {
		return False, nil
	}
	if aok && bok {
		return True, nil
	}
	return Null, nil
}

// Or implements SQL OR, including UNKNOWN OR TRUE = TRUE.
func Or(a, b Value) (Value, error) {
	av, aok, err := Truth(a)
	if err != nil {
		return Null, err
	}
	bv, bok, err := Truth(b)
	if err != nil {
		return Null, err
	}
	if (aok && av) || (bok && bv) {
		return True, nil
	}
	if aok && bok {
		return False, nil
	}
	return Null, nil
}

// Not implements SQL NOT, where NOT UNKNOWN is UNKNOWN.
func Not(a Value) (Value, error) {
	av, aok, err := Truth(a)
	if err != nil {
		return Null, err
	}
	if !aok {
		return Null, nil
	}
	return NewBool(!av), nil
}
