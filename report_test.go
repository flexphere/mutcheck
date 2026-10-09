package main

import (
	"bytes"
	"strings"
	"testing"
)

func renderTx(t *testing.T, tx *Tx, verbose bool) (string, string) {
	t.Helper()
	var b bytes.Buffer
	tag := reportTx(&b, tx, fixtureDir, verbose, palette{})
	return b.String(), tag
}

// TestReportTx は内訳の表示に、書き込み・分岐・ループの記号・上限内の条件が出ることを確かめる。
func TestReportTx(t *testing.T) {
	_, txs := loadFixture(t, fixtureSchema)
	tests := []struct {
		fn   string
		tag  string
		want []string
	}{
		{"app.SimpleInsert", tagFixed, []string{
			"Commit  5 / 80,000",
			"insert  Parent  5 × 1  model.go:",
			"└ 列 4 + インデックス 1",
		}},
		{"app.IfElse", tagFixed, []string{
			"◇ if  app.go:59（どれか1つ。合計には項ごとに大きい方を使う）",
			"[if cond]",
			"[else]",
		}},
		{"app.NestedLoop", tagSymbolic, []string{
			"insert  Child  5 × cs·ps",
			"上限内の条件\n    cs·ps ≤ 16,000",
			"cs  len(cs) 回のループ  app.go:",
		}},
		{"app.Interface", tagFixed, []string{
			"Repository.Save  app.go:",
			"◇ 実装の候補（どれか1つ",
			"[ParentRepository.Save]",
			"[ChildRepository.Save]",
		}},
		{"app.PhiExclusive", tagFixed, []string{
			"◇ 値の合流  app.go:",
			"update  Parent  6 × 1",
			"insert  Child  5 × 1",
		}},
		{"app.DMLDelete", tagSymbolic, []string{
			"Commit  0 / 80,000",
			"DML（1文ごとに上限 80,000）",
			"DML DELETE  Parent  1·rows(Child←Parent) + 2·rows(Parent)  app.go:",
			"→ ほかの動的件数が 0 のとき: rows(Child←Parent) ≤ 80,000 / rows(Parent) ≤ 40,000",
			"Parent の削除で CASCADE 削除される Child の行数",
		}},
		{"app.DeleteSingleKey", tagSymbolic, []string{
			"delete  Parent  2 + 1·rows(Child←Parent)  model.go:",
			"rows(Child←Parent) ≤ 79,998",
		}},
		{"app.PrefilledAppend", tagSymbolic, []string{
			"Commit  5 + 5·cs / 80,000",
		}},
		{"app.ReadOnlyInRW", tagZero, []string{
			"⚠ 要確認: 書き込みが見つからない",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.fn, func(t *testing.T) {
			got, tag := renderTx(t, txs[tt.fn], true)
			if tag != tt.tag {
				t.Errorf("判定 = %s, want %s", tag, tt.tag)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("%q がない\n%s", w, got)
				}
			}
		})
	}
}

// TestReportWithoutVerbose は、-v がなければ内訳を出さないことを確かめる。
func TestReportWithoutVerbose(t *testing.T) {
	_, txs := loadFixture(t, fixtureSchema)
	got, _ := renderTx(t, txs["app.NestedLoop"], false)
	if !strings.Contains(got, "Commit  5·cs·ps / 80,000") || !strings.Contains(got, "cs·ps ≤ 16,000") {
		t.Errorf("合計か上限内の条件がない\n%s", got)
	}
	if strings.Contains(got, "insert  Child") {
		t.Errorf("-v なしで内訳が出ている\n%s", got)
	}
}

// TestReportDuplicateTable は、別の DB に同名テーブルがあるとき、schema ごとの書き込みを分岐として出すことを確かめる。
func TestReportDuplicateTable(t *testing.T) {
	_, txs := loadFixture(t, fixtureSchema, fixtureDupSchema)
	got, _ := renderTx(t, txs["app.SimpleInsert"], true)
	for _, w := range []string{"◇ 同名テーブル（どれか1つ", "[schema " + fixtureSchema + "]", "[schema " + fixtureDupSchema + "]"} {
		if !strings.Contains(got, w) {
			t.Errorf("%q がない\n%s", w, got)
		}
	}
}

// TestFactorCommon は、どの分岐にもある書き込みを分岐の外にくくり出しても合計が変わらないことを確かめる。
func TestFactorCommon(t *testing.T) {
	a := writeNode("insert", "A", Const(61), "", "x.go:1")
	b := writeNode("insert", "B", Const(13), "", "x.go:2")
	c := loopNode("len(r)@x.go:3", writeNode("insert", "C", Const(10), "", "x.go:4"))
	a2 := writeNode("insert", "A", Const(61), "", "x.go:1")
	n := altNode("値の合流", "x.go:5", []string{"経路 1", "経路 2"}, []*Node{seqNode(a, b), seqNode(a2, c)})
	if got, want := n.Total().String(), "74 + 10·len(r)@x.go:3"; got != want {
		t.Errorf("合計 = %q, want %q", got, want)
	}
	if n.Kind != NodeSeq || n.Children[0].Table != "A" || n.Children[1].Kind != NodeAlt {
		t.Errorf("共通の A が分岐の外に出ていない: %+v", n)
	}
}

// TestReportColor は色を付けたとき、判定・操作・記号に色が付き、記号の凡例の揃えが崩れないことを確かめる。
func TestReportColor(t *testing.T) {
	_, txs := loadFixture(t, fixtureSchema)
	var b bytes.Buffer
	reportTx(&b, txs["app.DMLDelete"], fixtureDir, true, palette{on: true})
	got := b.String()
	for _, w := range []string{
		"\x1b[33m[件数次第]\x1b[0m",
		"\x1b[36mDML DELETE\x1b[0m",
		"\x1b[35mrows(Parent)\x1b[0m        Parent の影響を受ける行数",
		"\x1b[1m≤ 40,000\x1b[0m",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("%q がない\n%s", w, got)
		}
	}
	b.Reset()
	reportTx(&b, txs["app.DMLDelete"], fixtureDir, true, palette{})
	if strings.Contains(b.String(), "\x1b[") {
		t.Errorf("色なしなのにエスケープがある")
	}
}
