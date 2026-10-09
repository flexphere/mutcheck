package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// printTables は -tables の表を書き出す。arg はカンマ区切りのテーブル名か all。
// schema ファイルごとに見出しを付け、ON DELETE CASCADE の子は親の行の下に字下げして出す。
func printTables(w io.Writer, s *Schema, arg string, pal palette) {
	rows, missing := selectTables(s, arg)

	// 外部キーを使っていない schema では 0 だけの列になるので、どのテーブルにもなければ出さない
	showFK := false
	width := len("table")
	cascadeWidth := 0
	for _, t := range rows {
		showFK = showFK || t.FKCount > 0
		width = max(width, len(t.Name))
		walkCascade(t, 1, func(c, _ *Table, depth int) {
			cascadeWidth = max(cascadeWidth, 2*depth+len(cascadeLabel+"  ")+len(c.Name))
		})
	}

	header := fmt.Sprintf("%-*s %8s %8s", width, "table", "columns", "indexes")
	if showFK {
		header += fmt.Sprintf(" %8s", "fks")
	}
	header += fmt.Sprintf(" %8s %8s %8s", "insert", "update", "delete")

	for _, path := range s.Paths {
		var ts []*Table
		for _, t := range rows {
			if t.Source == path {
				ts = append(ts, t)
			}
		}
		if len(ts) == 0 {
			continue
		}
		// 色は桁揃えのあとに付ける。単価は内訳の操作と同じ色（insert 緑 / update 黄 / delete 赤）
		fmt.Fprintf(w, "%s\n%s\n", pal.bold("=== "+path), pal.dim(header))
		for _, t := range ts {
			line := pal.bold(fmt.Sprintf("%-*s", width, t.Name)) + fmt.Sprintf(" %8d %8d", len(t.Columns), len(t.Indexes))
			if showFK {
				line += fmt.Sprintf(" %8d", t.FKCount)
			}
			line += " " + pal.color(colorGreen, fmt.Sprintf("%8d", t.InsertCost(nil))) +
				" " + pal.color(colorYellow, fmt.Sprintf("%8d", t.UpdateCost(nil))) +
				" " + pal.color(colorRed, fmt.Sprintf("%8d", 1+t.DeleteIndexCost()))
			fmt.Fprintln(w, line)
			walkCascade(t, 1, func(c, parent *Table, depth int) {
				indent := strings.Repeat("  ", depth)
				name := fmt.Sprintf("%-*s", cascadeWidth-len(indent)-len(cascadeLabel+"  "), c.Name)
				// 解析結果の動的件数と同じ rows(子←親) で書き、突き合わせられるようにする
				add := fmt.Sprintf("+%d × rows(%s←%s)", c.DeleteIndexCost(), c.Name, parent.Name)
				// 親の delete に足される分なので delete と同じ赤。足されない +0 は薄くする
				if c.DeleteIndexCost() == 0 {
					add = pal.dim(add)
				} else {
					add = pal.color(colorRed, add)
				}
				fmt.Fprintf(w, "%s%s  %s  %s\n", indent, pal.color(colorCyan, cascadeLabel), name, add)
			})
		}
		fmt.Fprintln(w)
	}
	for _, n := range missing {
		fmt.Fprintln(w, pal.color(colorMagenta, n+" (not found)"))
	}
}

// selectTables は -tables の引数（カンマ区切りのテーブル名か all）に当たるテーブルと、見つからない名前を返す。
func selectTables(s *Schema, arg string) (rows []*Table, missing []string) {
	if arg == "all" {
		return s.AllTables(), nil
	}
	for _, n := range strings.Split(arg, ",") {
		n = strings.TrimSpace(n)
		ts := s.Lookup(n)
		if len(ts) == 0 {
			missing = append(missing, n)
		}
		rows = append(rows, ts...)
	}
	return rows, missing
}

// cascadeLabel は -tables で子孫の行の先頭に付ける。schema.sql と同じ並びにして grep しやすくする
const cascadeLabel = "ON DELETE CASCADE"

// walkCascade は t の削除で ON DELETE CASCADE により消える子孫を、テーブル名順にたどる。depth は t の子が 1。
// 子の行そのものは数えず、子のインデックス（と外部キー）を子の行1行あたりで数える（cascadeCost と同じ）。
func walkCascade(t *Table, depth int, f func(c, parent *Table, depth int)) {
	children := append([]*Table{}, t.Children...)
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	for _, c := range children {
		if !c.Cascade {
			continue
		}
		f(c, t, depth)
		walkCascade(c, depth+1, f)
	}
}
