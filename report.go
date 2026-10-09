package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// 判定。見出しに付ける
const (
	tagFixed    = "固定"
	tagSymbolic = "件数次第"
	tagExceeded = "超過"
	tagZero     = "要確認"
)

// verdictKeys は判定の、JSON と -fail-on で使う名前。
var verdictKeys = map[string]string{
	tagFixed:    "fixed",
	tagSymbolic: "dynamic",
	tagExceeded: "over",
	tagZero:     "check",
}

// verdict はトランザクションの判定を返す。
func verdict(s *Summary) string {
	_, over := s.Cost.LimitLines(MutationLimit, plainSym, baseSym)
	for _, n := range s.Stmts {
		_, ex := n.Total().LimitLines(MutationLimit, plainSym, baseSym)
		over = over || ex
	}
	switch {
	case s.ZeroReason() != "":
		return tagZero
	case over:
		return tagExceeded
	case s.Cost.IsFixed() && s.DML.IsFixed():
		return tagFixed
	}
	return tagSymbolic
}

func plainSym(sym string) string { return sym }

func report(w io.Writer, txs []*Tx, dir string, verbose bool, pal palette) {
	counts := map[string]int{}
	var withNotes int
	var zeros []*Tx
	for _, tx := range txs {
		tag := reportTx(w, tx, dir, verbose, pal)
		counts[tag]++
		if tag == tagZero {
			zeros = append(zeros, tx)
		}
		if len(tx.Summary.Notes) > 0 {
			withNotes++
		}
	}
	fmt.Fprintf(w, "\n%d 件: 固定 %d 件 / 件数次第 %d 件 / 超過 %d 件 / 要確認 %d 件（注記あり %d 件）\n",
		len(txs), counts[tagFixed], counts[tagSymbolic], counts[tagExceeded], counts[tagZero], withNotes)
	if len(zeros) > 0 {
		fmt.Fprintln(w, "\n--- 要確認（mutation 数が 0） ---")
		for _, tx := range zeros {
			fmt.Fprintf(w, "%s:%d  %s\n    %s\n", relPath(tx.Pos.Filename, dir), tx.Pos.Line, funcLabel(tx.Encl), tx.Summary.ZeroReason())
		}
	}
}

// reportTx はトランザクション1件分を書き出し、判定を返す。
func reportTx(w io.Writer, tx *Tx, dir string, verbose bool, pal palette) string {
	s := tx.Summary
	names := newSymNames(append([]*Node{s.Tree}, s.Stmts...))
	// 記号は式・内訳・凡例のどこでも同じ色にする
	sym := func(x string) string { return pal.magenta(names.name(x)) }

	commitLines, _ := s.Cost.LimitLines(MutationLimit, sym, baseSym)
	var stmtHints [][]string
	for _, n := range s.Stmts {
		lines, _ := n.Total().LimitLines(MutationLimit, sym, baseSym)
		stmtHints = append(stmtHints, lines)
	}
	tag := verdict(s)

	fmt.Fprintf(w, "\n■ %s  %s\n", pal.bold(funcLabel(tx.Encl)), pal.tag(tag))
	fmt.Fprintf(w, "  %s:%d\n", relPath(tx.Pos.Filename, dir), tx.Pos.Line)

	r := &treeWriter{w: w, sym: sym, verbose: verbose, pal: pal}
	fmt.Fprintf(w, "\n  Commit  %s / %s\n", s.Cost.Format(sym), comma(MutationLimit))
	// 内訳は長くなるので -v のときだけ出す
	if verbose {
		r.node(s.Tree, 2, nil)
	}
	if len(commitLines) > 0 {
		fmt.Fprintln(w, "\n  上限内の条件")
		for _, l := range commitLines {
			fmt.Fprintf(w, "    %s\n", pal.boldBounds(l))
		}
	}

	if len(s.Stmts) > 0 {
		fmt.Fprintf(w, "\n  DML（1文ごとに上限 %s）\n", comma(MutationLimit))
		for i, n := range s.Stmts {
			r.node(n, 2, nil)
			for _, l := range stmtHints[i] {
				fmt.Fprintf(w, "      → %s\n", pal.boldBounds(l))
			}
		}
	}

	if syms := names.order; len(syms) > 0 {
		fmt.Fprintln(w, "\n  動的件数")
		width := 0
		for _, sym := range syms {
			width = max(width, displayWidth(names.name(sym)))
		}
		for _, sym := range syms {
			n := names.name(sym)
			desc, pos := describeSym(sym)
			fmt.Fprintf(w, "    %s%s  %s  %s\n", pal.magenta(n), strings.Repeat(" ", width-displayWidth(n)), desc, pal.dim(basePos(pos)))
		}
	}

	if r := s.ZeroReason(); r != "" {
		fmt.Fprintf(w, "\n  %s\n", pal.color(colorMagenta, "⚠ 要確認: "+r))
	}
	if verbose && len(s.Notes) > 0 {
		notes := make([]string, 0, len(s.Notes))
		for n := range s.Notes {
			notes = append(notes, n)
		}
		sort.Strings(notes)
		fmt.Fprintln(w, "\n  注記")
		for _, n := range notes {
			fmt.Fprintf(w, "    %s\n", pal.dim("! "+n))
		}
	}
	return tag
}

// treeWriter は内訳の木を字下げして書き出す。
type treeWriter struct {
	w       io.Writer
	sym     func(string) string
	verbose bool
	pal     palette
}

// node は n を depth 段の字下げで書き出す。mult は外側のループの記号で、書き込みの行に掛け算として出す。
func (r *treeWriter) node(n *Node, depth int, mult []string) {
	if n == nil {
		return
	}
	indent := strings.Repeat("  ", depth)
	switch n.Kind {
	case NodeSeq:
		for _, c := range n.Children {
			r.node(c, depth, mult)
		}
	case NodeLoop:
		r.node(n.Children[0], depth, append(append([]string{}, mult...), n.Sym))
	case NodeCall:
		// BufferWrite の位置は書き込みの位置とほぼ同じなので、段を増やさない
		if n.Label == "BufferWrite" {
			r.node(n.Children[0], depth, mult)
			return
		}
		fmt.Fprintf(r.w, "%s%s  %s\n", indent, n.Label, r.pal.dim(basePos(n.Pos)))
		r.node(n.Children[0], depth+1, mult)
	case NodeAlt:
		where := n.Label
		if n.Pos != "" {
			where += "  " + basePos(n.Pos)
		}
		cyan := func(s string) string { return r.pal.color(colorCyan, s) }
		if n.Optional && len(n.Children) == 1 {
			// 書き込むかどうかだけが分かれる場合。値の合流の「経路 N」はラベルとして意味がないので出さない
			fmt.Fprintf(r.w, "%s%s\n", indent, cyan("◇ 条件付き（"+where+"）"))
			if strings.HasPrefix(n.AltLabels[0], "経路") {
				r.node(n.Children[0], depth+1, mult)
				return
			}
			fmt.Fprintf(r.w, "%s  %s\n", indent, cyan("["+n.AltLabels[0]+"]"))
			r.node(n.Children[0], depth+2, mult)
			return
		}
		note := "どれか1つ。合計には項ごとに大きい方を使う"
		if n.Optional {
			note = "どれか1つ、または書き込みなし。合計には項ごとに大きい方を使う"
		}
		fmt.Fprintf(r.w, "%s%s\n", indent, cyan("◇ "+where+"（"+note+"）"))
		for i, c := range n.Children {
			fmt.Fprintf(r.w, "%s  %s\n", indent, cyan("["+n.AltLabels[i]+"]"))
			r.node(c, depth+2, mult)
		}
	case NodeWrite:
		unit := n.Unit.Format(r.sym)
		if len(n.Unit) > 1 && len(mult) > 0 {
			unit = "(" + unit + ")"
		}
		// 式に記号を含む書き込み（DML の影響行数など）は、ループの外なら「× 1」を付けない
		times := " × 1"
		if !n.Unit.IsFixed() {
			times = ""
		}
		if len(mult) > 0 {
			var ms []string
			for _, m := range mult {
				ms = append(ms, r.sym(m))
			}
			times = " × " + strings.Join(ms, "·")
		}
		fmt.Fprintf(r.w, "%s%s  %s  %s%s  %s\n", indent, r.pal.op(n.Label), r.pal.bold(n.Table), unit, times, r.pal.dim(basePos(n.Pos)))
		if r.verbose && n.Detail != "" {
			fmt.Fprintf(r.w, "%s  %s\n", indent, r.pal.dim("└ "+n.Detail))
		}
	}
}

// symNames はトランザクションごとの記号の短い名前。位置を除いた名前が同じ別の記号には #2、#3 を付ける。
type symNames struct {
	short map[string]string
	order []string
}

// newSymNames は内訳の木に出てくる順に記号の名前を決める。
func newSymNames(roots []*Node) *symNames {
	sn := &symNames{short: map[string]string{}}
	used := map[string]int{}
	add := func(sym string) {
		if _, ok := sn.short[sym]; ok {
			return
		}
		b := baseSym(sym)
		used[b]++
		name := b
		if used[b] > 1 {
			name = fmt.Sprintf("%s#%d", b, used[b])
		}
		sn.short[sym] = name
		sn.order = append(sn.order, sym)
	}
	var walk func(n *Node)
	walk = func(n *Node) {
		if n == nil {
			return
		}
		switch n.Kind {
		case NodeLoop:
			add(n.Sym)
		case NodeWrite:
			for _, k := range n.Unit.Keys() {
				for _, sym := range strings.Split(k, "·") {
					add(sym)
				}
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return sn
}

func (sn *symNames) name(sym string) string {
	if n, ok := sn.short[sym]; ok {
		return n
	}
	return sym
}

// splitSym は記号を位置を除いた名前と位置に分ける。
func splitSym(sym string) (name, pos string) {
	if i := strings.LastIndex(sym, "@"); i >= 0 {
		return sym[:i], sym[i+1:]
	}
	return sym, ""
}

// baseSym は記号の短い名前。len(x) は x、それ以外は位置を除いた名前。
func baseSym(sym string) string {
	name, _ := splitSym(sym)
	if strings.HasPrefix(name, "len(") && strings.HasSuffix(name, ")") {
		return name[len("len(") : len(name)-1]
	}
	return name
}

// describeSym は記号の凡例に出す説明と位置を返す。
func describeSym(sym string) (desc, pos string) {
	name, pos := splitSym(sym)
	inner := func(prefix string) string { return name[len(prefix) : len(name)-1] }
	switch {
	case strings.HasPrefix(name, "len("):
		desc = name + " 回のループ"
	case strings.HasPrefix(name, "rows(") && strings.Contains(name, "←"):
		parts := strings.SplitN(inner("rows("), "←", 2)
		desc = fmt.Sprintf("%s の削除で CASCADE 削除される %s の行数", parts[1], parts[0])
	case strings.HasPrefix(name, "rows("):
		desc = inner("rows(") + " の影響を受ける行数"
	case strings.HasPrefix(name, "calls("):
		desc = inner("calls(") + " が渡した関数を呼ぶ回数"
	case name == "for":
		desc = "for ループの回数"
	default:
		desc = name
	}
	return desc, pos
}

// displayWidth は等幅端末での表示幅の目安（全角を 2 とする）。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if r >= 0x2E80 {
			w += 2
		} else {
			w++
		}
	}
	return w
}
