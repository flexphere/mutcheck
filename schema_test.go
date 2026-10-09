package main

import (
	"strings"
	"testing"
)

const (
	fixtureDir       = "testdata/fixture"
	fixtureSchema    = "testdata/fixture/schema.sql"
	fixtureDupSchema = "testdata/fixture/schema_dup.sql"
)

func mustLoadSchemas(t *testing.T, paths ...string) *Schema {
	t.Helper()
	s, err := LoadSchemas(paths)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustTable(t *testing.T, s *Schema, name string) *Table {
	t.Helper()
	ts := s.Lookup(name)
	if len(ts) != 1 {
		t.Fatalf("%s: テーブルが %d 件", name, len(ts))
	}
	return ts[0]
}

func TestTableCosts(t *testing.T) {
	s := mustLoadSchemas(t, fixtureSchema)

	// want は Spanner Omni（2026.r4-lts）の CommitStats で実測した値
	tests := []struct {
		name   string
		table  string
		got    func(t *Table) int
		want   int
		reason string
	}{
		{"Parent insert", "Parent", func(t *Table) int { return t.InsertCost(nil) }, 5, "4列 + インデックス1本"},
		{"Parent 全列 update", "Parent", func(t *Table) int { return t.UpdateCost(nil) }, 6, "4列 + 値が変わりうるインデックス1本×2（上限）"},
		{"Parent InsertOrUpdate", "Parent", func(t *Table) int { return t.UpsertCost(nil) }, 6, "既存行で Name が変わると 4 + 2"},
		{"Parent delete", "Parent", func(t *Table) int { return 1 + t.DeleteIndexCost() }, 2, "1 + インデックス1本"},
		{"Child insert", "Child", func(t *Table) int { return t.InsertCost(nil) }, 5, "4列 + インデックス1本"},
		{"Gen insert", "Gen", func(t *Table) int { return t.InsertCost(nil) }, 7, "Note10: 書き込み3 + 生成1 + DEFAULT1 + 主キー1 + インデックス1"},
		{"Gen insert DEFAULT 列を省略", "Gen", func(t *Table) int { return t.InsertCost([]string{"ID", "Phone"}) }, 6, "Note10: 2 + 生成1 + DEFAULT1 + 主キー1 + インデックス1"},
		{"Gen Phone のみ update", "Gen", func(t *Table) int { return t.UpdateCost([]string{"ID", "Phone"}) }, 7, "Digits が再計算されるので Note10: 2 + 1 + 1 + 1 + インデックス2"},
		{"Gen Count のみ update", "Gen", func(t *Table) int { return t.UpdateCost([]string{"ID", "Count"}) }, 2, "再計算される生成列がないので Note3: 2"},
		{"Parent Status のみ update", "Parent", func(t *Table) int { return t.UpdateCost([]string{"OrgID", "ParentID", "Status"}) }, 3, "Status + 主キー2、インデックスは影響なし"},
		{"Parent Name のみ update", "Parent", func(t *Table) int { return t.UpdateCost([]string{"OrgID", "ParentID", "Name"}) }, 5, "Name + 主キー2 + インデックス2"},
		{"Gen DML UPDATE SET Phone", "Gen", func(t *Table) int { return t.DMLUpdateCost([]string{"Phone"}) }, 5, "Phone ∪ 再計算される Digits ∪ 主キー + インデックス2"},
		{"Gen DML UPDATE SET Count", "Gen", func(t *Table) int { return t.DMLUpdateCost([]string{"Count"}) }, 2, "Count ∪ 主キー"},
		{"Gen DML INSERT Count を省略", "Gen", func(t *Table) int { return t.DMLInsertCost([]string{"ID", "Phone"}) }, 5, "ID, Phone ∪ Digits, Count + インデックス1"},
		{"Parent DML UPDATE SET Name, Status", "Parent", func(t *Table) int { return t.DMLUpdateCost([]string{"Name", "Status"}) }, 6, "2 + 主キー2 + インデックス2"},
		{"Parent DML INSERT 全列", "Parent", func(t *Table) int { return t.DMLInsertCost([]string{"OrgID", "ParentID", "Name", "Status"}) }, 5, "4 + インデックス1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.got(mustTable(t, s, tt.table)); got != tt.want {
				t.Errorf("got %d, want %d (%s)", got, tt.want, tt.reason)
			}
		})
	}
}

func TestInterleave(t *testing.T) {
	s := mustLoadSchemas(t, fixtureSchema)
	parent := mustTable(t, s, "Parent")
	child := mustTable(t, s, "Child")
	if !child.Cascade || child.Parent != "Parent" {
		t.Errorf("Child: Parent=%q Cascade=%v", child.Parent, child.Cascade)
	}
	if len(parent.Children) != 1 || parent.Children[0] != child {
		t.Errorf("Parent.Children = %v", parent.Children)
	}
}

func TestLookupIsCaseInsensitive(t *testing.T) {
	s := mustLoadSchemas(t, fixtureSchema)
	if len(s.Lookup("parent")) != 1 {
		t.Error("小文字のテーブル名で引けない")
	}
}

func TestDuplicateTablesAcrossSchemas(t *testing.T) {
	s := mustLoadSchemas(t, fixtureSchema, fixtureDupSchema)
	ts := s.Lookup("Parent")
	if len(ts) != 2 {
		t.Fatalf("Parent が %d 件（2 件を期待）", len(ts))
	}
	if ts[0].Source == ts[1].Source {
		t.Error("定義元の schema が区別されていない")
	}
	// 親子関係は schema ファイルごとに解決し、別 DB の同名テーブルとは混ざらない
	for _, p := range ts {
		if p.Source == fixtureDupSchema && len(p.Children) != 0 {
			t.Errorf("別 schema の Parent に子テーブルが付いている: %v", p.Children)
		}
	}
	if len(s.Lookup("Child")) != 1 {
		t.Error("Child は1件のはず")
	}
}

func TestAllTables(t *testing.T) {
	s := mustLoadSchemas(t, fixtureSchema, fixtureDupSchema)
	var got []string
	for _, tb := range s.AllTables() {
		got = append(got, tb.Name+"@"+tb.Source)
	}
	// schema ファイルの指定順、ファイル内ではテーブル名順。別 DB の同名テーブルは別の行
	want := []string{
		"Child@" + fixtureSchema, "Gen@" + fixtureSchema, "Parent@" + fixtureSchema,
		"Parent@" + fixtureDupSchema,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCost(t *testing.T) {
	c := Const(3)
	c.Add(Const(2).Mul("len(b)").Mul("len(a)"))
	if got, want := c.String(), "3 + 2·len(a)·len(b)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	m := Cost{"": 5, "x": 1}
	m.Max(Cost{"": 3, "x": 4, "y": 2})
	if got, want := m.String(), "5 + 4·x + 2·y"; got != want {
		t.Errorf("Max = %q, want %q", got, want)
	}
	if !Const(1).IsFixed() || m.IsFixed() {
		t.Error("IsFixed が誤っている")
	}
}

func TestLimitLines(t *testing.T) {
	name := func(sym string) string { return sym }
	base := func(sym string) string { return strings.SplitN(sym, "@", 2)[0] }
	tests := []struct {
		name     string
		c        Cost
		want     []string
		exceeded bool
	}{
		{"記号1つ", Cost{"": 143, "len(x)": 17}, []string{"len(x) ≤ 4,697"}, false},
		{"定数のみ", Const(100), nil, false},
		{"定数だけで超過", Const(80001), []string{"定数部分だけで上限 80,000 を超える"}, true},
		{"記号の積", Cost{"len(a)·len(b)": 8}, []string{"len(a)·len(b) ≤ 10,000"}, false},
		{"記号が2つ", Cost{"": 100, "len(a)": 3, "len(b)": 5}, []string{
			"3·len(a) + 5·len(b) ≤ 79,900",
			"ほかの動的件数が 0 のとき: len(a) ≤ 26,633 / len(b) ≤ 15,980",
		}, false},
		{"積の項と積でない項", Cost{"len(p)": 29, "len(p)·len(u)": 15}, []string{
			"29·len(p) + 15·len(p)·len(u) ≤ 80,000",
			"ほかの動的件数が 0 のとき: len(p) ≤ 2,758",
		}, false},
		{"同じ名前の記号", Cost{"len(a)@x.go:1": 10, "len(a)@x.go:2": 6}, []string{
			"10·len(a)@x.go:1 + 6·len(a)@x.go:2 ≤ 80,000",
			"ほかの動的件数が 0 のとき: len(a)@x.go:1 ≤ 8,000 / len(a)@x.go:2 ≤ 13,333",
			"同じ名前の動的件数を同じ値とみなすと: len(a) ≤ 5,000（推定）",
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ex := tt.c.LimitLines(MutationLimit, name, base)
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") || ex != tt.exceeded {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ex, tt.want, tt.exceeded)
			}
		})
	}
}
