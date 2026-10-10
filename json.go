package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// -format json の出力。AI Agent や CI から読むためのもので、テキスト出力と同じ解析結果を構造化して出す。
// フィールド名・判定の値は互換性を保つ（変えるときは README の JSON の節も直す）。

type jsonReport struct {
	// Limit は Commit 1回、および DML 1文あたりの mutation 数の上限
	Limit        int           `json:"limit"`
	Transactions []jsonTx      `json:"transactions"`
	Summary      jsonSummary   `json:"summary"`
	Coverage     *jsonCoverage `json:"coverage,omitempty"`
	// LoadErrors はパッケージの読み込み・型チェックのエラー。エラーのあるパッケージの中のトランザクションは transactions に入らない
	LoadErrors []LoadError `json:"load_errors"`
}

type jsonSummary struct {
	Total     int `json:"total"`
	Fixed     int `json:"fixed"`
	Dynamic   int `json:"dynamic"`
	Over      int `json:"over"`
	Check     int `json:"check"`
	WithNotes int `json:"with_notes"`
}

type jsonTx struct {
	Func string `json:"func"`
	// FuncFull は -func が照合する関数の完全な名前。^...$ で囲んで -func に渡すと、この関数のトランザクションだけを表示できる
	FuncFull string `json:"func_full"`
	Pos      string `json:"pos"`
	// Verdict は fixed / dynamic / over / check
	Verdict     string     `json:"verdict"`
	CheckReason string     `json:"check_reason,omitempty"`
	Commit      jsonCost   `json:"commit"`
	DML         []jsonStmt `json:"dml"`
	Symbols     []jsonSym  `json:"symbols"`
	Notes       []string   `json:"notes"`
	// Callers はトランザクションを始める関数を直接呼んでいる箇所（1 段だけ。interface 経由は実装しうるものを含む）
	Callers []jsonCaller `json:"callers"`
	// Tree は Commit 分の内訳。-v のときだけ出す
	Tree *jsonNode `json:"tree,omitempty"`
}

type jsonCaller struct {
	Pos  string `json:"pos"`
	Func string `json:"func"`
}

// jsonCost は mutation 数の式。動的件数は symbols の name で書く。
type jsonCost struct {
	Expr  string     `json:"expr"`
	Const int        `json:"const"`
	Terms []jsonTerm `json:"terms"`
	// Headroom は上限から定数部分を引いた残り。動的件数の項の合計がこれ以下なら上限内
	Headroom int `json:"headroom"`
	// Limits は上限内に収まる条件（テキスト出力の「上限内の条件」と同じ）
	Limits []string `json:"limits"`
}

type jsonTerm struct {
	Coef int      `json:"coef"`
	Syms []string `json:"syms"`
	// Max は、ほかの項が 0 のときにこの項の動的件数（の積）がとれる最大値
	Max int `json:"max"`
}

type jsonStmt struct {
	Op    string    `json:"op"`
	Table string    `json:"table"`
	Pos   string    `json:"pos"`
	Cost  jsonCost  `json:"cost"`
	Tree  *jsonNode `json:"tree,omitempty"`
}

type jsonSym struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
	Pos  string `json:"pos"`
}

type jsonNode struct {
	// Kind は write / seq / alt / call / loop
	Kind  string `json:"kind"`
	Label string `json:"label,omitempty"`
	Pos   string `json:"pos,omitempty"`
	Table string `json:"table,omitempty"`
	// Unit は write の1回あたりの mutation 数
	Unit   string `json:"unit,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Sym は loop で掛ける動的件数
	Sym string `json:"sym,omitempty"`
	// Branches は alt の各分岐のラベル（children と同じ並び）。Optional は書き込みのない経路もあること
	Branches []string    `json:"branches,omitempty"`
	Optional bool        `json:"optional,omitempty"`
	Total    string      `json:"total"`
	Children []*jsonNode `json:"children,omitempty"`
}

type jsonCoverage struct {
	Unreached []jsonSite `json:"unreached"`
	Outside   []jsonSite `json:"outside"`
}

type jsonSite struct {
	Pos  string `json:"pos"`
	Func string `json:"func"`
	Call string `json:"call"`
	Hint string `json:"hint,omitempty"`
	Cost string `json:"cost,omitempty"`
}

// jsonBuilder は解析結果を JSON の形にする。
type jsonBuilder struct {
	dir     string
	verbose bool
}

// pos は「パス:行」の位置を、解析対象のモジュールからの相対パスにする。
// テキスト出力は読みやすさのためファイル名だけにしているが、JSON はそのままファイルを開けるようにパスを付ける。
func (b *jsonBuilder) pos(full string) string {
	i := strings.LastIndex(full, ":")
	if i < 0 {
		return full
	}
	return relPath(full[:i], b.dir) + full[i:]
}

func (b *jsonBuilder) report(txs []*Tx) *jsonReport {
	out := &jsonReport{Limit: MutationLimit, Transactions: []jsonTx{}, LoadErrors: []LoadError{}}
	for _, tx := range txs {
		jt := b.tx(tx)
		out.Transactions = append(out.Transactions, jt)
		out.Summary.Total++
		switch jt.Verdict {
		case "fixed":
			out.Summary.Fixed++
		case "dynamic":
			out.Summary.Dynamic++
		case "over":
			out.Summary.Over++
		case "check":
			out.Summary.Check++
		}
		if len(jt.Notes) > 0 {
			out.Summary.WithNotes++
		}
	}
	return out
}

func (b *jsonBuilder) tx(tx *Tx) jsonTx {
	s := tx.Summary
	names := newSymNames(append([]*Node{s.Tree}, s.Stmts...))
	jt := jsonTx{
		Func:        funcLabel(tx.Encl),
		FuncFull:    tx.Encl.String(),
		Pos:         fmt.Sprintf("%s:%d", relPath(tx.Pos.Filename, b.dir), tx.Pos.Line),
		Verdict:     verdictKeys[verdict(s)],
		CheckReason: s.ZeroReason(),
		Commit:      jsonCostOf(s.Cost, names),
		DML:         []jsonStmt{},
		Symbols:     []jsonSym{},
		Notes:       []string{},
		Callers:     []jsonCaller{},
	}
	for _, c := range tx.Callers {
		jt.Callers = append(jt.Callers, jsonCaller{Pos: b.pos(c.Pos), Func: c.Func})
	}
	for _, n := range s.Stmts {
		st := jsonStmt{Cost: jsonCostOf(n.Total(), names)}
		if w := firstWrite(n); w != nil {
			st.Op, st.Table, st.Pos = w.Label, w.Table, b.pos(w.Pos)
		}
		if b.verbose {
			st.Tree = b.node(n, names)
		}
		jt.DML = append(jt.DML, st)
	}
	for _, sym := range names.order {
		desc, pos := describeSym(sym)
		jt.Symbols = append(jt.Symbols, jsonSym{Name: names.name(sym), Desc: desc, Pos: b.pos(pos)})
	}
	for n := range s.Notes {
		jt.Notes = append(jt.Notes, n)
	}
	sort.Strings(jt.Notes)
	if b.verbose {
		jt.Tree = b.node(s.Tree, names)
	}
	return jt
}

func jsonCostOf(c Cost, names *symNames) jsonCost {
	limits, _ := c.LimitLines(MutationLimit, names.name, baseSym)
	if limits == nil {
		limits = []string{}
	}
	jc := jsonCost{
		Expr:     c.Format(names.name),
		Const:    c.Constant(),
		Terms:    []jsonTerm{},
		Headroom: MutationLimit - c.Constant(),
		Limits:   limits,
	}
	for _, k := range c.Keys() {
		t := jsonTerm{Coef: c[k]}
		for _, sym := range strings.Split(k, "·") {
			t.Syms = append(t.Syms, names.name(sym))
		}
		if jc.Headroom > 0 {
			t.Max = jc.Headroom / c[k]
		}
		jc.Terms = append(jc.Terms, t)
	}
	return jc
}

// firstWrite は n の中で最初の書き込みを返す。DML の文の op・テーブル・位置に使う。
func firstWrite(n *Node) *Node {
	if n == nil {
		return nil
	}
	if n.Kind == NodeWrite {
		return n
	}
	for _, c := range n.Children {
		if w := firstWrite(c); w != nil {
			return w
		}
	}
	return nil
}

var nodeKindNames = map[NodeKind]string{
	NodeWrite: "write",
	NodeSeq:   "seq",
	NodeAlt:   "alt",
	NodeCall:  "call",
	NodeLoop:  "loop",
}

func (b *jsonBuilder) node(n *Node, names *symNames) *jsonNode {
	if n == nil {
		return nil
	}
	jn := &jsonNode{
		Kind:     nodeKindNames[n.Kind],
		Label:    n.Label,
		Pos:      b.pos(n.Pos),
		Table:    n.Table,
		Detail:   n.Detail,
		Branches: n.AltLabels,
		Optional: n.Optional,
		Total:    n.total.Format(names.name),
	}
	if n.Kind == NodeWrite {
		jn.Unit = n.Unit.Format(names.name)
	}
	if n.Sym != "" {
		jn.Sym = names.name(n.Sym)
	}
	for _, c := range n.Children {
		jn.Children = append(jn.Children, b.node(c, names))
	}
	return jn
}

func jsonCoverageOf(unreached, outside []WriteSite, dir string) *jsonCoverage {
	site := func(s WriteSite) jsonSite {
		js := jsonSite{
			Pos:  fmt.Sprintf("%s:%d", relPath(s.Pos.Filename, dir), s.Pos.Line),
			Func: shortFunc(s.Fn.String()),
			Call: s.Call,
			Hint: s.Hint,
		}
		if s.Cost != nil {
			js.Cost = s.Cost.Cost.ShortString()
		}
		return js
	}
	out := &jsonCoverage{Unreached: []jsonSite{}, Outside: []jsonSite{}}
	for _, s := range unreached {
		out.Unreached = append(out.Unreached, site(s))
	}
	for _, s := range outside {
		out.Outside = append(out.Outside, site(s))
	}
	return out
}

// -tables の JSON

type jsonTables struct {
	Schemas []jsonSchema `json:"schemas"`
	Missing []string     `json:"missing"`
}

type jsonSchema struct {
	Path   string      `json:"path"`
	Tables []jsonTable `json:"tables"`
}

type jsonTable struct {
	Name    string `json:"name"`
	Columns int    `json:"columns"`
	Indexes int    `json:"indexes"`
	FKs     int    `json:"fks"`
	Insert  int    `json:"insert"`
	Update  int    `json:"update"`
	Delete  int    `json:"delete"`
	// Cascade は ON DELETE CASCADE で消える子。親の削除に、子の行1行あたり PerRow が足される
	Cascade []jsonCascade `json:"cascade"`
}

type jsonCascade struct {
	Table   string        `json:"table"`
	PerRow  int           `json:"per_row"`
	Sym     string        `json:"sym"`
	Cascade []jsonCascade `json:"cascade"`
}

func buildJSONTables(s *Schema, arg string) *jsonTables {
	rows, missing := selectTables(s, arg)
	out := &jsonTables{Schemas: []jsonSchema{}, Missing: missing}
	if out.Missing == nil {
		out.Missing = []string{}
	}
	var cascade func(t *Table) []jsonCascade
	cascade = func(t *Table) []jsonCascade {
		cs := []jsonCascade{}
		walkCascade(t, 1, func(c, parent *Table, depth int) {
			if depth == 1 {
				cs = append(cs, jsonCascade{
					Table:   c.Name,
					PerRow:  c.DeleteIndexCost(),
					Sym:     fmt.Sprintf("rows(%s←%s)", c.Name, parent.Name),
					Cascade: cascade(c),
				})
			}
		})
		return cs
	}
	for _, path := range s.Paths {
		js := jsonSchema{Path: path, Tables: []jsonTable{}}
		for _, t := range rows {
			if t.Source != path {
				continue
			}
			js.Tables = append(js.Tables, jsonTable{
				Name:    t.Name,
				Columns: len(t.Columns),
				Indexes: len(t.Indexes),
				FKs:     t.FKCount,
				Insert:  t.InsertCost(nil),
				Update:  t.UpdateCost(nil),
				Delete:  1 + t.DeleteIndexCost(),
				Cascade: cascade(t),
			})
		}
		if len(js.Tables) > 0 {
			out.Schemas = append(out.Schemas, js)
		}
	}
	return out
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
