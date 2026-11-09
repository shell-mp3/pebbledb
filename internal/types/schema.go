package types

import (
	"fmt"
	"strings"
)

// Column is one output column of a plan node. Table is the table name or
// alias the column came from, and is empty for computed columns.
type Column struct {
	Table string
	Name  string
	Kind  Kind
}

// Qualified renders the column as it would be written in a query.
func (c Column) Qualified() string {
	if c.Table == "" {
		return c.Name
	}
	return c.Table + "." + c.Name
}

// Schema is the ordered list of columns a plan node produces.
type Schema struct {
	Columns []Column
}

// NewSchema builds a schema from columns.
func NewSchema(cols ...Column) *Schema { return &Schema{Columns: cols} }

// Len returns the number of columns.
func (s *Schema) Len() int { return len(s.Columns) }

// Concat returns the columns of s followed by those of other, which is the
// output schema of a join.
func (s *Schema) Concat(other *Schema) *Schema {
	cols := make([]Column, 0, len(s.Columns)+len(other.Columns))
	cols = append(cols, s.Columns...)
	cols = append(cols, other.Columns...)
	return &Schema{Columns: cols}
}

// ErrNoSuchColumn and ErrAmbiguousColumn are the two ways name resolution can
// fail. They are distinct types so the optimizer can tell "this plan is
// invalid" from "I cannot prove this rewrite is safe".
type ErrNoSuchColumn struct{ Ref string }

func (e *ErrNoSuchColumn) Error() string { return fmt.Sprintf("no such column: %s", e.Ref) }

// ErrAmbiguousColumn is returned when an unqualified name matches more than
// one column, which happens after a join of tables with overlapping headers.
type ErrAmbiguousColumn struct {
	Ref     string
	Matches []string
}

func (e *ErrAmbiguousColumn) Error() string {
	return fmt.Sprintf("ambiguous column: %s (matches %s)", e.Ref, strings.Join(e.Matches, ", "))
}

// Resolve finds the index of the column referenced by table.name. An empty
// table matches any table, and both parts compare case-insensitively.
func (s *Schema) Resolve(table, name string) (int, error) {
	found := -1
	var matches []string
	for i, c := range s.Columns {
		if !strings.EqualFold(c.Name, name) {
			continue
		}
		if table != "" && !strings.EqualFold(c.Table, table) {
			continue
		}
		matches = append(matches, c.Qualified())
		if found < 0 {
			found = i
		}
	}
	switch {
	case len(matches) == 0:
		return -1, &ErrNoSuchColumn{Ref: Ref(table, name)}
	case len(matches) > 1:
		return -1, &ErrAmbiguousColumn{Ref: Ref(table, name), Matches: matches}
	}
	return found, nil
}

// Tables returns the distinct table names in the schema, in first-seen order.
func (s *Schema) Tables() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range s.Columns {
		if c.Table == "" || seen[strings.ToLower(c.Table)] {
			continue
		}
		seen[strings.ToLower(c.Table)] = true
		out = append(out, c.Table)
	}
	return out
}

// String renders the schema for error messages and for .schema output.
func (s *Schema) String() string {
	parts := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		parts[i] = c.Qualified() + " " + c.Kind.String()
	}
	return strings.Join(parts, ", ")
}

// Ref renders a possibly qualified column reference.
func Ref(table, name string) string {
	if table == "" {
		return name
	}
	return table + "." + name
}
