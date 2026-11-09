// Package physical is the executable plan: a tree of volcano-style
// iterators, plus the EXPLAIN printer that renders it.
//
// Every operator implements the same three-call protocol. Open prepares,
// Next pulls one row, Close releases. A query runs by calling Next on the
// root until it says there are no more rows, and each operator pulls from its
// children on demand, so a LIMIT 3 over a 30-row file reads only as far as it
// has to.
//
// Rows are borrowed, not owned: the row Next returns is only valid until the
// next call. Operators that buffer rows (Sort, the build side of HashJoin,
// HashAggregate's group keys) clone them.
package physical

import (
	"fmt"
	"strings"
	"time"

	"github.com/shell-mp3/pebbledb/internal/types"
)

// Operator is one node of the executable plan.
type Operator interface {
	// Open prepares the operator and its children. Operators that need all
	// their input before producing anything (Sort, HashAggregate, the build
	// side of HashJoin) do that work here.
	Open() error
	// Next returns the next row. The second result is false at end of input.
	// The returned row is only valid until the next call.
	Next() (types.Row, bool, error)
	// Close releases resources. It is safe to call without Open, and safe to
	// call twice, so a failed query can always be torn down.
	Close() error

	// Schema is the operator's output shape.
	Schema() *types.Schema
	// Label is the one-line description EXPLAIN prints.
	Label() string
	// Children returns the inputs, in the order EXPLAIN should list them.
	Children() []Operator
	// ChildLabels optionally names each child, which is how a hash join
	// shows which side it builds its table from. A nil result means no names.
	ChildLabels() []string
	// Stats reports what the operator did, for EXPLAIN ANALYZE.
	Stats() Stats
}

// Stats is the per-operator measurement EXPLAIN ANALYZE reports. Duration is
// inclusive of children, because an operator's time is measured around the
// calls it makes into them.
type Stats struct {
	Rows     int64
	Duration time.Duration
}

// stats is embedded by every operator to supply Stats and to wrap the row
// loop, so the bookkeeping is written once rather than at every return.
type stats struct {
	rows int64
	dur  time.Duration
}

// Stats reports the counters.
func (s *stats) Stats() Stats { return Stats{Rows: s.rows, Duration: s.dur} }

// measure times one Next call and counts the row it produced.
func (s *stats) measure(next func() (types.Row, bool, error)) (types.Row, bool, error) {
	start := time.Now()
	row, ok, err := next()
	s.dur += time.Since(start)
	if ok {
		s.rows++
	}
	return row, ok, err
}

// measureOpen times an Open call, which is where the blocking operators do
// most of their work.
func (s *stats) measureOpen(open func() error) error {
	start := time.Now()
	err := open()
	s.dur += time.Since(start)
	return err
}

// Explain renders the plan as a tree. With analyze set, each line is
// annotated with the rows the operator produced and the time it took, which
// is only meaningful after the query has actually been run.
func Explain(root Operator, analyze bool) string {
	var sb strings.Builder
	explain(&sb, root, "", "", true, true, analyze)
	return strings.TrimRight(sb.String(), "\n")
}

func explain(sb *strings.Builder, op Operator, prefix, label string, last, root, analyze bool) {
	line := prefix
	if !root {
		if last {
			line += "└─ "
			prefix += "   "
		} else {
			line += "├─ "
			prefix += "│  "
		}
	}
	line += label + op.Label()
	if analyze {
		s := op.Stats()
		line += fmt.Sprintf("  (rows=%d, %s)", s.Rows, formatDuration(s.Duration))
	}
	sb.WriteString(line + "\n")

	kids := op.Children()
	labels := op.ChildLabels()
	for i, kid := range kids {
		kidLabel := ""
		if i < len(labels) && labels[i] != "" {
			kidLabel = labels[i] + ": "
		}
		explain(sb, kid, prefix, kidLabel, i == len(kids)-1, false, analyze)
	}
}

// formatDuration keeps EXPLAIN ANALYZE output to a readable width.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fus", float64(d.Nanoseconds())/1e3)
	case d < time.Second:
		return fmt.Sprintf("%.2fms", float64(d.Nanoseconds())/1e6)
	}
	return fmt.Sprintf("%.3fs", d.Seconds())
}

// closeAll closes every operator and returns the first error, so a partial
// teardown still releases the rest of the file handles.
func closeAll(ops ...Operator) error {
	var first error
	for _, op := range ops {
		if op == nil {
			continue
		}
		if err := op.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
