package types

import (
	"errors"
	"testing"
)

func TestValueString(t *testing.T) {
	tests := []struct {
		name string
		val  Value
		want string
		sql  string
	}{
		{"null", Null, "NULL", "NULL"},
		{"int", NewInt(42), "42", "42"},
		{"negative int", NewInt(-7), "-7", "-7"},
		{"float", NewFloat(1.5), "1.5", "1.5"},
		{"integral float keeps no decimal", NewFloat(3), "3", "3.0"},
		{"string", NewString("hi"), "hi", "'hi'"},
		{"string with quote", NewString("it's"), "it's", "'it''s'"},
		{"true", True, "true", "true"},
		{"false", False, "false", "false"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.val.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
			if got := tc.val.SQL(); got != tc.sql {
				t.Errorf("SQL() = %q, want %q", got, tc.sql)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name    string
		a, b    Value
		want    int
		wantErr bool
	}{
		{"int less", NewInt(1), NewInt(2), -1, false},
		{"int equal", NewInt(2), NewInt(2), 0, false},
		{"int greater", NewInt(3), NewInt(2), 1, false},
		{"int vs float promotes", NewInt(2), NewFloat(2.5), -1, false},
		{"float vs int promotes", NewFloat(2.5), NewInt(2), 1, false},
		{"int equals integral float", NewInt(2), NewFloat(2), 0, false},
		{"string lexical", NewString("abc"), NewString("abd"), -1, false},
		{"bool false before true", False, True, -1, false},
		{"string vs int is a type error", NewString("1"), NewInt(1), 0, true},
		{"bool vs int is a type error", True, NewInt(1), 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Compare(tc.a, tc.b)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Compare(%v, %v) succeeded, want a type error", tc.a, tc.b)
				}
				var te *TypeError
				if !errors.As(err, &te) {
					t.Fatalf("error is %T, want *TypeError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Compare: %v", err)
			}
			if got != tc.want {
				t.Errorf("Compare(%v, %v) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestCompareOrderingPutsNullFirstAndNeverFails(t *testing.T) {
	if got := CompareOrdering(Null, NewInt(0)); got != -1 {
		t.Errorf("NULL vs 0 = %d, want -1", got)
	}
	if got := CompareOrdering(NewInt(0), Null); got != 1 {
		t.Errorf("0 vs NULL = %d, want 1", got)
	}
	if got := CompareOrdering(Null, Null); got != 0 {
		t.Errorf("NULL vs NULL = %d, want 0", got)
	}
	// Mixed kinds must still produce a total order rather than a panic.
	a := CompareOrdering(NewString("x"), NewInt(1))
	b := CompareOrdering(NewInt(1), NewString("x"))
	if a == 0 || a != -b {
		t.Errorf("mixed-kind ordering is not antisymmetric: %d vs %d", a, b)
	}
}

func TestArith(t *testing.T) {
	tests := []struct {
		name string
		op   string
		a, b Value
		want Value
	}{
		{"int add stays int", "+", NewInt(2), NewInt(3), NewInt(5)},
		{"int sub stays int", "-", NewInt(2), NewInt(3), NewInt(-1)},
		{"int mul stays int", "*", NewInt(2), NewInt(3), NewInt(6)},
		{"int div becomes float", "/", NewInt(7), NewInt(2), NewFloat(3.5)},
		{"mixed add promotes", "+", NewInt(1), NewFloat(0.5), NewFloat(1.5)},
		{"modulo", "%", NewInt(7), NewInt(2), NewInt(1)},
		{"null propagates", "+", Null, NewInt(1), Null},
		{"null propagates on the right", "*", NewInt(1), Null, Null},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Arith(tc.op, tc.a, tc.b)
			if err != nil {
				t.Fatalf("Arith: %v", err)
			}
			if got != tc.want {
				t.Errorf("%v %s %v = %v, want %v", tc.a, tc.op, tc.b, got, tc.want)
			}
		})
	}
}

func TestArithErrors(t *testing.T) {
	if _, err := Arith("/", NewInt(1), NewInt(0)); !errors.Is(err, ErrDivideByZero) {
		t.Errorf("1/0 error = %v, want ErrDivideByZero", err)
	}
	if _, err := Arith("%", NewInt(1), NewInt(0)); !errors.Is(err, ErrDivideByZero) {
		t.Errorf("1%%0 error = %v, want ErrDivideByZero", err)
	}
	if _, err := Arith("+", NewString("a"), NewInt(1)); err == nil {
		t.Error(`'a' + 1 succeeded, want a type error`)
	}
	if _, err := Arith("%", NewFloat(1), NewFloat(2)); err == nil {
		t.Error("float modulo succeeded, want a type error")
	}
}

// TestThreeValuedLogic pins down the cases that separate SQL's logic from
// Go's: NULL AND FALSE is FALSE, and NULL OR TRUE is TRUE, because the
// unknown operand cannot change the answer.
func TestThreeValuedLogic(t *testing.T) {
	tests := []struct {
		name string
		got  func() (Value, error)
		want Value
	}{
		{"true and true", func() (Value, error) { return And(True, True) }, True},
		{"true and false", func() (Value, error) { return And(True, False) }, False},
		{"null and false", func() (Value, error) { return And(Null, False) }, False},
		{"false and null", func() (Value, error) { return And(False, Null) }, False},
		{"null and true", func() (Value, error) { return And(Null, True) }, Null},
		{"null and null", func() (Value, error) { return And(Null, Null) }, Null},
		{"false or false", func() (Value, error) { return Or(False, False) }, False},
		{"null or true", func() (Value, error) { return Or(Null, True) }, True},
		{"true or null", func() (Value, error) { return Or(True, Null) }, True},
		{"null or false", func() (Value, error) { return Or(Null, False) }, Null},
		{"not true", func() (Value, error) { return Not(True) }, False},
		{"not null", func() (Value, error) { return Not(Null) }, Null},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.got()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("= %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsTrueOnlyAcceptsTrue(t *testing.T) {
	for _, tc := range []struct {
		val  Value
		want bool
	}{{True, true}, {False, false}, {Null, false}} {
		got, err := IsTrue(tc.val)
		if err != nil {
			t.Fatalf("IsTrue(%v): %v", tc.val, err)
		}
		if got != tc.want {
			t.Errorf("IsTrue(%v) = %v, want %v", tc.val, got, tc.want)
		}
	}
	if _, err := IsTrue(NewInt(1)); err == nil {
		t.Error("IsTrue(1) succeeded; integers are not truthy in SQL")
	}
}

func TestSchemaResolve(t *testing.T) {
	s := NewSchema(
		Column{Table: "c", Name: "id", Kind: KindInt},
		Column{Table: "c", Name: "name", Kind: KindString},
		Column{Table: "o", Name: "id", Kind: KindInt},
	)

	if idx, err := s.Resolve("o", "id"); err != nil || idx != 2 {
		t.Errorf(`Resolve("o","id") = %d, %v; want 2, nil`, idx, err)
	}
	if idx, err := s.Resolve("", "name"); err != nil || idx != 1 {
		t.Errorf(`Resolve("","name") = %d, %v; want 1, nil`, idx, err)
	}
	if idx, err := s.Resolve("C", "NAME"); err != nil || idx != 1 {
		t.Errorf("resolution should be case-insensitive, got %d, %v", idx, err)
	}

	var amb *ErrAmbiguousColumn
	if _, err := s.Resolve("", "id"); !errors.As(err, &amb) {
		t.Errorf("unqualified duplicate column gave %v, want *ErrAmbiguousColumn", err)
	}
	var missing *ErrNoSuchColumn
	if _, err := s.Resolve("", "nope"); !errors.As(err, &missing) {
		t.Errorf("unknown column gave %v, want *ErrNoSuchColumn", err)
	}
	if _, err := s.Resolve("x", "id"); !errors.As(err, &missing) {
		t.Errorf("unknown qualifier gave %v, want *ErrNoSuchColumn", err)
	}
}

func TestSchemaTablesAndConcat(t *testing.T) {
	a := NewSchema(Column{Table: "c", Name: "id"}, Column{Table: "c", Name: "name"})
	b := NewSchema(Column{Table: "o", Name: "id"}, Column{Name: "total"})
	joined := a.Concat(b)
	if joined.Len() != 4 {
		t.Fatalf("Concat len = %d, want 4", joined.Len())
	}
	tables := joined.Tables()
	if len(tables) != 2 || tables[0] != "c" || tables[1] != "o" {
		t.Errorf("Tables() = %v, want [c o] (computed columns have no table)", tables)
	}
	if a.Len() != 2 {
		t.Error("Concat must not mutate the receiver")
	}
}

func TestRowCloneIsIndependent(t *testing.T) {
	r := Row{NewInt(1), NewString("a")}
	c := r.Clone()
	r[0] = NewInt(99)
	if c[0] != NewInt(1) {
		t.Error("Clone shares backing storage with the original")
	}
}
