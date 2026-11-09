package lexer

import (
	"strings"
	"testing"
)

// tokenString renders a token stream compactly so a whole expectation fits on
// one line of the test table.
func tokenString(toks []Token) string {
	var parts []string
	for _, t := range toks {
		if t.Type == EOF {
			break
		}
		parts = append(parts, t.Type.String()[:3]+":"+t.Val)
	}
	return strings.Join(parts, " ")
}

func TestTokenize(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "keywords are upper-cased, identifiers keep their case",
			src:  "select Name from Customers",
			want: "key:SELECT ide:Name key:FROM ide:Customers",
		},
		{
			name: "qualified column",
			src:  "c.id",
			want: "ide:c ope:. ide:id",
		},
		{
			name: "comparison operators",
			src:  "a <= b and c <> d or e != f",
			want: "ide:a ope:<= ide:b key:AND ide:c ope:<> ide:d key:OR ide:e ope:!= ide:f",
		},
		{
			name: "numbers",
			src:  "1 2.5 .75 1e3 1.5e-2",
			want: "int:1 flo:2.5 flo:.75 flo:1e3 flo:1.5e-2",
		},
		{
			name: "string literal with an escaped quote",
			src:  "'it''s'",
			want: "str:it's",
		},
		{
			name: "empty string literal",
			src:  "''",
			want: "str:",
		},
		{
			name: "quoted identifier is never a keyword",
			src:  `"select" "a""b"`,
			want: `ide:select ide:a"b`,
		},
		{
			name: "line comment runs to end of line",
			src:  "select -- everything\n1",
			want: "key:SELECT int:1",
		},
		{
			name: "block comment",
			src:  "select /* a\nb */ 1",
			want: "key:SELECT int:1",
		},
		{
			name: "aggregate names lex as identifiers so they can be column names",
			src:  "count sum avg min max",
			want: "ide:count ide:sum ide:avg ide:min ide:max",
		},
		{
			name: "star and parens",
			src:  "count(*)",
			want: "ide:count ope:( ope:* ope:)",
		},
		{
			name: "empty input is just EOF",
			src:  "   \n\t ",
			want: "",
		},
		{
			name: "semicolon",
			src:  "1;",
			want: "int:1 ope:;",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			toks, err := Tokenize(tc.src)
			if err != nil {
				t.Fatalf("Tokenize(%q): %v", tc.src, err)
			}
			if got := tokenString(toks); got != tc.want {
				t.Errorf("Tokenize(%q)\n got: %s\nwant: %s", tc.src, got, tc.want)
			}
			if toks[len(toks)-1].Type != EOF {
				t.Error("token stream does not end with EOF")
			}
		})
	}
}

func TestTokenizeErrors(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantMsg string
	}{
		{"unterminated string", "'abc", "unterminated string literal"},
		{"unterminated quoted identifier", `"abc`, "unterminated quoted identifier"},
		{"empty quoted identifier", `""`, "empty quoted identifier"},
		{"unterminated block comment", "/* abc", "unterminated block comment"},
		{"unexpected character", "a # b", "unexpected character"},
		{"number followed by letters", "12abc", "invalid number"},
		{"integer overflow", "99999999999999999999", "out of range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Tokenize(tc.src)
			if err == nil {
				t.Fatalf("Tokenize(%q) succeeded, want an error", tc.src)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantMsg)
			}
			var le *Error
			if !asLexError(err, &le) {
				t.Fatalf("error type is %T, want *lexer.Error", err)
			}
			if le.Line < 1 || le.Col < 1 {
				t.Errorf("error has no usable position: line %d col %d", le.Line, le.Col)
			}
		})
	}
}

func asLexError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

// TestPositions checks that line and column survive newlines, since every
// parse error message is built from them.
func TestPositions(t *testing.T) {
	toks, err := Tokenize("select\n  a,\n  b")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	want := []struct {
		val  string
		line int
		col  int
	}{
		{"SELECT", 1, 1},
		{"a", 2, 3},
		{",", 2, 4},
		{"b", 3, 3},
	}
	for i, w := range want {
		got := toks[i]
		if got.Val != w.val || got.Line != w.line || got.Col != w.col {
			t.Errorf("token %d = %q at %d:%d, want %q at %d:%d", i, got.Val, got.Line, got.Col, w.val, w.line, w.col)
		}
	}
}

// TestNotAnExponent covers the backtracking in lexNumber: "1e" is not a valid
// exponent, so the "e" must be handed back and lexed separately.
func TestNotAnExponent(t *testing.T) {
	if _, err := Tokenize("1e"); err == nil {
		t.Error(`Tokenize("1e") succeeded; "1e" is a malformed number, not 1 followed by e`)
	}
	toks, err := Tokenize("1 e")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	if got := tokenString(toks); got != "int:1 ide:e" {
		t.Errorf("got %s, want int:1 ide:e", got)
	}
}

func TestIsKeyword(t *testing.T) {
	for _, w := range []string{"select", "SELECT", "Where", "join"} {
		if !IsKeyword(w) {
			t.Errorf("IsKeyword(%q) = false, want true", w)
		}
	}
	for _, w := range []string{"count", "sum", "customers", ""} {
		if IsKeyword(w) {
			t.Errorf("IsKeyword(%q) = true, want false", w)
		}
	}
}
