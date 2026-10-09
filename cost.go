package main

import (
	"fmt"
	"sort"
	"strings"
)

// Cost は mutation 数を記号つきの多項式で表す。
// キーは記号の積（"·" 区切り、ソート済み）で、空文字は定数項を表す。
type Cost map[string]int

func Const(n int) Cost {
	if n == 0 {
		return Cost{}
	}
	return Cost{"": n}
}

func (c Cost) Add(o Cost) {
	for k, v := range o {
		c[k] += v
	}
}

// Mul は全項に記号 sym を掛けたコストを返す。
func (c Cost) Mul(sym string) Cost {
	out := Cost{}
	for k, v := range c {
		out[joinSyms(k, sym)] += v
	}
	return out
}

func (c Cost) Scale(n int) Cost {
	out := Cost{}
	for k, v := range c {
		out[k] = v * n
	}
	return out
}

// Max は項ごとの最大値をとる。interface の実装が複数あるときの上限見積もりに使う。
func (c Cost) Max(o Cost) {
	for k, v := range o {
		if v > c[k] {
			c[k] = v
		}
	}
}

func (c Cost) Constant() int { return c[""] }

func (c Cost) IsFixed() bool {
	for k, v := range c {
		if k != "" && v != 0 {
			return false
		}
	}
	return true
}

func (c Cost) String() string { return c.Format(func(sym string) string { return sym }) }

// ShortString は String と同じ式を、記号の位置をファイル名と行だけにして返す。記号の位置はパス付きなので、表示にはこちらを使う。
func (c Cost) ShortString() string {
	return c.Format(func(sym string) string {
		name, pos := splitSym(sym)
		if pos == "" {
			return name
		}
		return name + "@" + basePos(pos)
	})
}

// Format は記号を name で置き換えて式を文字列にする。項の並びは String と同じ。
func (c Cost) Format(name func(sym string) string) string {
	if len(c) == 0 {
		return "0"
	}
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		di, dj := strings.Count(keys[i], "·"), strings.Count(keys[j], "·")
		if (keys[i] == "") != (keys[j] == "") {
			return keys[i] == ""
		}
		if di != dj {
			return di < dj
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			parts = append(parts, fmt.Sprint(c[k]))
			continue
		}
		var names []string
		for _, sym := range strings.Split(k, "·") {
			names = append(names, name(sym))
		}
		parts = append(parts, fmt.Sprintf("%d·%s", c[k], strings.Join(names, "·")))
	}
	return strings.Join(parts, " + ")
}

// Keys は式に出てくる項のキー（定数項を除く）を String と同じ順で返す。
func (c Cost) Keys() []string {
	var keys []string
	for k, v := range c {
		if k != "" && v != 0 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		di, dj := strings.Count(keys[i], "·"), strings.Count(keys[j], "·")
		if di != dj {
			return di < dj
		}
		return keys[i] < keys[j]
	})
	return keys
}

func joinSyms(a, b string) string {
	var syms []string
	if a != "" {
		syms = strings.Split(a, "·")
	}
	syms = append(syms, b)
	sort.Strings(syms)
	return strings.Join(syms, "·")
}

// MutationLimit は Commit で送る mutation の合計、および DML 1 文あたりの mutation 数の上限。
const MutationLimit = 80000

// LimitLines は式 c が上限 limit に収まる条件を、表示用の行で返す。exceeded は定数部分だけで上限を超えるとき。
// name は記号の表示名、base は同じ値とみなす記号をまとめるための名前（位置を除いた名前）。
//   - 項が1つなら、その項の記号（の積）がいくつまでなら上限内か
//   - 項が複数なら、記号の部分の不等式と、ほかの記号が 0 のときの各記号の上限（積でない項の記号だけ）
//   - 同じ名前の記号をまとめると記号が1つになるなら、その場合の上限（推定）
//
// 表示では記号を「動的件数」と呼ぶ
func (c Cost) LimitLines(limit int, name, base func(sym string) string) (lines []string, exceeded bool) {
	constant := c[""]
	if constant > limit {
		return []string{fmt.Sprintf("定数部分だけで上限 %s を超える", comma(limit))}, true
	}
	keys := c.Keys()
	if len(keys) == 0 {
		return nil, false
	}
	rest := limit - constant
	symName := func(k string) string { return strings.TrimPrefix(Cost{k: 1}.Format(name), "1·") }
	if len(keys) == 1 {
		return []string{fmt.Sprintf("%s ≤ %s", symName(keys[0]), comma(rest/c[keys[0]]))}, false
	}
	vars := Cost{}
	for _, k := range keys {
		vars[k] = c[k]
	}
	lines = append(lines, fmt.Sprintf("%s ≤ %s", vars.Format(name), comma(rest)))
	var each []string
	for _, k := range keys {
		if !strings.Contains(k, "·") {
			each = append(each, fmt.Sprintf("%s ≤ %s", name(k), comma(rest/c[k])))
		}
	}
	// 項が複数あるので、積でない項が1つでもあれば、ほかを 0 にしたときの上限に意味がある
	if len(each) > 0 {
		lines = append(lines, "ほかの動的件数が 0 のとき: "+strings.Join(each, " / "))
	}
	merged := Cost{}
	syms := map[string]bool{}
	for _, k := range keys {
		var bs []string
		for _, sym := range strings.Split(k, "·") {
			bs = append(bs, base(sym))
			syms[base(sym)] = true
		}
		sort.Strings(bs)
		merged[strings.Join(bs, "·")] += c[k]
	}
	if len(syms) == 1 && len(merged) == 1 {
		for k, v := range merged {
			if !strings.Contains(k, "·") {
				lines = append(lines, fmt.Sprintf("同じ名前の動的件数を同じ値とみなすと: %s ≤ %s（推定）", k, comma(rest/v)))
			}
		}
	}
	return lines, false
}

// comma は 3 桁ごとにカンマを入れる。
func comma(n int) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return "-" + comma(-n)
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}
