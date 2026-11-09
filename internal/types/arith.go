package types

import "errors"

// ErrDivideByZero is returned by "/" and "%" for a zero divisor. Most SQL
// engines return NULL here; pebbledb raises an error so the user notices.
var ErrDivideByZero = errors.New("division by zero")

// Arith applies a binary arithmetic operator, one of "+", "-", "*", "/", "%".
// A NULL operand yields NULL.
//
// Int op Int stays Int except for "/", which always produces a float. That is
// a deliberate deviation from SQL, where 7/2 is 3. See the README.
func Arith(op string, a, b Value) (Value, error) {
	if a.IsNull() || b.IsNull() {
		return Null, nil
	}
	if !a.Kind.IsNumeric() || !b.Kind.IsNumeric() {
		return Null, &TypeError{Op: "operator " + op, Left: a.Kind, Right: b.Kind}
	}
	bothInt := a.Kind == KindInt && b.Kind == KindInt
	switch op {
	case "+":
		if bothInt {
			return NewInt(a.Int + b.Int), nil
		}
		return NewFloat(a.Float() + b.Float()), nil
	case "-":
		if bothInt {
			return NewInt(a.Int - b.Int), nil
		}
		return NewFloat(a.Float() - b.Float()), nil
	case "*":
		if bothInt {
			return NewInt(a.Int * b.Int), nil
		}
		return NewFloat(a.Float() * b.Float()), nil
	case "/":
		if b.Float() == 0 {
			return Null, ErrDivideByZero
		}
		return NewFloat(a.Float() / b.Float()), nil
	case "%":
		if !bothInt {
			return Null, &TypeError{Op: "operator %", Left: a.Kind, Right: b.Kind}
		}
		if b.Int == 0 {
			return Null, ErrDivideByZero
		}
		return NewInt(a.Int % b.Int), nil
	}
	return Null, errors.New("unknown arithmetic operator " + op)
}

// Neg applies unary minus.
func Neg(v Value) (Value, error) {
	switch v.Kind {
	case KindNull:
		return Null, nil
	case KindInt:
		return NewInt(-v.Int), nil
	case KindFloat:
		return NewFloat(-v.Flt), nil
	}
	return Null, &TypeError{Op: "unary -", Left: v.Kind, Right: KindNull}
}
