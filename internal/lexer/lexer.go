// Package lexer turns SQL text into a token stream.
//
// It is a hand-written scanner rather than a generated one so that every
// token carries a line and column, which is what makes parse errors point at
// the right character. The lexer never panics: malformed input always comes
// back as an error, which the parser fuzz target relies on.
package lexer

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Type classifies a token.
type Type uint8

const (
	// EOF marks the end of input.
	EOF Type = iota
	// Ident is a bare or double-quoted identifier. Case is preserved; name
	// resolution is case-insensitive.
	Ident
	// Keyword is a reserved word. Val is upper-cased.
	Keyword
	// Int is an integer literal.
	Int
	// Float is a floating point literal.
	Float
	// String is a single-quoted string literal, with quotes removed and ''
	// unescaped.
	String
	// Punct is an operator or punctuation character sequence.
	Punct
)

func (t Type) String() string {
	switch t {
	case EOF:
		return "end of input"
	case Ident:
		return "identifier"
	case Keyword:
		return "keyword"
	case Int:
		return "integer"
	case Float:
		return "float"
	case String:
		return "string"
	case Punct:
		return "operator"
	}
	return "unknown"
}

// Token is one lexical unit.
type Token struct {
	Type Type
	// Val is the normalized text: keywords upper-cased, string literals
	// unquoted, everything else verbatim.
	Val  string
	Line int
	Col  int
}

// String renders the token for error messages.
func (t Token) String() string {
	if t.Type == EOF {
		return "end of input"
	}
	if t.Type == String {
		return fmt.Sprintf("%s %q", t.Type, t.Val)
	}
	return fmt.Sprintf("%s %q", t.Type, t.Val)
}

// Error is a lexical error with a position.
type Error struct {
	Msg  string
	Line int
	Col  int
}

func (e *Error) Error() string {
	return fmt.Sprintf("line %d:%d: %s", e.Line, e.Col, e.Msg)
}

// keywords is the reserved word set. Aggregate names are deliberately absent:
// COUNT and friends are lexed as identifiers and recognised by the parser only
// when followed by "(", so a CSV column named "count" still works.
var keywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "GROUP": true, "BY": true,
	"ORDER": true, "LIMIT": true, "AS": true, "AND": true, "OR": true,
	"NOT": true, "NULL": true, "IS": true, "IN": true, "LIKE": true,
	"JOIN": true, "INNER": true, "LEFT": true, "RIGHT": true, "FULL": true,
	"OUTER": true, "CROSS": true, "ON": true, "ASC": true, "DESC": true,
	"EXPLAIN": true, "ANALYZE": true, "TRUE": true, "FALSE": true,
	"HAVING": true, "DISTINCT": true, "OFFSET": true, "UNION": true,
}

// IsKeyword reports whether s is a reserved word.
func IsKeyword(s string) bool { return keywords[strings.ToUpper(s)] }

// multiCharOps are matched before single characters, longest first.
var multiCharOps = []string{"<=", ">=", "<>", "!="}

// Tokenize scans the whole input. It returns tokens with a trailing EOF, or
// the first lexical error.
func Tokenize(src string) ([]Token, error) {
	l := &lexer{src: src, line: 1, col: 1}
	var out []Token
	for {
		tok, err := l.next()
		if err != nil {
			return nil, err
		}
		out = append(out, tok)
		if tok.Type == EOF {
			return out, nil
		}
	}
}

type lexer struct {
	src  string
	pos  int
	line int
	col  int
}

func (l *lexer) errorf(line, col int, format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...), Line: line, Col: col}
}

// advance consumes n bytes, keeping line and column in sync.
func (l *lexer) advance(n int) {
	for i := 0; i < n && l.pos < len(l.src); i++ {
		if l.src[l.pos] == '\n' {
			l.line++
			l.col = 1
		} else {
			l.col++
		}
		l.pos++
	}
}

func (l *lexer) peek(off int) byte {
	if l.pos+off >= len(l.src) {
		return 0
	}
	return l.src[l.pos+off]
}

// skipSpaceAndComments consumes whitespace, -- line comments and /* */ block
// comments. An unterminated block comment is an error rather than a silent
// truncation.
func (l *lexer) skipSpaceAndComments() error {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f':
			l.advance(1)
		case c == '-' && l.peek(1) == '-':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.advance(1)
			}
		case c == '/' && l.peek(1) == '*':
			startLine, startCol := l.line, l.col
			l.advance(2)
			for {
				if l.pos >= len(l.src) {
					return l.errorf(startLine, startCol, "unterminated block comment")
				}
				if l.src[l.pos] == '*' && l.peek(1) == '/' {
					l.advance(2)
					break
				}
				l.advance(1)
			}
		default:
			return nil
		}
	}
	return nil
}

func (l *lexer) next() (Token, error) {
	if err := l.skipSpaceAndComments(); err != nil {
		return Token{}, err
	}
	line, col := l.line, l.col
	if l.pos >= len(l.src) {
		return Token{Type: EOF, Line: line, Col: col}, nil
	}

	c := l.src[l.pos]
	switch {
	case isIdentStart(c):
		return l.lexBareIdent(line, col), nil
	case c == '"':
		return l.lexQuotedIdent(line, col)
	case c == '\'':
		return l.lexString(line, col)
	case isDigit(c):
		return l.lexNumber(line, col)
	case c == '.' && isDigit(l.peek(1)):
		return l.lexNumber(line, col)
	}

	for _, op := range multiCharOps {
		if strings.HasPrefix(l.src[l.pos:], op) {
			l.advance(len(op))
			return Token{Type: Punct, Val: op, Line: line, Col: col}, nil
		}
	}
	if strings.IndexByte("=<>+-*/%(),.;", c) >= 0 {
		l.advance(1)
		return Token{Type: Punct, Val: string(c), Line: line, Col: col}, nil
	}

	r, size := utf8.DecodeRuneInString(l.src[l.pos:])
	l.advance(size)
	if r == utf8.RuneError && size <= 1 {
		return Token{}, l.errorf(line, col, "invalid UTF-8 byte 0x%02x", c)
	}
	return Token{}, l.errorf(line, col, "unexpected character %q", r)
}

func (l *lexer) lexBareIdent(line, col int) Token {
	start := l.pos
	for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
		l.advance(1)
	}
	word := l.src[start:l.pos]
	if upper := strings.ToUpper(word); keywords[upper] {
		return Token{Type: Keyword, Val: upper, Line: line, Col: col}
	}
	return Token{Type: Ident, Val: word, Line: line, Col: col}
}

// lexQuotedIdent handles "col name", where "" is an escaped quote. A quoted
// identifier is never treated as a keyword.
func (l *lexer) lexQuotedIdent(line, col int) (Token, error) {
	l.advance(1)
	var sb strings.Builder
	for {
		if l.pos >= len(l.src) {
			return Token{}, l.errorf(line, col, "unterminated quoted identifier")
		}
		c := l.src[l.pos]
		if c == '"' {
			if l.peek(1) == '"' {
				sb.WriteByte('"')
				l.advance(2)
				continue
			}
			l.advance(1)
			break
		}
		sb.WriteByte(c)
		l.advance(1)
	}
	if sb.Len() == 0 {
		return Token{}, l.errorf(line, col, "empty quoted identifier")
	}
	return Token{Type: Ident, Val: sb.String(), Line: line, Col: col}, nil
}

func (l *lexer) lexString(line, col int) (Token, error) {
	l.advance(1)
	var sb strings.Builder
	for {
		if l.pos >= len(l.src) {
			return Token{}, l.errorf(line, col, "unterminated string literal")
		}
		c := l.src[l.pos]
		if c == '\'' {
			if l.peek(1) == '\'' {
				sb.WriteByte('\'')
				l.advance(2)
				continue
			}
			l.advance(1)
			break
		}
		sb.WriteByte(c)
		l.advance(1)
	}
	return Token{Type: String, Val: sb.String(), Line: line, Col: col}, nil
}

// lexNumber scans an integer or float, including exponents. An integer too
// large for int64 is reported rather than silently wrapped.
func (l *lexer) lexNumber(line, col int) (Token, error) {
	start := l.pos
	isFloat := false
	for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
		l.advance(1)
	}
	if l.pos < len(l.src) && l.src[l.pos] == '.' {
		isFloat = true
		l.advance(1)
		for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			l.advance(1)
		}
	}
	if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
		save, saveLine, saveCol := l.pos, l.line, l.col
		l.advance(1)
		if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
			l.advance(1)
		}
		if l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			isFloat = true
			for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
				l.advance(1)
			}
		} else {
			// Not an exponent after all, e.g. "1e" or "2eggs".
			l.pos, l.line, l.col = save, saveLine, saveCol
		}
	}
	text := l.src[start:l.pos]

	// "12abc" is a typo, not a number followed by an identifier.
	if l.pos < len(l.src) && isIdentStart(l.src[l.pos]) {
		return Token{}, l.errorf(line, col, "invalid number %q", text+string(l.src[l.pos]))
	}

	if isFloat {
		if _, err := strconv.ParseFloat(text, 64); err != nil {
			return Token{}, l.errorf(line, col, "invalid float %q", text)
		}
		return Token{Type: Float, Val: text, Line: line, Col: col}, nil
	}
	if _, err := strconv.ParseInt(text, 10, 64); err != nil {
		return Token{}, l.errorf(line, col, "integer literal out of range: %q", text)
	}
	return Token{Type: Int, Val: text, Line: line, Col: col}, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isIdentStart accepts ASCII letters and underscore only. Non-ASCII column
// names have to be double-quoted, which keeps the scanner byte-oriented.
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }
