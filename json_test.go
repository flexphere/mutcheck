package main

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// fixtureTxs は fixture のトランザクションを関数名順に返す。
func fixtureTxs(t *testing.T) (*Analyzer, []*Tx) {
	a, m := loadFixture(t, fixtureSchema)
	var txs []*Tx
	for _, tx := range m {
		txs = append(txs, tx)
	}
	sort.Slice(txs, func(i, j int) bool { return txs[i].Encl.String() < txs[j].Encl.String() })
	return a, txs
}

// TestJSONReport は JSON の判定がテキスト出力の判定と同じで、件数・式・動的件数を構造化して出すことを確かめる。
func TestJSONReport(t *testing.T) {
	_, txs := fixtureTxs(t)
	var b bytes.Buffer
	if err := writeJSON(&b, (&jsonBuilder{dir: fixtureDir}).report(txs)); err != nil {
		t.Fatal(err)
	}
	var got jsonReport
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Limit != MutationLimit || got.Summary.Total != len(txs) {
		t.Errorf("limit = %d, total = %d", got.Limit, got.Summary.Total)
	}
	byFunc := map[string]jsonTx{}
	for i, jt := range got.Transactions {
		byFunc[jt.Func] = jt
		if want := verdictKeys[reportTx(io.Discard, txs[i], fixtureDir, false, palette{})]; jt.Verdict != want {
			t.Errorf("%s: verdict = %q, テキスト出力では %q", jt.Func, jt.Verdict, want)
		}
		if jt.Tree != nil {
			t.Errorf("%s: -v なしなのに tree がある", jt.Func)
		}
	}

	pa := byFunc["app.PrefilledAppend"]
	if pa.Commit.Expr != "5 + 5·cs" || pa.Commit.Const != 5 || pa.Commit.Headroom != 79995 {
		t.Errorf("commit = %+v", pa.Commit)
	}
	if len(pa.Commit.Terms) != 1 || pa.Commit.Terms[0].Coef != 5 || pa.Commit.Terms[0].Syms[0] != "cs" || pa.Commit.Terms[0].Max != 15999 {
		t.Errorf("terms = %+v", pa.Commit.Terms)
	}
	// 位置はテキスト出力と違い、モジュールからの相対パスを付ける
	if len(pa.Symbols) != 1 || pa.Symbols[0].Name != "cs" || pa.Symbols[0].Desc != "len(cs) 回のループ" || pa.Symbols[0].Pos != "app/app.go:239" {
		t.Errorf("symbols = %+v", pa.Symbols)
	}

	dd := byFunc["app.DMLDelete"]
	if len(dd.DML) != 1 || dd.DML[0].Op != "DML DELETE" || dd.DML[0].Table != "Parent" || dd.DML[0].Pos != "app/app.go:104" || dd.DML[0].Cost.Expr != "1·rows(Child←Parent) + 2·rows(Parent)" {
		t.Errorf("dml = %+v", dd.DML)
	}
	if len(dd.Notes) == 0 {
		t.Error("注記がない")
	}

	ro := byFunc["app.ReadOnlyInRW"]
	if ro.Verdict != "check" || ro.CheckReason == "" {
		t.Errorf("verdict = %q, check_reason = %q", ro.Verdict, ro.CheckReason)
	}
}

// TestJSONReportVerbose は -v のときに内訳の木を出すことを確かめる。
func TestJSONReportVerbose(t *testing.T) {
	_, txs := loadFixture(t, fixtureSchema)
	jt := (&jsonBuilder{dir: fixtureDir, verbose: true}).tx(txs["app.IfElse"])
	if jt.Tree == nil || jt.Tree.Kind != "alt" || len(jt.Tree.Branches) != 2 || jt.Tree.Total != "10" {
		t.Fatalf("tree = %+v", jt.Tree)
	}
	var writes []*jsonNode
	var walk func(n *jsonNode)
	walk = func(n *jsonNode) {
		if n.Kind == "write" {
			writes = append(writes, n)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(jt.Tree)
	if len(writes) != 3 || writes[0].Table != "Parent" || writes[0].Unit != "5" || writes[0].Detail == "" || writes[0].Pos != "model/model.go:16" {
		t.Errorf("writes = %+v", writes)
	}
}

func TestJSONTables(t *testing.T) {
	got := buildJSONTables(mustLoadSchemas(t, fixtureSchema), "Parent,Nope")
	if len(got.Schemas) != 1 || len(got.Schemas[0].Tables) != 1 {
		t.Fatalf("schemas = %+v", got.Schemas)
	}
	p := got.Schemas[0].Tables[0]
	if p.Name != "Parent" || p.Columns != 4 || p.Indexes != 1 || p.Insert != 5 || p.Update != 6 || p.Delete != 2 {
		t.Errorf("Parent = %+v", p)
	}
	if len(p.Cascade) != 1 || p.Cascade[0].Table != "Child" || p.Cascade[0].PerRow != 1 || p.Cascade[0].Sym != "rows(Child←Parent)" {
		t.Errorf("cascade = %+v", p.Cascade)
	}
	if len(got.Missing) != 1 || got.Missing[0] != "Nope" {
		t.Errorf("missing = %v", got.Missing)
	}
}

func TestParseFailOn(t *testing.T) {
	got, err := parseFailOn("over, check")
	if err != nil {
		t.Fatal(err)
	}
	if !got[tagExceeded] || !got[tagZero] || got[tagFixed] || got[tagSymbolic] {
		t.Errorf("got %v", got)
	}
	if _, err := parseFailOn("超過"); err == nil {
		t.Error("知らない値でエラーにならない")
	}
	if got, _ := parseFailOn(""); len(got) != 0 {
		t.Errorf("空なら何も当たらない: %v", got)
	}
}

// TestJSONPosHasPath は、JSON の位置がどれもモジュールからの相対パス付きであることを確かめる。
// 同じファイル名が別のディレクトリにあっても区別できるように、内部では位置をパス付きで持つ。
func TestJSONPosHasPath(t *testing.T) {
	_, txs := fixtureTxs(t)
	var b bytes.Buffer
	if err := writeJSON(&b, (&jsonBuilder{dir: fixtureDir, verbose: true}).report(txs)); err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	n := 0
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if p, ok := x["pos"].(string); ok {
				n++
				if !strings.Contains(p, "/") || strings.HasPrefix(p, "/") {
					t.Errorf("pos %q が相対パス付きでない", p)
				}
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(v)
	if n == 0 {
		t.Error("pos が1つもない")
	}
}

// TestNoSyntheticCallInTree は、SSA が作ったラッパーの中の位置のない呼び出しを内訳の段にしないことを確かめる。
func TestNoSyntheticCallInTree(t *testing.T) {
	_, txs := fixtureTxs(t)
	for _, tx := range txs {
		var b bytes.Buffer
		reportTx(&b, tx, fixtureDir, true, palette{})
		if strings.Contains(b.String(), ".:0") || strings.Contains(b.String(), "  :0") {
			t.Errorf("%s: 位置のない段がある\n%s", funcLabel(tx.Encl), b.String())
		}
	}
	_, m := loadFixture(t, fixtureSchema)
	// 値のメソッドとポインタ版のラッパーは中身が同じなので、実装の候補の分岐にならない
	if tree := m["app.ValueReceiver"].Summary.Tree; tree.Kind != NodeCall || tree.Children[0].Kind == NodeAlt {
		t.Errorf("ValueReceiver の内訳 = %+v", tree)
	}
}

// TestLoadErrors は、型エラーがあるパッケージでは go list がまとめたエラーを除き、位置を相対パスにすることを確かめる。
func TestLoadErrors(t *testing.T) {
	pkgs := []*packages.Package{
		{PkgPath: "m/b", Errors: []packages.Error{
			{Pos: "-", Msg: "# m/b\nb.go:3:1: x undefined", Kind: packages.ListError},
			{Pos: "/mod/b/b.go:3:1", Msg: "x undefined", Kind: packages.TypeError},
		}},
		{PkgPath: "m/a", Errors: []packages.Error{{Pos: "-", Msg: "no Go files", Kind: packages.ListError}}},
		{PkgPath: "m/ok"},
	}
	got := loadErrors(pkgs, "/mod")
	want := []LoadError{
		{Package: "m/a", Msg: "no Go files"},
		{Package: "m/b", Pos: "b/b.go:3:1", Msg: "x undefined"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestFailedTxs(t *testing.T) {
	_, txs := fixtureTxs(t)
	check, _ := parseFailOn("check")
	// fixture の要確認は ReadOnlyInRW と UntraceableBuffer
	if n := failedTxs(txs, check); n != 2 {
		t.Errorf("check = %d, want 2", n)
	}
	if n := failedTxs(txs, map[string]bool{}); n != 0 {
		t.Errorf("指定なし = %d, want 0", n)
	}
}

// TestTxCallers は、トランザクションを始める関数の直接の呼び出し元を、静的な呼び出しと interface 経由の両方で拾うことを確かめる。
func TestTxCallers(t *testing.T) {
	a, txs := loadFixture(t, fixtureSchema)
	b := &jsonBuilder{dir: fixtureDir}
	for _, tt := range []struct {
		fn   string
		want []jsonCaller
	}{
		{"app.runTx", []jsonCaller{{Pos: "app/app.go:318", Func: "app.BodyParam"}}},
		{"(*app.job).withTx", []jsonCaller{{Pos: "app/app.go:303", Func: "job.Run"}}},
		{"(app.parentSaver).RunTx", []jsonCaller{{Pos: "app/app.go:457", Func: "app.InvokeTx"}}},
		{"app.SimpleInsert", []jsonCaller{}},
	} {
		tx := txs[tt.fn]
		if tx == nil {
			t.Fatalf("%s: トランザクションが見つからない", tt.fn)
		}
		tx.Callers = a.TxCallers(tx)
		got := b.tx(tx).Callers
		if len(got) != len(tt.want) {
			t.Errorf("%s: callers = %+v, want %+v", tt.fn, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("%s: callers = %+v, want %+v", tt.fn, got, tt.want)
			}
		}
	}
}

// TestReportDeterministic は同じコードなら何度解析しても同じ出力になることを確かめる。
// パッケージやメンバーの走査順（マップの順）が出力に漏れると、interface の実装の候補の並びが実行ごとに変わる。
func TestReportDeterministic(t *testing.T) {
	var first string
	for i := range 10 {
		_, txs := fixtureTxs(t)
		var b bytes.Buffer
		if err := writeJSON(&b, (&jsonBuilder{dir: fixtureDir, verbose: true}).report(txs)); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = b.String()
		} else if b.String() != first {
			t.Fatalf("%d 回目の出力が 1 回目と違う", i+1)
		}
	}
}
