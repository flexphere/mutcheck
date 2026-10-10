package main

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

var (
	fixtureOnce     sync.Once
	fixtureAnalyzer *Analyzer
	fixtureErr      error
)

// loadFixture は fixture モジュールを1回だけ読み込む。schema を差し替えるときは同じ SSA から Analyzer を作り直す。
func loadFixture(t *testing.T, schemaPaths ...string) (*Analyzer, map[string]*Tx) {
	t.Helper()
	fixtureOnce.Do(func() {
		s, err := LoadSchemas([]string{fixtureSchema})
		if err != nil {
			fixtureErr = err
			return
		}
		fixtureAnalyzer, fixtureErr = loadModule(fixtureDir, s, nil)
	})
	if fixtureErr != nil {
		t.Fatal(fixtureErr)
	}
	a := NewAnalyzer(fixtureAnalyzer.prog, mustLoadSchemas(t, schemaPaths...), fixtureAnalyzer.modPrefix)
	a.BuildImplIndex()
	txs := map[string]*Tx{}
	for _, tx := range a.FindTransactions() {
		a.Analyze(tx)
		txs[strings.ReplaceAll(tx.Encl.String(), "example.com/fixture/", "")] = tx
	}
	return a, txs
}

var symPos = regexp.MustCompile(`@[^·\s]+`)

// costString は記号の位置（@file:line）を除いた式を返す。
func costString(c Cost) string {
	return symPos.ReplaceAllString(c.String(), "")
}

func notesOf(tx *Tx) []string {
	var out []string
	for n := range tx.Summary.Notes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

type txCase struct {
	fn      string
	want    string // Commit 側の期待値
	wantDML string // DML 1 文あたりの最大値の期待値。空なら 0
	note    string // 注記に含まれるべき文字列
	zero    string // mutation 数が 0 のときの要確認の理由に含まれるべき文字列
}

// 単価は Spanner Omni の実測値: Parent insert 5 / 全列 update 6 / delete 2、Child insert 5 / delete 2、Gen insert 7 / Phone の update 7
var txCases = []txCase{
	{fn: "app.SimpleInsert", want: "5"},
	{fn: "app.UpdateAll", want: "6"},
	{fn: "app.GeneratedInsert", want: "7"},
	{fn: "app.LoopInsert", want: "5·len(ps)"},
	{fn: "app.NestedLoop", want: "5·len(cs)·len(ps)"},
	{fn: "app.IfElse", want: "10"},
	{fn: "app.Switch", want: "5"},
	{fn: "app.Sequence", want: "10"},
	{fn: "app.Interface", want: "5"}, // mock（15）は除外し、実装 5 と 5 の最大値
	{fn: "app.DMLDelete", want: "0", wantDML: "1·rows(Child←Parent) + 2·rows(Parent)", note: "CASCADE 削除される Child"},
	{fn: "app.DMLUpdateStructLit", want: "0", wantDML: "5·rows(Gen)"},
	{fn: "app.DMLInsertValues", want: "0", wantDML: "10"},
	{fn: "app.DeleteSingleKey", want: "2 + 1·rows(Child←Parent)", note: "CASCADE 削除される Child"},
	{fn: "app.DeleteRange", want: "1 + 1·rows(Child←Parent) + 1·rows(Parent)"},
	{fn: "app.ClosureToExternal", want: "5·calls(slices.ContainsFunc)"},
	{fn: "app.DirectClosure", want: "10"},
	{fn: "app.HelperReturnsMutations", want: "10"},
	{fn: "app.BodyAsVariable", want: "5"},
	{fn: "app.NestedTx", want: "5", note: "別トランザクションを開始"},
	{fn: "app.NestedTx$1", want: "5"},
	{fn: "app.OutsideBody", want: "5"},
	{fn: "app.FuncValue", want: "5"},
	{fn: "app.CreatedNotBuffered", want: "5"},
	{fn: "app.PrefilledAppend", want: "5 + 5·len(cs)"},
	{fn: "app.MakeIndexAssign", want: "5·len(ps)"},
	{fn: "app.ParamMutations", want: "5"},
	{fn: "app.TupleHelper", want: "10"},
	{fn: "app.BufferInLoop", want: "5·len(ps)"},
	{fn: "(*app.job).withTx", want: "5"},
	{fn: "app.runTx", want: "5"},
	{fn: "app.PhiExclusive", want: "6"}, // Parent の全列 update（6）と Child の insert（5）の最大値
	{fn: "app.PhiSharedBase", want: "10"},
	{fn: "app.CapturedIndexAssign", want: "5·len(ps)"},
	{fn: "app.ReadOnlyInRW", want: "0", zero: "書き込みが見つからない"},
	{fn: "app.UntraceableBuffer", want: "0", zero: "書き込みはあるが"},
	{fn: "app.DMLInLoop", want: "0", wantDML: "2·rows(Child)"},
	{fn: "app.MixedDMLAndMutation", want: "5", wantDML: "3·rows(Parent)"},
	{fn: "app.ValueReceiver", want: "6"},
	{fn: "app.MethodValue", want: "5"},
	{fn: "(app.parentSaver).RunTx", want: "5"},
}

func TestTransactions(t *testing.T) {
	_, txs := loadFixture(t, fixtureSchema)
	tested := map[string]bool{}
	for _, tt := range txCases {
		tested[tt.fn] = true
		t.Run(tt.fn, func(t *testing.T) {
			tx, ok := txs[tt.fn]
			if !ok {
				t.Fatalf("トランザクションが見つからない")
			}
			if got := costString(tx.Summary.Cost); got != tt.want {
				t.Errorf("got %q, want %q\nnotes: %v", got, tt.want, notesOf(tx))
			}
			wantDML := tt.wantDML
			if wantDML == "" {
				wantDML = "0"
			}
			if gotDML := costString(tx.Summary.DML); gotDML != wantDML {
				t.Errorf("DML: got %q, want %q", gotDML, wantDML)
			}
			if tt.note != "" && !strings.Contains(strings.Join(notesOf(tx), "\n"), tt.note) {
				t.Errorf("注記 %q がない: %v", tt.note, notesOf(tx))
			}
			if r := tx.Summary.ZeroReason(); !strings.Contains(r, tt.zero) || (tt.zero == "") != (r == "") {
				t.Errorf("要確認の理由 = %q, want %q を含む", r, tt.zero)
			}
		})
	}
	// fixture に追加したパターンのテスト漏れを防ぐ
	for fn := range txs {
		if !tested[fn] {
			t.Errorf("テストケースがないトランザクション: %s", fn)
		}
	}
}

func TestDuplicateTableUsesMax(t *testing.T) {
	_, txs := loadFixture(t, fixtureSchema, fixtureDupSchema)
	tx := txs["app.SimpleInsert"]
	// schema_dup.sql の Parent はインデックスが1本多い: 4列 + インデックス2本
	if got, want := costString(tx.Summary.Cost), "6"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !strings.Contains(strings.Join(notesOf(tx), "\n"), "複数の schema に同名テーブル Parent") {
		t.Errorf("同名テーブルの注記がない: %v", notesOf(tx))
	}
}
