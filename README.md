# pebbledb

A toy SQL engine in Go that runs real queries over CSV files, and shows you the plan it chose.

```
$ pebbledb -c "EXPLAIN SELECT c.name, SUM(o.amount) AS total
                FROM customers c JOIN orders o ON c.id = o.customer_id
                WHERE c.country = 'CA' AND o.amount > 20
                GROUP BY c.name ORDER BY total DESC LIMIT 3" data/

Limit(3)
└─ Project(c.name, "SUM(o.amount)" AS total)
   └─ Sort("SUM(o.amount)" DESC)
      └─ HashAggregate(group by c.name, SUM(o.amount))
         └─ HashJoin(inner, on c.id = o.customer_id)
            ├─ probe: Filter(c.country = 'CA')
            │  └─ Scan(customers AS c) reads 3 of 5 columns [id, name, country], 12 rows
            └─ build: Filter(o.amount > 20)
               └─ Scan(orders AS o) reads 2 of 6 columns [customer_id, amount], 30 rows

rules applied: predicate pushdown, projection pruning
```

That is the whole point of the project. Most of us write SQL every day and never see the layer
underneath it, so pebbledb implements that layer end to end: a lexer, a recursive-descent parser,
a logical plan, two optimizer rules, and a tree of volcano-style iterators that pull rows one at a
time. `EXPLAIN` prints what came out, and `EXPLAIN ANALYZE` prints how many rows each operator
actually produced.

## Run it

```bash
git clone https://github.com/shell-mp3/pebbledb && cd pebbledb
```

```bash
go run ./cmd/pebbledb data/
```

```
pebbledb> SELECT c.name, SUM(o.amount) AS total
     ...> FROM customers c JOIN orders o ON c.id = o.customer_id
     ...> WHERE c.country = 'CA' AND o.amount > 20
     ...> GROUP BY c.name ORDER BY total DESC LIMIT 3;
name       | total
-----------+------
Lena Berg  |   214
Ada Okafor |   130
Chen Wei   |  94.5
(3 rows)
```

Needs Go 1.24 or newer, and `go build -o pebbledb ./cmd/pebbledb` gives you the binary the
examples here call `pebbledb`. One statement at a time without the REPL:

```bash
go run ./cmd/pebbledb -c "SELECT COUNT(*) FROM orders" data/
```

Each CSV file becomes a table named after it, so `data/orders.csv` is the table `orders`. Inside
the REPL, `.help` lists the meta-commands: `.tables`, `.schema`, `.timer on`, and `.optimize off`,
which is how you get the before and after plans below.

## Why I built this

I wanted to know what actually happens between typing a query and getting rows back, and the only
way I know to find that out is to write it. Reading about predicate pushdown is not the same as
discovering, from a query that returned the wrong rows, that the rule is unsafe on the inner side
of a left join, and then working out why.

CSV is the input because it strips away everything that is not the query engine. There are no
pages, no B-trees and no buffer pool, so what is left is parsing, planning and iteration, which is
the part I wanted to understand.

## The plan changes: before and after

Here is the same query planned twice. First with the optimizer off (`.optimize off`, or
`-no-optimize`):

```
Limit(3)
└─ Project(c.name, "SUM(o.amount)" AS total)
   └─ Sort("SUM(o.amount)" DESC)
      └─ HashAggregate(group by c.name, SUM(o.amount))
         └─ Filter(c.country = 'CA' AND o.amount > 20)
            └─ HashJoin(inner, on c.id = o.customer_id)
               ├─ probe: Scan(customers AS c) reads 5 of 5 columns [id, name, country, plan, signup_date], 12 rows
               └─ build: Scan(orders AS o) reads 6 of 6 columns [id, customer_id, sku, qty, amount, order_date], 30 rows
```

And with it on:

```
Limit(3)
└─ Project(c.name, "SUM(o.amount)" AS total)
   └─ Sort("SUM(o.amount)" DESC)
      └─ HashAggregate(group by c.name, SUM(o.amount))
         └─ HashJoin(inner, on c.id = o.customer_id)
            ├─ probe: Filter(c.country = 'CA')
            │  └─ Scan(customers AS c) reads 3 of 5 columns [id, name, country], 12 rows
            └─ build: Filter(o.amount > 20)
               └─ Scan(orders AS o) reads 2 of 6 columns [customer_id, amount], 30 rows

rules applied: predicate pushdown, projection pruning
```

Two things moved.

**The single `Filter` above the join became two filters below it.** The `WHERE` clause is a
conjunction, so it can be split: `c.country = 'CA'` only reads columns from `customers` and
`o.amount > 20` only reads columns from `orders`. Each half is pushed into the side that can
evaluate it. That matters because the join has to copy every build-side row into a hash table and
then probe it with every left row, and both of those costs are proportional to how many rows
arrive. Filtering first makes the join smaller rather than making the filter faster.

**Each `Scan` now reads fewer columns.** Nothing above the scans ever looks at `plan`,
`signup_date`, `sku`, `qty` or the two `id` columns, so the scans stop converting those fields.
With a CSV file that is not a small saving: parsing a field is most of what a scan does.

The two rules interact, and the order is not arbitrary. Pushdown runs first so that pruning sees
`Filter(o.amount > 20)` already sitting on the orders scan and keeps `amount` alive for it. Run
pruning first and the scan would drop a column the filter is about to need. There is a test named
`TestRuleOrderMatters` that exists only to say so.

`EXPLAIN ANALYZE` makes the difference concrete. The same query over a generated 200,000-row
`orders` and 5,000-row `customers`, with the optimizer on:

```
Project(c.country, "COUNT(*)" AS n, "SUM(o.amount)" AS total)  (rows=1, 69.27ms)
└─ HashAggregate(group by c.country, COUNT(*), SUM(o.amount))  (rows=1, 69.27ms)
   └─ HashJoin(inner, on c.id = o.customer_id)  (rows=2220, 69.27ms)
      ├─ probe: Filter(c.country = 'CA')  (rows=500, 507.0us)
      │  └─ Scan(customers AS c) reads 2 of 5 columns [id, country], 5000 rows  (rows=5000, 507.0us)
      └─ build: Filter(o.amount > 800)  (rows=21978, 62.46ms)
         └─ Scan(orders AS o) reads 2 of 6 columns [customer_id, amount], 200000 rows  (rows=200000, 51.56ms)
```

and off:

```
Project(c.country, "COUNT(*)" AS n, "SUM(o.amount)" AS total)  (rows=1, 199.06ms)
└─ HashAggregate(group by c.country, COUNT(*), SUM(o.amount))  (rows=1, 199.06ms)
   └─ Filter(c.country = 'CA' AND o.amount > 800)  (rows=2220, 199.06ms)
      └─ HashJoin(inner, on c.id = o.customer_id)  (rows=200000, 175.85ms)
         ├─ probe: Scan(customers AS c) reads 5 of 5 columns [id, name, country, plan, signup_date], 5000 rows  (rows=5000, 2.60ms)
         └─ build: Scan(orders AS o) reads 6 of 6 columns [id, customer_id, sku, qty, amount, order_date], 200000 rows  (rows=200000, 88.19ms)
```

Both answer identically. The join emitted 2,220 rows instead of 200,000, and the query went from
199 ms to 69 ms.

## Architecture

Each stage is a package that knows nothing about the next one.

```
SQL text
  │
  ▼
internal/lexer       hand-written scanner, every token carries a line and column
  │  []Token
  ▼
internal/parser      recursive descent, precedence climbing for expressions
  │  ast.Statement                                        (internal/ast)
  ▼
internal/logical     relational algebra: Scan, Filter, Project, Join, Aggregate, Sort, Limit
  │  logical.Node    names resolved against internal/catalog, aggregates rewritten
  ▼
internal/optimizer   predicate pushdown, then projection pruning
  │  logical.Node
  ▼
internal/physical    HashJoin, HashAggregate and friends, compiled by internal/eval
  │  physical.Operator
  ▼
rows (internal/engine drives it, cmd/pebbledb wraps it in a REPL)
```

A few decisions that shaped the rest:

**Logical nodes carry AST expressions, not compiled ones.** Pushing a predicate down is then just
moving an `ast.Expr` to a different node. Expressions are compiled exactly once, after the plan has
stopped changing, which matters because projection pruning changes what column index a name
resolves to. Getting this backwards is the mistake that makes an optimizer painful to write.

**Compilation produces closures, not an interpreter.** `internal/eval` turns an expression into a
tree of `func(types.Row) (types.Value, error)` with column references already resolved to indices,
so the per-row path never walks the AST and never looks a name up in a map.

**The aggregate node publishes named columns.** `HashAggregate` outputs one column per group key
and one per distinct aggregate call, and the planner rewrites everything above it to reference
those columns: `SUM(o.amount)` in the select list becomes a reference to the column the aggregate
node calls `"SUM(o.amount)"`. That is why the plans above show quoted names. It also gives the
"must appear in GROUP BY" error for free, since a select item still holding a bare column
reference after that rewrite is exactly that mistake.

**Rows are borrowed, not owned.** `Next` returns a slice that is valid until the next call, so a
scan reuses one row for the whole file. The three operators that buffer (`Sort`, the build side of
`HashJoin`, and `HashAggregate`'s group keys) clone. This is the kind of contract that is fine when
it is written down and a memory-corruption bug when it is not, so it is on the `Operator`
interface.

**Three-valued logic is implemented properly.** `NULL AND FALSE` is `FALSE`, `NULL OR TRUE` is
`TRUE`, a `NULL` join key matches nothing (not even another `NULL`), and `SUM` over no rows is
`NULL` rather than 0. These have tests of their own because they are invisible in a result set
until they are wrong.

## What it supports

```sql
SELECT  *, t.*, expr, expr AS alias
FROM    table [AS alias]
        [INNER | LEFT [OUTER]] JOIN table [AS alias] ON <condition>
WHERE   <predicate>
GROUP BY <expr>, ...
HAVING  <predicate>
ORDER BY <expr> [ASC | DESC], ...
LIMIT   <n>
```

Expressions: `+ - * / %`, `= <> != < <= > >=`, `AND OR NOT`, `IS [NOT] NULL`, `[NOT] IN (list)`,
`[NOT] LIKE` with `%` and `_`, parentheses, integer, float, string, `TRUE`, `FALSE` and `NULL`
literals, and double-quoted identifiers for column names that need them.

Aggregates: `COUNT(*)`, `COUNT(expr)`, `SUM`, `AVG`, `MIN`, `MAX`.

Also `EXPLAIN` and `EXPLAIN ANALYZE`, SQL comments (`--` and `/* */`), and case-insensitive
keywords and identifiers.

## Not implemented

This is a toy, and the list of what it does not do is longer than the list of what it does.

**No writes of any kind.** There is no `INSERT`, `UPDATE`, `DELETE`, `CREATE TABLE` or `DROP`.
Files are read-only, and there are no transactions, no locking and no durability concerns, because
nothing is ever written.

**No indexes.** Every query is a full scan of every file it touches. There is no index, no sorted
file, no zone map and no statistics beyond a row count, so there is nothing to look up a predicate
in.

**No subqueries, no CTEs, no `UNION`, no `DISTINCT`, no `OFFSET`, no window functions, no `CASE`,
and no scalar functions.** `IN` takes a list of literals, not a `SELECT`. The parser rejects each
of these by name rather than as a generic syntax error.

**No `RIGHT`, `FULL` or `CROSS` join, and no nested loop join.** Only `INNER` and `LEFT`, and a
join condition must contain at least one equality between the two tables. Without one there is no
hash key, and since the hash join is the only join operator, pebbledb refuses the query instead of
quietly computing a cross product. Join order is never changed: the order you write is the order
that runs, and the right side is always the one built into the hash table.

**Missed optimizations.** A `WHERE` predicate on the inner side of a left join that cannot be
`NULL` could turn that join into an inner join and then be pushed down; pebbledb leaves it above
the join. An inner join's `ON` predicate that touches only one side could be pushed into that
side; it stays a residual. Predicates are not pushed through a `Project`, through an `Aggregate`
(so a `HAVING` on a group key is never turned into a `WHERE`), or out of an `OR`. There is no
constant folding, no common subexpression elimination, and no cost model at all: both rules are
applied because they are never worse, not because anything was costed.

**Memory.** `Sort` and `HashAggregate` are blocking and keep everything in memory, and `HashJoin`
keeps the whole build side. There is no spilling, so a file larger than RAM will fail. `ORDER BY
... LIMIT 10` sorts all the rows instead of keeping a top-N heap, which is measurable: 412 ms for
`LIMIT 10` over 200,000 rows, against 43 ms for a plain scan of the same file.

**NULL and type edge cases.** An empty CSV field is read as `NULL`, which means a genuinely empty
string is not representable. There is no `DATE` type, so dates are strings that happen to sort
correctly in ISO form. There is no boolean column type: a column of `true` and `false` is read as
strings. `/` always produces a float, so `7 / 2` is `3.5` and not `3` as it would be in most SQL
engines. Division by zero is an error rather than `NULL`. `NULL` sorts first in `ASC` (`NULLS
FIRST`), which matches SQLite and not Postgres. `GROUP BY` emits groups in first-seen order, which
SQL does not promise, so add an `ORDER BY` if you care.

**`GROUP BY` matching is structural.** A select item is matched to a group key by comparing the
expressions' canonical text, so `GROUP BY qty * 2` matches `SELECT qty * 2` but not `SELECT 2 *
qty`. Case and spacing do not matter; operand order does. There is a test that will fail if this
ever changes, to keep this paragraph honest.

**`LIKE` is byte-oriented.** `_` matches one byte, not one rune, so it does the wrong thing on
multi-byte characters. Identifiers must be ASCII unless double-quoted. The result table counts
runes when aligning columns but assumes one cell per rune, so a double-width character will
misalign a row.

**`EXPLAIN ANALYZE` timings are inclusive of children** (an operator's time is measured around the
calls it makes into its inputs), and they are only as good as the platform clock. On the Windows
machine this was developed on, Go's monotonic clock has about 0.5 ms of granularity, measured, so
every operator in a query over the 30-row sample data reports `0ns`. The row counts are exact
everywhere; the timings need a workload bigger than the clock's resolution to mean anything.

## Tests

```bash
go test ./...
```

501 test cases. Statement coverage is 75.7% across the engine packages and 71.3% including the
REPL, which has no tests of its own beyond being driven by hand. The parts that carry the most
weight:

**A golden-file suite of 60 queries** in [internal/engine/testdata/golden](internal/engine/testdata/golden).
Each file holds one query, the plan before optimization, the plan after, which rules fired, and
the result rows. Asserting on the plan and the rows together is the point: a plan-only test cannot
catch an optimization that changes an answer. Regenerate with
`go test ./internal/engine -update`, and CI then fails if regenerating produces a diff, so a
golden file cannot drift away from what the engine really does.

**Every golden query runs twice,** with the rules on and off, and the suite fails if the two
results differ. That invariant is the only reason to trust the optimizer at all.

**Two fuzz targets.** `FuzzParse` asserts that the parser never panics and that anything which
parses prints back to SQL that re-parses to the same tree. It found two real bugs, both committed
as regression seeds in
[internal/parser/testdata/fuzz](internal/parser/testdata/fuzz/FuzzParse): identifiers that needed
quoting were printed unquoted, and float literals lost their decimal point, so `-0.0` was read
back as a negated integer. `FuzzQuery` drives the whole pipeline over the real sample data and
re-checks the optimize-on-equals-optimize-off invariant on whatever it invents. Both run for 45
seconds on every push. By hand they have gone further, 11.7 million parser executions and 1.3
million through the whole pipeline, with no further findings.

**Operator tests for the cases that are invisible in a result set:** a `NULL` join key matching
nothing, a left join null-extending rather than dropping, `SUM` over no rows being `NULL`, groups
coming out in first-seen order, and a `LIMIT` pulling only as many rows as it needs, which is
checked through the scan's own row counter rather than by timing, so it cannot flake.

Benchmarks generate a 200,000-row dataset in a temp directory:

```bash
go test ./internal/engine -run=XXX -bench=. -benchtime=10x
```

On an AMD Ryzen 7 PRO 8840HS, Go 1.27.3, Windows, 200,000 orders and 5,000 customers:

| benchmark | ms/op | what it measures |
|---|---:|---|
| `Scan` | 43 | `SELECT COUNT(*)`, which parses no fields at all |
| `ScanAllColumns` | 102 | the same scan with all six fields parsed |
| `Filter` | 65 | one selective predicate over the whole file |
| `JoinOptimized` | 97 | join, filter and group, rules on |
| `JoinUnoptimized` | 224 | the same query, rules off |
| `GroupBy` | 87 | hash aggregation into 500 groups |
| `SortAndLimit` | 412 | `ORDER BY ... LIMIT 10`, which sorts everything |

Reading and typing CSV fields dominates everything: the gap between `Scan` and `ScanAllColumns` is
why projection pruning is worth having, and `SortAndLimit` is the cost of not having a top-N heap.

## What I learned and what I would do differently

> Draft: the notes below are mine from building this; rewrite them in your own voice.

The optimizer was easier than I expected and the planner was harder. Pushdown is about thirty
lines of recursion; the subtlety is entirely in knowing when to stop, and almost all of that is the
left join, where restricting the preserved side is free and restricting the inner side silently
changes a dropped row into a null-extended one. Grouped queries, on the other hand, took three
attempts. The thing that finally made them simple was giving the aggregate node real named output
columns and rewriting the expressions above it to reference them, instead of trying to evaluate
aggregates in place.

Fuzzing paid for itself in about a minute. Both bugs it found were in the printer rather than the
parser, which I would not have thought to test: I only caught them because the property I asserted
was a round trip rather than "does not crash". That is the lesson I will reuse, which is that the
property you choose matters more than the time you spend fuzzing.

What I would do differently: keep the whole plan immutable. The rules rewrite nodes in place,
which means showing the before and after plans requires building the plan twice, and that is a
smell I had to write a comment to excuse. I would also implement `ORDER BY ... LIMIT` as a top-N
heap, since the benchmark shows it is the single worst thing in the engine, and the fact that it
turned up as a 412 ms line in a table rather than as a hunch is the strongest argument for writing
the benchmark before writing the README.

## License

MIT. See [LICENSE](LICENSE).
