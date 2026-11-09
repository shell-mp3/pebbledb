// Package parser is a recursive-descent parser over the lexer's token slice.
//
// Expressions use precedence climbing, one method per precedence level, so the
// grammar can be read straight off the call chain:
//
//	or -> and -> not -> comparison -> additive -> multiplicative -> unary -> primary
//
// Unsupported SQL is rejected with a message that names the feature rather
// than a generic syntax error, because "OFFSET is not supported" is a much
// better experience than "unexpected keyword".
package parser

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/shell-mp3/pebbledb/internal/ast"
	"github.com/shell-mp3/pebbledb/internal/lexer"
	"github.com/shell-mp3/pebbledb/internal/types"
)

// Error is a parse error with the position of the offending token.
type Error struct {
	Msg  string
	Line int
	Col  int
}

func (e *Error) Error() string {
	return fmt.Sprintf("line %d:%d: %s", e.Line, e.Col, e.Msg)
}

// Parse parses a single statement, which must be the whole input apart from an
// optional trailing semicolon.
func Parse(src string) (ast.Statement, error) {
	toks, err := lexer.Tokenize(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	stmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	p.accept(lexer.Punct, ";")
	if p.cur().Type != lexer.EOF {
		return nil, p.errf(p.cur(), "unexpected %s after end of statement", p.cur())
	}
	return stmt, nil
}

type parser struct {
	toks []lexer.Token
	pos  int
	// aggDepth is non-zero while parsing an aggregate's argument, which is how
	// nested aggregates are rejected.
	aggDepth int
}

func (p *parser) cur() lexer.Token { return p.toks[p.pos] }

func (p *parser) peek(off int) lexer.Token {
	if p.pos+off >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.pos+off]
}

func (p *parser) errf(tok lexer.Token, format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...), Line: tok.Line, Col: tok.Col}
}

// accept consumes the current token if it matches, reporting whether it did.
// An empty val matches any value of that type.
func (p *parser) accept(t lexer.Type, val string) bool {
	tok := p.cur()
	if tok.Type != t || (val != "" && tok.Val != val) {
		return false
	}
	p.pos++
	return true
}

func (p *parser) at(t lexer.Type, val string) bool {
	tok := p.cur()
	return tok.Type == t && (val == "" || tok.Val == val)
}

// atAnyKeyword reports whether the current token is one of the given keywords.
func (p *parser) atAnyKeyword(words ...string) bool {
	tok := p.cur()
	if tok.Type != lexer.Keyword {
		return false
	}
	for _, w := range words {
		if tok.Val == w {
			return true
		}
	}
	return false
}

func (p *parser) expectKeyword(word string) error {
	if p.accept(lexer.Keyword, word) {
		return nil
	}
	return p.errf(p.cur(), "expected %s, found %s", word, p.cur())
}

func (p *parser) expectPunct(val string) error {
	if p.accept(lexer.Punct, val) {
		return nil
	}
	return p.errf(p.cur(), "expected %q, found %s", val, p.cur())
}

// expectIdent accepts an identifier. A reserved word in an identifier position
// gets a hint about double quoting, since CSV headers are not under the user's
// control.
func (p *parser) expectIdent(what string) (string, error) {
	tok := p.cur()
	if tok.Type == lexer.Ident {
		p.pos++
		return tok.Val, nil
	}
	if tok.Type == lexer.Keyword {
		return "", p.errf(tok, "expected %s, found reserved word %s (quote it as %q to use it as a name)", what, tok.Val, strings.ToLower(tok.Val))
	}
	return "", p.errf(tok, "expected %s, found %s", what, tok)
}

func (p *parser) parseStatement() (ast.Statement, error) {
	if p.cur().Type == lexer.EOF {
		return nil, p.errf(p.cur(), "empty statement")
	}
	if p.accept(lexer.Keyword, "EXPLAIN") {
		analyze := p.accept(lexer.Keyword, "ANALYZE")
		sel, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		return &ast.Explain{Analyze: analyze, Stmt: sel}, nil
	}
	return p.parseSelect()
}

func (p *parser) parseSelect() (*ast.Select, error) {
	start := p.cur()
	if err := p.expectKeyword("SELECT"); err != nil {
		if start.Type == lexer.Ident || start.Type == lexer.Keyword {
			return nil, p.errf(start, "only SELECT and EXPLAIN are supported, found %s", start)
		}
		return nil, err
	}
	if p.at(lexer.Keyword, "DISTINCT") {
		return nil, p.errf(p.cur(), "SELECT DISTINCT is not supported")
	}

	sel := &ast.Select{}
	items, err := p.parseSelectList()
	if err != nil {
		return nil, err
	}
	sel.Items = items

	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	if sel.From, err = p.parseTableRef(); err != nil {
		return nil, err
	}
	if sel.Joins, err = p.parseJoins(); err != nil {
		return nil, err
	}

	if p.accept(lexer.Keyword, "WHERE") {
		if sel.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
		if aggs := ast.Aggregates(sel.Where); len(aggs) > 0 {
			return nil, p.errf(p.cur(), "aggregate %s is not allowed in WHERE (use HAVING)", aggs[0])
		}
	}
	if p.accept(lexer.Keyword, "GROUP") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if aggs := ast.Aggregates(e); len(aggs) > 0 {
				return nil, p.errf(p.cur(), "aggregate %s is not allowed in GROUP BY", aggs[0])
			}
			sel.GroupBy = append(sel.GroupBy, e)
			if !p.accept(lexer.Punct, ",") {
				break
			}
		}
	}
	if p.accept(lexer.Keyword, "HAVING") {
		if sel.Having, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	if p.accept(lexer.Keyword, "ORDER") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		for {
			key := ast.OrderBy{}
			if key.Expr, err = p.parseExpr(); err != nil {
				return nil, err
			}
			switch {
			case p.accept(lexer.Keyword, "DESC"):
				key.Desc = true
			case p.accept(lexer.Keyword, "ASC"):
			}
			sel.OrderBy = append(sel.OrderBy, key)
			if !p.accept(lexer.Punct, ",") {
				break
			}
		}
	}
	if p.accept(lexer.Keyword, "LIMIT") {
		tok := p.cur()
		if !p.accept(lexer.Int, "") {
			return nil, p.errf(tok, "expected an integer after LIMIT, found %s", tok)
		}
		n, err := strconv.ParseInt(tok.Val, 10, 64)
		if err != nil {
			return nil, p.errf(tok, "invalid LIMIT value %q", tok.Val)
		}
		sel.Limit = &n
	}
	if p.at(lexer.Keyword, "OFFSET") {
		return nil, p.errf(p.cur(), "OFFSET is not supported")
	}
	if p.at(lexer.Keyword, "UNION") {
		return nil, p.errf(p.cur(), "UNION is not supported")
	}
	return sel, nil
}

func (p *parser) parseSelectList() ([]ast.SelectItem, error) {
	var items []ast.SelectItem
	for {
		item, err := p.parseSelectItem()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		if !p.accept(lexer.Punct, ",") {
			return items, nil
		}
	}
}

func (p *parser) parseSelectItem() (ast.SelectItem, error) {
	// "*" and "table.*" are structural, not expressions.
	if p.accept(lexer.Punct, "*") {
		return ast.SelectItem{Star: true}, nil
	}
	if p.cur().Type == lexer.Ident && p.peek(1).Type == lexer.Punct && p.peek(1).Val == "." &&
		p.peek(2).Type == lexer.Punct && p.peek(2).Val == "*" {
		table := p.cur().Val
		p.pos += 3
		return ast.SelectItem{Star: true, StarTable: table}, nil
	}

	e, err := p.parseExpr()
	if err != nil {
		return ast.SelectItem{}, err
	}
	item := ast.SelectItem{Expr: e}
	if item.Alias, err = p.parseOptionalAlias(); err != nil {
		return ast.SelectItem{}, err
	}
	return item, nil
}

// parseOptionalAlias handles both "expr AS name" and the bare "expr name"
// form. Reserved words are safe here: they lex as Keyword, not Ident, so
// "SELECT name FROM t" never mistakes FROM for an alias.
func (p *parser) parseOptionalAlias() (string, error) {
	if p.accept(lexer.Keyword, "AS") {
		return p.expectIdent("an alias after AS")
	}
	if p.cur().Type == lexer.Ident {
		name := p.cur().Val
		p.pos++
		return name, nil
	}
	return "", nil
}

func (p *parser) parseTableRef() (*ast.TableRef, error) {
	name, err := p.expectIdent("a table name")
	if err != nil {
		return nil, err
	}
	ref := &ast.TableRef{Name: name}
	if ref.Alias, err = p.parseOptionalAlias(); err != nil {
		return nil, err
	}
	return ref, nil
}

func (p *parser) parseJoins() ([]ast.Join, error) {
	var joins []ast.Join
	for {
		var jt ast.JoinType
		switch {
		case p.atAnyKeyword("RIGHT", "FULL"):
			return nil, p.errf(p.cur(), "%s JOIN is not supported (only INNER and LEFT)", p.cur().Val)
		case p.at(lexer.Keyword, "CROSS"):
			return nil, p.errf(p.cur(), "CROSS JOIN is not supported")
		case p.accept(lexer.Keyword, "INNER"):
			jt = ast.InnerJoin
			if err := p.expectKeyword("JOIN"); err != nil {
				return nil, err
			}
		case p.accept(lexer.Keyword, "LEFT"):
			jt = ast.LeftJoin
			p.accept(lexer.Keyword, "OUTER")
			if err := p.expectKeyword("JOIN"); err != nil {
				return nil, err
			}
		case p.accept(lexer.Keyword, "JOIN"):
			jt = ast.InnerJoin
		default:
			return joins, nil
		}

		table, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		if !p.accept(lexer.Keyword, "ON") {
			return nil, p.errf(p.cur(), "expected ON after JOIN %s (there is no cross join)", table.Name)
		}
		on, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if aggs := ast.Aggregates(on); len(aggs) > 0 {
			return nil, p.errf(p.cur(), "aggregate %s is not allowed in a join condition", aggs[0])
		}
		joins = append(joins, ast.Join{Type: jt, Table: table, On: on})
	}
}

// Expressions, lowest precedence first.

func (p *parser) parseExpr() (ast.Expr, error) { return p.parseOr() }

func (p *parser) parseOr() (ast.Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.accept(lexer.Keyword, "OR") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &ast.Binary{Op: "OR", Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (ast.Expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.accept(lexer.Keyword, "AND") {
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &ast.Binary{Op: "AND", Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseNot() (ast.Expr, error) {
	if p.accept(lexer.Keyword, "NOT") {
		inner, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &ast.Unary{Op: "NOT", Expr: inner}, nil
	}
	return p.parseComparison()
}

var comparisonOps = map[string]bool{"=": true, "!=": true, "<>": true, "<": true, "<=": true, ">": true, ">=": true}

func (p *parser) parseComparison() (ast.Expr, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.cur().Type == lexer.Punct && comparisonOps[p.cur().Val]:
			op := p.cur().Val
			if op == "!=" {
				op = "<>"
			}
			p.pos++
			right, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			left = &ast.Binary{Op: op, Left: left, Right: right}

		case p.accept(lexer.Keyword, "IS"):
			negate := p.accept(lexer.Keyword, "NOT")
			if err := p.expectKeyword("NULL"); err != nil {
				return nil, p.errf(p.cur(), "only IS NULL and IS NOT NULL are supported, found %s", p.cur())
			}
			left = &ast.IsNull{Expr: left, Negate: negate}

		case p.at(lexer.Keyword, "LIKE"):
			p.pos++
			if left, err = p.parseLike(left, false); err != nil {
				return nil, err
			}

		case p.at(lexer.Keyword, "IN"):
			p.pos++
			if left, err = p.parseIn(left, false); err != nil {
				return nil, err
			}

		case p.at(lexer.Keyword, "NOT") && (p.peek(1).Val == "LIKE" || p.peek(1).Val == "IN") && p.peek(1).Type == lexer.Keyword:
			kind := p.peek(1).Val
			p.pos += 2
			if kind == "LIKE" {
				left, err = p.parseLike(left, true)
			} else {
				left, err = p.parseIn(left, true)
			}
			if err != nil {
				return nil, err
			}

		default:
			return left, nil
		}
	}
}

func (p *parser) parseLike(left ast.Expr, negate bool) (ast.Expr, error) {
	right, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	var out ast.Expr = &ast.Binary{Op: "LIKE", Left: left, Right: right}
	if negate {
		out = &ast.Unary{Op: "NOT", Expr: out}
	}
	return out, nil
}

func (p *parser) parseIn(left ast.Expr, negate bool) (ast.Expr, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	if p.at(lexer.Keyword, "SELECT") {
		return nil, p.errf(p.cur(), "subqueries are not supported; IN takes a list of values")
	}
	var list []ast.Expr
	for {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		list = append(list, e)
		if !p.accept(lexer.Punct, ",") {
			break
		}
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return &ast.In{Expr: left, List: list, Negate: negate}, nil
}

func (p *parser) parseAdditive() (ast.Expr, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for p.cur().Type == lexer.Punct && (p.cur().Val == "+" || p.cur().Val == "-") {
		op := p.cur().Val
		p.pos++
		right, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		left = &ast.Binary{Op: op, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseMultiplicative() (ast.Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.cur().Type == lexer.Punct && (p.cur().Val == "*" || p.cur().Val == "/" || p.cur().Val == "%") {
		op := p.cur().Val
		p.pos++
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &ast.Binary{Op: op, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseUnary() (ast.Expr, error) {
	if p.at(lexer.Punct, "-") {
		p.pos++
		inner, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		// Fold the sign into the literal so EXPLAIN prints -1 rather than (-1).
		if lit, ok := inner.(*ast.Literal); ok {
			if neg, err := types.Neg(lit.Value); err == nil {
				return &ast.Literal{Value: neg}, nil
			}
		}
		return &ast.Unary{Op: "-", Expr: inner}, nil
	}
	if p.at(lexer.Punct, "+") {
		p.pos++
		return p.parseUnary()
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (ast.Expr, error) {
	tok := p.cur()
	switch tok.Type {
	case lexer.Int:
		p.pos++
		n, err := strconv.ParseInt(tok.Val, 10, 64)
		if err != nil {
			return nil, p.errf(tok, "invalid integer %q", tok.Val)
		}
		return &ast.Literal{Value: types.NewInt(n)}, nil

	case lexer.Float:
		p.pos++
		f, err := strconv.ParseFloat(tok.Val, 64)
		if err != nil {
			return nil, p.errf(tok, "invalid float %q", tok.Val)
		}
		return &ast.Literal{Value: types.NewFloat(f)}, nil

	case lexer.String:
		p.pos++
		return &ast.Literal{Value: types.NewString(tok.Val)}, nil

	case lexer.Keyword:
		switch tok.Val {
		case "NULL":
			p.pos++
			return &ast.Literal{Value: types.Null}, nil
		case "TRUE":
			p.pos++
			return &ast.Literal{Value: types.True}, nil
		case "FALSE":
			p.pos++
			return &ast.Literal{Value: types.False}, nil
		}
		return nil, p.errf(tok, "unexpected keyword %s in expression", tok.Val)

	case lexer.Punct:
		if tok.Val == "(" {
			p.pos++
			inner, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			return inner, nil
		}
		if tok.Val == "*" {
			return nil, p.errf(tok, "* is only allowed in the select list and in COUNT(*)")
		}
		return nil, p.errf(tok, "unexpected %s in expression", tok)

	case lexer.Ident:
		// A name followed by "(" is a function call.
		if p.peek(1).Type == lexer.Punct && p.peek(1).Val == "(" {
			return p.parseFuncCall()
		}
		p.pos++
		if p.accept(lexer.Punct, ".") {
			name, err := p.expectIdent("a column name after " + tok.Val + ".")
			if err != nil {
				return nil, err
			}
			return &ast.ColumnRef{Table: tok.Val, Name: name}, nil
		}
		return &ast.ColumnRef{Name: tok.Val}, nil
	}
	return nil, p.errf(tok, "unexpected %s in expression", tok)
}

func (p *parser) parseFuncCall() (ast.Expr, error) {
	nameTok := p.cur()
	fn, ok := ast.LookupAgg(nameTok.Val)
	if !ok {
		return nil, p.errf(nameTok, "unknown function %s (pebbledb has COUNT, SUM, AVG, MIN and MAX)", nameTok.Val)
	}
	if p.aggDepth > 0 {
		return nil, p.errf(nameTok, "aggregate functions cannot be nested")
	}
	p.pos += 2 // name and "("

	if p.at(lexer.Keyword, "DISTINCT") {
		return nil, p.errf(p.cur(), "%s(DISTINCT ...) is not supported", fn)
	}
	if p.at(lexer.Punct, "*") {
		if fn != ast.AggCount {
			return nil, p.errf(p.cur(), "%s(*) is not valid; only COUNT(*) is", fn)
		}
		p.pos++
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return &ast.Aggregate{Func: fn, Star: true}, nil
	}

	p.aggDepth++
	arg, err := p.parseExpr()
	p.aggDepth--
	if err != nil {
		return nil, err
	}
	if p.at(lexer.Punct, ",") {
		return nil, p.errf(p.cur(), "%s takes exactly one argument", fn)
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return &ast.Aggregate{Func: fn, Arg: arg}, nil
}
