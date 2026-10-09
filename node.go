package main

import (
	"fmt"
	"hash/fnv"
	"sort"
)

// NodeKind は内訳の木の節の種類。
type NodeKind int

const (
	// NodeWrite は書き込み1つ（mutation の作成、DML の文）。
	NodeWrite NodeKind = iota
	// NodeSeq は順に実行される書き込みの並び。合計は足し算。
	NodeSeq
	// NodeAlt は排他的な分岐（if/else、switch、interface の実装、複数の return など）。合計は項ごとの最大値。
	NodeAlt
	// NodeCall は関数の呼び出し。合計は子と同じ。
	NodeCall
	// NodeLoop はループや繰り返し呼び出し。合計は子に記号を掛けたもの。
	NodeLoop
)

// Node は mutation 数の内訳の木。Summary.Cost はこの木から計算する。
// 解析結果は関数ごとにキャッシュされ、別の呼び出し元から共有されるので、作ったあとは書き換えない。
type Node struct {
	Kind NodeKind
	// Label は NodeWrite では操作（insert など）、NodeCall では関数名、NodeAlt では分岐の種類
	Label string
	// Pos は file:line
	Pos string
	// NodeWrite: 書き込み先のテーブルと1回あたりのコスト、単価の内訳
	Table  string
	Unit   Cost
	Detail string
	// NodeLoop: 掛ける記号
	Sym string
	// NodeAlt: 各分岐のラベル（Children と同じ並び）
	AltLabels []string
	// Optional は NodeAlt で、書き込みのない経路もあること
	Optional bool
	Children []*Node

	total Cost
	// hash は節の構造から計算する値。分岐の共通部分をくくり出すときに、同じ内容の節を見分けるのに使う
	hash uint64
}

func (n *Node) setHash() *Node {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%s|%s|%s|%s|%s|%s|%v|%v|", n.Kind, n.Label, n.Pos, n.Table, n.Unit, n.Detail, n.Sym, n.AltLabels, n.Optional)
	for _, c := range n.Children {
		fmt.Fprintf(h, "%x,", c.hash)
	}
	n.hash = h.Sum64()
	return n
}

// Total は節の合計。呼び出し側で書き換えてよいように複製を返す。
func (n *Node) Total() Cost {
	out := Cost{}
	if n != nil {
		out.Add(n.total)
	}
	return out
}

func (n *Node) isZero() bool {
	if n == nil {
		return true
	}
	for _, v := range n.total {
		if v != 0 {
			return false
		}
	}
	return true
}

func writeNode(op, table string, unit Cost, detail, pos string) *Node {
	if len(unit) == 0 {
		return nil
	}
	n := &Node{Kind: NodeWrite, Label: op, Table: table, Unit: unit, Detail: detail, Pos: pos}
	n.total = Cost{}
	n.total.Add(unit)
	return n.setHash()
}

// seqNode は子を順に並べた節を返す。合計が 0 の子は除き、子が1つならその子を返す。
func seqNode(children ...*Node) *Node {
	var kids []*Node
	for _, c := range children {
		if c.isZero() {
			continue
		}
		if c.Kind == NodeSeq {
			kids = append(kids, c.Children...)
			continue
		}
		kids = append(kids, c)
	}
	switch len(kids) {
	case 0:
		return nil
	case 1:
		return kids[0]
	}
	n := &Node{Kind: NodeSeq, Children: kids, total: Cost{}}
	for _, c := range kids {
		n.total.Add(c.total)
	}
	return n.setHash()
}

// altNode は排他的な分岐の節を返す。
// どの分岐にも同じ内容の節があれば分岐の外にくくり出す（max(c+a, c+b) = c + max(a, b) なので合計は変わらない）。
// 書き込みのない分岐は除いて Optional として残し、分岐が1つだけで書き込みのない経路もなければ、その子を返す。
func altNode(label, pos string, labels []string, children []*Node) *Node {
	var kids []*Node
	var ls []string
	optional := false
	for i, c := range children {
		if c.isZero() {
			optional = true
			continue
		}
		kids = append(kids, c)
		ls = append(ls, labels[i])
	}
	switch {
	case len(kids) == 0:
		return nil
	case len(kids) == 1 && !optional:
		return kids[0]
	}
	if !optional {
		if common, rest := factorCommon(kids); len(common) > 0 {
			return seqNode(append(common, altNode(label, pos, ls, rest))...)
		}
	}
	n := &Node{Kind: NodeAlt, Label: label, Pos: pos, AltLabels: ls, Optional: optional, Children: kids, total: Cost{}}
	for _, c := range kids {
		n.total.Max(c.total)
	}
	return n.setHash()
}

// factorCommon は、すべての分岐に含まれる同じ内容の節を共通部分として取り出し、残りを分岐ごとに返す。
func factorCommon(kids []*Node) (common []*Node, rest []*Node) {
	items := make([][]*Node, len(kids))
	for i, k := range kids {
		if k.Kind == NodeSeq {
			items[i] = k.Children
		} else {
			items[i] = []*Node{k}
		}
	}
	// 各分岐での出現回数の最小値だけ、共通部分としてくくり出せる
	counts := make([]map[uint64]int, len(kids))
	for i, it := range items {
		counts[i] = map[uint64]int{}
		for _, n := range it {
			counts[i][n.hash]++
		}
	}
	take := map[uint64]int{}
	for h, c := range counts[0] {
		for _, m := range counts[1:] {
			c = min(c, m[h])
		}
		if c > 0 {
			take[h] = c
		}
	}
	if len(take) == 0 {
		return nil, kids
	}
	left := map[uint64]int{}
	for h, c := range take {
		left[h] = c
	}
	for _, n := range items[0] {
		if left[n.hash] > 0 {
			common = append(common, n)
			left[n.hash]--
		}
	}
	for _, it := range items {
		skip := map[uint64]int{}
		for h, c := range take {
			skip[h] = c
		}
		var r []*Node
		for _, n := range it {
			if skip[n.hash] > 0 {
				skip[n.hash]--
				continue
			}
			r = append(r, n)
		}
		rest = append(rest, seqNode(r...))
	}
	return common, rest
}

// callNode は呼び出しの節を返す。位置のない呼び出しは、SSA が作ったラッパー（値レシーバのメソッドのポインタ版、
// メソッド値の $bound など）の中の呼び出しで、ソースにはないので段にせず中身をそのまま返す。
func callNode(label, pos string, child *Node) *Node {
	if child.isZero() {
		return nil
	}
	if pos == "" {
		return child
	}
	return (&Node{Kind: NodeCall, Label: label, Pos: pos, Children: []*Node{child}, total: child.Total()}).setHash()
}

func loopNode(sym string, child *Node) *Node {
	if child.isZero() {
		return nil
	}
	return (&Node{Kind: NodeLoop, Sym: sym, Children: []*Node{child}, total: child.total.Mul(sym)}).setHash()
}

func loopNodes(syms []string, child *Node) *Node {
	for _, s := range syms {
		child = loopNode(s, child)
	}
	return child
}

// sortedNodes は位置で並べた節の一覧を返す。
func sortedNodes(m map[int]*Node) []*Node {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	out := make([]*Node, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}
