package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/cloudspannerecosystem/memefish"
	memeast "github.com/cloudspannerecosystem/memefish/ast"
)

// omniResult は omnicheck ライブラリが Spanner Omni 上で記録した1ケース分の結果。omnicheck/omnicheck.go の Result と同じ形。
type omniResult struct {
	Name      string         `json:"name"`
	Func      string         `json:"func"`
	Symbols   map[string]int `json:"symbols"`
	Mutations []omniMutation `json:"mutations"`
	DML       []omniDML      `json:"dml"`
	Error     string         `json:"error,omitempty"`
	// MutationCount は CommitStats の実測値
	MutationCount int64 `json:"mutation_count"`
}

type omniMutation struct {
	Op      string   `json:"op"`
	Table   string   `json:"table"`
	Columns []string `json:"columns,omitempty"`
	Rows    int      `json:"rows,omitempty"`
	Keys    int      `json:"keys,omitempty"`
	Ranges  int      `json:"ranges,omitempty"`
}

type omniDML struct {
	SQL  string `json:"sql"`
	Rows int64  `json:"rows"`
}

type omniVerdict struct {
	Name string
	// Commit 側（Mutation API）
	Static string // 静的な式
	Eval   int    // 静的な式に実際の件数を代入した値
	Actual int    // 実際に送られた mutation から同じ単価で計算した値
	// DML 1 文あたりの最大値
	StaticDML string
	EvalDML   int
	ActualDML int
	Details   []string
	Err       string
	// Measured は CommitStats の実測値（Commit と全 DML の合計）
	Measured int
	// DMLCount は実行した DML の文の数。実測値は全 DML の合計なので、文が2つ以上だと DML 1 文あたりの最大値とは比べられない
	DMLCount int
}

// staticTotal は静的な式から見積もった、CommitStats と比べられる合計。
func (v omniVerdict) staticTotal() int { return v.Eval + v.EvalDML }

// formulaTotal は実際に送られた mutation と DML から、単価の計算式で計算した合計。
func (v omniVerdict) formulaTotal() int { return v.Actual + v.ActualDML }

// Status は突き合わせの結果。静的な式は上限として見積もるので、実際より大きいのは許容する。
func (v omniVerdict) Status() string {
	if v.Err != "" {
		return "ERR"
	}
	if v.DMLCount <= 1 {
		switch {
		case v.staticTotal() == v.Measured:
			return "一致"
		case v.staticTotal() > v.Measured:
			return "上限内"
		}
		return "過小評価"
	}
	// 実測値は全 DML の合計なので、DML 1 文あたりの最大値と比べられない。実際の書き込みから計算した値と比べる
	if v.Eval < v.Actual || v.EvalDML < v.ActualDML {
		return "過小評価"
	}
	if v.Eval > v.Actual || v.EvalDML > v.ActualDML {
		return "上限内"
	}
	return "一致"
}

func loadOmniResults(path string) ([]omniResult, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rs []omniResult
	return rs, json.Unmarshal(b, &rs)
}

// OmniCheck は Spanner Omni での実測値・実際の書き込みと、静的な式を突き合わせる。BuildImplIndex のあとに呼ぶ。
func (a *Analyzer) OmniCheck(results []omniResult) []omniVerdict {
	byName := map[string]bool{}
	for _, fn := range a.funcs {
		byName[fn.String()] = true
	}
	var out []omniVerdict
	for _, r := range results {
		v := omniVerdict{Name: r.Name}
		if r.Error != "" {
			v.Err = "Spanner Omni での実行に失敗: " + r.Error
			out = append(out, v)
			continue
		}
		var target *Summary
		for _, fn := range a.funcs {
			if fn.String() == r.Func {
				target = a.summarize(fn)
				break
			}
		}
		if target == nil {
			v.Err = "関数が見つからない: " + r.Func
			out = append(out, v)
			continue
		}
		v.Static, v.StaticDML = target.Cost.ShortString(), target.DML.ShortString()
		v.DMLCount = len(r.DML)
		v.Measured = int(r.MutationCount)

		syms := map[string]int{}
		for k, n := range r.Symbols {
			syms[k] = n
		}
		actual, actualDML, details, dmlRows := a.actualCost(r)
		v.Actual, v.ActualDML, v.Details = actual, actualDML, details
		for t, n := range dmlRows {
			syms["rows("+t+")"] += n
		}
		var missing []string
		v.Eval, missing = evalCost(target.Cost, syms)
		var missingDML []string
		v.EvalDML, missingDML = evalCost(target.DML, syms)
		for _, m := range append(missing, missingDML...) {
			if strings.Contains(m, "←") {
				// CASCADE で消える子の行数は、記録した書き込みからは分からない
				v.Details = append(v.Details, "動的件数 "+m+" は値が分からないので 0 として評価")
				continue
			}
			v.Err = "動的件数 " + m + " の値がない"
		}
		out = append(out, v)
	}
	return out
}

// evalCost は記号（@位置を除いた名前）に値を代入して式を評価する。値のない記号は 0 とし、名前を返す。
func evalCost(c Cost, syms map[string]int) (int, []string) {
	total := 0
	var missing []string
	for key, coef := range c {
		term := coef
		if key != "" {
			for _, sym := range strings.Split(key, "·") {
				name := sym
				if i := strings.Index(sym, "@"); i >= 0 {
					name = sym[:i]
				}
				n, ok := syms[name]
				if !ok {
					missing = append(missing, name)
				}
				term *= n
			}
		}
		total += term
	}
	sort.Strings(missing)
	return total, missing
}

// actualCost は実際に送られた mutation と DML を、静的解析と同じ単価で数える。
// 戻り値は Commit 側の合計、DML 1 文あたりの最大値、内訳、DML のテーブルごとの影響行数。
func (a *Analyzer) actualCost(r omniResult) (int, int, []string, map[string]int) {
	total, dmlMax := 0, 0
	var details []string
	dmlRows := map[string]int{}
	for _, m := range r.Mutations {
		ts := a.schema.Lookup(m.Table)
		if len(ts) == 0 {
			details = append(details, "schema にないテーブル: "+m.Table)
			continue
		}
		t := ts[0]
		var n int
		switch m.Op {
		case "insert_or_update", "replace":
			n = m.Rows * t.UpsertCost(m.Columns)
		case "insert":
			n = m.Rows * t.InsertCost(m.Columns)
			if missing := missingWritable(t, m.Columns); m.Op == "insert" && len(missing) > 0 {
				details = append(details, fmt.Sprintf("%s %s: 書き込んでいない列 %v（静的解析は全列を書く前提）", m.Op, m.Table, missing))
			}
		case "update":
			n = m.Rows * t.UpdateCost(m.Columns)
		case "delete":
			n = m.Keys * (1 + t.DeleteIndexCost())
			if m.Ranges > 0 {
				details = append(details, fmt.Sprintf("delete %s: 範囲削除 %d 件は行数が分からないので未計上", m.Table, m.Ranges))
			}
		}
		details = append(details, fmt.Sprintf("%s %s ×%d = %d", m.Op, m.Table, max(m.Rows, m.Keys), n))
		total += n
	}
	for _, d := range r.DML {
		table, per, err := a.dmlPerRow(d.SQL)
		if err != nil {
			details = append(details, "DML: "+err.Error())
			continue
		}
		n := per * int(d.Rows)
		dmlRows[table] += int(d.Rows)
		details = append(details, fmt.Sprintf("DML %s %d行 × %d = %d", table, d.Rows, per, n))
		dmlMax = max(dmlMax, n)
	}
	return total, dmlMax, details, dmlRows
}

func missingWritable(t *Table, cols []string) []string {
	set := map[string]bool{}
	for _, c := range cols {
		set[strings.ToLower(c)] = true
	}
	var missing []string
	for _, c := range t.WritableColumns() {
		if !set[strings.ToLower(c)] {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	return missing
}

// dmlPerRow は DML の対象テーブルと、1行あたりの mutation 数を返す。
func (a *Analyzer) dmlPerRow(sql string) (string, int, error) {
	dml, err := memefish.ParseDML("", sql)
	if err != nil {
		return "", 0, err
	}
	lookup := func(p *memeast.Path) (*Table, error) {
		ts := a.schema.Lookup(pathName(p))
		if len(ts) == 0 {
			return nil, fmt.Errorf("schema にないテーブル: %s", pathName(p))
		}
		return ts[0], nil
	}
	switch d := dml.(type) {
	case *memeast.Insert:
		t, err := lookup(d.TableName)
		if err != nil {
			return "", 0, err
		}
		var cols []string
		for _, c := range d.Columns {
			cols = append(cols, c.Name)
		}
		return t.Name, t.DMLInsertCost(cols), nil
	case *memeast.Update:
		t, err := lookup(d.TableName)
		if err != nil {
			return "", 0, err
		}
		var cols []string
		for _, u := range d.Updates {
			cols = append(cols, u.Path[len(u.Path)-1].Name)
		}
		return t.Name, t.DMLUpdateCost(cols), nil
	case *memeast.Delete:
		t, err := lookup(d.TableName)
		if err != nil {
			return "", 0, err
		}
		return t.Name, 1 + t.DeleteIndexCost(), nil
	}
	return "", 0, fmt.Errorf("未対応の DML: %s", sql)
}

func reportOmniCheck(vs []omniVerdict) {
	counts := map[string]int{}
	for _, v := range vs {
		counts[v.Status()]++
		fmt.Printf("[%s]  %s\n", v.Status(), v.Name)
		if v.Err != "" {
			fmt.Printf("    ! %s\n", v.Err)
			continue
		}
		fmt.Printf("    Commit: 静的 %s → %d / 実際 %d\n", v.Static, v.Eval, v.Actual)
		if v.StaticDML != "0" || v.ActualDML != 0 {
			fmt.Printf("    DML 1文あたり最大: 静的 %s → %d / 実際 %d\n", v.StaticDML, v.EvalDML, v.ActualDML)
		}
		formula := "計算式は実測と一致"
		if v.formulaTotal() != v.Measured {
			formula = fmt.Sprintf("⚠ 計算式が実測とずれている（実際の書き込みから計算 %d）", v.formulaTotal())
		}
		fmt.Printf("    実測 CommitStats: %d / 静的の合計 %d  （%s）\n", v.Measured, v.staticTotal(), formula)
		if v.DMLCount > 1 {
			fmt.Println("    ※ DML が2文以上あるので、判定は実際の書き込みとの比較で行った")
		}
		for _, d := range v.Details {
			fmt.Printf("      %s\n", d)
		}
	}
	var measured, formulaOK int
	for _, v := range vs {
		if v.Err == "" {
			measured++
			if v.formulaTotal() == v.Measured {
				formulaOK++
			}
		}
	}
	fmt.Printf("\n%d 件中 一致 %d 件 / 上限内 %d 件 / 過小評価 %d 件 / ERR %d 件\n", len(vs), counts["一致"], counts["上限内"], counts["過小評価"], counts["ERR"])
	if measured > 0 {
		fmt.Printf("計算式の検証（実際の書き込みから計算した値 = CommitStats）: %d 件中 %d 件一致\n", measured, formulaOK)
	}
}
