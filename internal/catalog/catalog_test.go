package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shell-mp3/pebbledb/internal/types"
)

// writeCSV creates a CSV file in a temp dir and returns its path.
func writeCSV(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestInferKinds(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []types.Kind
	}{
		{
			name: "all integers",
			body: "a,b\n1,2\n3,4\n",
			want: []types.Kind{types.KindInt, types.KindInt},
		},
		{
			name: "one decimal widens the column to float",
			body: "a\n1\n2\n3.5\n",
			want: []types.Kind{types.KindFloat},
		},
		{
			name: "one non-number widens the column to string",
			body: "a\n1\n2\nx\n",
			want: []types.Kind{types.KindString},
		},
		{
			name: "widening is one way, float then int stays float",
			body: "a\n1.5\n2\n",
			want: []types.Kind{types.KindFloat},
		},
		{
			name: "empty fields are ignored during inference",
			body: "a\n1\n\n3\n",
			want: []types.Kind{types.KindInt},
		},
		{
			name: "a wholly empty column is a string",
			body: "a,b\n1,\n2,\n",
			want: []types.Kind{types.KindInt, types.KindString},
		},
		{
			name: "header only",
			body: "a,b\n",
			want: []types.Kind{types.KindString, types.KindString},
		},
		{
			name: "hex and underscore forms are not numbers",
			body: "a,b\n0x1f,1_000\n0x2a,2_000\n",
			want: []types.Kind{types.KindString, types.KindString},
		},
		{
			name: "Inf and NaN are words, not numbers",
			body: "a,b\nInf,NaN\n1,2\n",
			want: []types.Kind{types.KindString, types.KindString},
		},
		{
			name: "exponent notation is a float",
			body: "a\n1e3\n2e4\n",
			want: []types.Kind{types.KindFloat},
		},
		{
			name: "integers that do not fit in int64 become floats",
			body: "a\n99999999999999999999\n",
			want: []types.Kind{types.KindFloat},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			if err := c.Add(writeCSV(t, "t.csv", tc.body)); err != nil {
				t.Fatalf("Add: %v", err)
			}
			tbl, err := c.Get("t")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if tbl.Schema.Len() != len(tc.want) {
				t.Fatalf("got %d columns, want %d", tbl.Schema.Len(), len(tc.want))
			}
			for i, want := range tc.want {
				if got := tbl.Schema.Columns[i].Kind; got != want {
					t.Errorf("column %d kind = %v, want %v", i, got, want)
				}
			}
		})
	}
}

func TestRowCountExcludesHeader(t *testing.T) {
	c := New()
	if err := c.Add(writeCSV(t, "t.csv", "a\n1\n2\n3\n")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	tbl, _ := c.Get("t")
	if tbl.Rows != 3 {
		t.Errorf("Rows = %d, want 3", tbl.Rows)
	}
}

func TestTableNameFromFileName(t *testing.T) {
	c := New()
	if err := c.Add(writeCSV(t, "Customers.CSV", "a\n1\n")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := c.Get("customers"); err != nil {
		t.Errorf("table should be registered lower-cased: %v", err)
	}
	if _, err := c.Get("CUSTOMERS"); err != nil {
		t.Errorf("lookup should be case-insensitive: %v", err)
	}
	if tbl, _ := c.Get("customers"); tbl.Schema.Columns[0].Table != "customers" {
		t.Errorf("columns are qualified by %q, want customers", tbl.Schema.Columns[0].Table)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{"empty file", "", "file is empty"},
		{"unnamed header column", "a,,c\n1,2,3\n", "has no name"},
		{"duplicate header column", "a,A\n1,2\n", "duplicate column"},
		{"ragged row", "a,b\n1,2\n3\n", "wrong number of fields"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := New().Add(writeCSV(t, "t.csv", tc.body))
			if err == nil {
				t.Fatalf("Add succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

func TestDuplicateTableNameIsRejected(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "t.csv"), filepath.Join(sub, "t.csv")} {
		if err := os.WriteFile(p, []byte("a\n1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Load([]string{filepath.Join(dir, "t.csv"), filepath.Join(sub, "t.csv")})
	if err == nil || !strings.Contains(err.Error(), "two files map to table") {
		t.Errorf("Load error = %v, want a duplicate table name error", err)
	}
}

func TestLoadDirectoryAndGlob(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.csv", "b.csv", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x\n1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, arg := range []string{dir, filepath.Join(dir, "*.csv")} {
		c, err := Load([]string{arg})
		if err != nil {
			t.Fatalf("Load(%q): %v", arg, err)
		}
		if got := c.Names(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Errorf("Load(%q) names = %v, want [a b]", arg, got)
		}
	}
}

func TestLoadMissingPath(t *testing.T) {
	if _, err := Load([]string{filepath.Join(t.TempDir(), "nope.csv")}); err == nil {
		t.Error("Load of a missing file succeeded, want an error")
	}
	if _, err := Load(nil); err == nil {
		t.Error("Load of nothing succeeded, want an error")
	}
}

func TestGetUnknownTableListsWhatIsLoaded(t *testing.T) {
	c := New()
	if err := c.Add(writeCSV(t, "orders.csv", "a\n1\n")); err != nil {
		t.Fatal(err)
	}
	_, err := c.Get("order")
	if err == nil {
		t.Fatal("Get of an unknown table succeeded")
	}
	if !strings.Contains(err.Error(), "orders") {
		t.Errorf("error %q should list the loaded tables to help with typos", err)
	}
}

func TestParseField(t *testing.T) {
	tests := []struct {
		field string
		kind  types.Kind
		want  types.Value
	}{
		{"", types.KindInt, types.Null},
		{"", types.KindString, types.Null},
		{"42", types.KindInt, types.NewInt(42)},
		{"-42", types.KindInt, types.NewInt(-42)},
		{"1.5", types.KindFloat, types.NewFloat(1.5)},
		{"2", types.KindFloat, types.NewFloat(2)},
		{"hi", types.KindString, types.NewString("hi")},
	}
	for _, tc := range tests {
		got, err := ParseField(tc.field, tc.kind)
		if err != nil {
			t.Fatalf("ParseField(%q, %v): %v", tc.field, tc.kind, err)
		}
		if got != tc.want {
			t.Errorf("ParseField(%q, %v) = %v, want %v", tc.field, tc.kind, got, tc.want)
		}
	}
	if _, err := ParseField("x", types.KindInt); err == nil {
		t.Error("ParseField of a non-integer as int succeeded")
	}
}

// TestSampleData checks the CSVs committed in data/, since every golden file
// and every README number is derived from them.
func TestSampleData(t *testing.T) {
	c, err := Load([]string{filepath.Join("..", "..", "data")})
	if err != nil {
		t.Fatalf("Load(data): %v", err)
	}
	want := map[string]struct {
		rows   int64
		schema string
	}{
		"customers": {12, "customers.id int, customers.name string, customers.country string, customers.plan string, customers.signup_date string"},
		"orders":    {30, "orders.id int, orders.customer_id int, orders.sku string, orders.qty int, orders.amount float, orders.order_date string"},
		"products":  {8, "products.sku string, products.title string, products.category string, products.unit_price float"},
	}
	if got := len(c.Names()); got != len(want) {
		t.Fatalf("data/ has %d tables, want %d: %v", got, len(want), c.Names())
	}
	for name, w := range want {
		tbl, err := c.Get(name)
		if err != nil {
			t.Fatalf("Get(%q): %v", name, err)
		}
		if tbl.Rows != w.rows {
			t.Errorf("%s has %d rows, want %d", name, tbl.Rows, w.rows)
		}
		if got := tbl.Schema.String(); got != w.schema {
			t.Errorf("%s schema\n got: %s\nwant: %s", name, got, w.schema)
		}
	}
}
