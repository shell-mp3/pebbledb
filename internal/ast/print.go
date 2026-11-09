package ast

import (
	"strconv"
	"strings"
)

// String renders a Select back to canonical SQL. Nothing in the engine needs
// this, but it makes the parser tests readable: an input string and the
// canonical form it should parse to, instead of a nested struct literal.
func (s *Select) String() string {
	var sb strings.Builder
	sb.WriteString("SELECT ")
	for i, item := range s.Items {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(item.String())
	}
	sb.WriteString(" FROM ")
	sb.WriteString(s.From.String())
	for _, j := range s.Joins {
		sb.WriteString(" ")
		sb.WriteString(j.String())
	}
	if s.Where != nil {
		sb.WriteString(" WHERE " + s.Where.String())
	}
	if len(s.GroupBy) > 0 {
		sb.WriteString(" GROUP BY " + exprList(s.GroupBy))
	}
	if s.Having != nil {
		sb.WriteString(" HAVING " + s.Having.String())
	}
	if len(s.OrderBy) > 0 {
		parts := make([]string, len(s.OrderBy))
		for i, k := range s.OrderBy {
			parts[i] = k.Expr.String()
			if k.Desc {
				parts[i] += " DESC"
			}
		}
		sb.WriteString(" ORDER BY " + strings.Join(parts, ", "))
	}
	if s.Limit != nil {
		sb.WriteString(" LIMIT " + strconv.FormatInt(*s.Limit, 10))
	}
	return sb.String()
}

// String renders an Explain statement.
func (e *Explain) String() string {
	if e.Analyze {
		return "EXPLAIN ANALYZE " + e.Stmt.String()
	}
	return "EXPLAIN " + e.Stmt.String()
}

// String renders one select-list entry.
func (i SelectItem) String() string {
	if i.Star {
		if i.StarTable != "" {
			return QuoteIdent(i.StarTable) + ".*"
		}
		return "*"
	}
	out := i.Expr.String()
	if i.Alias != "" {
		out += " AS " + QuoteIdent(i.Alias)
	}
	return out
}

// String renders a table reference with its alias.
func (t *TableRef) String() string {
	if t.Alias != "" {
		return QuoteIdent(t.Name) + " AS " + QuoteIdent(t.Alias)
	}
	return QuoteIdent(t.Name)
}

// String renders a join clause.
func (j Join) String() string {
	kind := "INNER JOIN"
	if j.Type == LeftJoin {
		kind = "LEFT JOIN"
	}
	return kind + " " + j.Table.String() + " ON " + j.On.String()
}

func exprList(exprs []Expr) string {
	parts := make([]string, len(exprs))
	for i, e := range exprs {
		parts[i] = e.String()
	}
	return strings.Join(parts, ", ")
}
