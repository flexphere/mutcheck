// mutcheck は Go のコードから Spanner の ReadWriteTransaction を辿り、
// トランザクションごとの mutation 数を schema.sql から見積もるツール。
package main

import (
	"flag"
	"fmt"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

func main() {
	// 一度きりの実行で読み込んだ型・SSA は最後まで使うので、GC の回数を減らした方が速い（数百パッケージのモジュールで 5% ほど速くなり、メモリは 2 割ほど増える）。
	// GOGC が指定されていればそちらに従う
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(400)
	}
	schemaPaths := flag.String("schema", "", "schema.sql のパス（必須）。DB が複数ある場合はカンマ区切りで指定する")
	dir := flag.String("dir", ".", "解析対象の Go モジュールのディレクトリ")
	funcPat := flag.String("func", "", "トランザクションを含む関数名を正規表現で絞り込む")
	filePat := flag.String("file", "", "トランザクションのあるファイルパスを部分一致で絞り込む")
	verbose := flag.Bool("v", false, "内訳（書き込みごとの単価と回数）・単価の内訳・注記（未追跡の呼び出しなど）を表示する")
	debug := flag.Bool("debug", false, "計測結果（各段階の所要時間）を stderr に出す")
	dumpPat := flag.String("dump", "", "デバッグ用: 正規表現に一致する関数の SSA を出力する")
	coverage := flag.Bool("coverage", false, "どのトランザクションからも辿り着かない書き込み箇所と、ReadWriteTransaction の外でのコミットを一覧にする")
	omniPath := flag.String("omnicheck", "", "omnicheck ライブラリが出力した JSON を読み、Spanner Omni での CommitStats の実測値・実際の書き込みと静的な式を突き合わせる")
	color := flag.String("color", "auto", "色を付けるか: auto（標準出力がターミナルで NO_COLOR がないとき）/ always / never")
	tables := flag.String("tables", "", "指定したテーブル（カンマ区切り）の単価を表示して終了する。all なら全テーブル")
	format := flag.String("format", "text", "出力形式: text / json（AI Agent や CI から読むとき）")
	failOn := flag.String("fail-on", "", "指定した判定（カンマ区切り。fixed / dynamic / over / check）のトランザクションがあれば終了コード 3 で終わる")
	showVersion := flag.Bool("version", false, "バージョンを表示して終了する")
	flag.Parse()
	if *showVersion {
		fmt.Println(version())
		return
	}
	if *color != "auto" && *color != "always" && *color != "never" {
		fmt.Fprintln(os.Stderr, "-color は auto / always / never のどれかを指定してください")
		os.Exit(2)
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintln(os.Stderr, "-format は text / json のどちらかを指定してください")
		os.Exit(2)
	}
	failVerdicts, err := parseFailOn(*failOn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if *schemaPaths == "" {
		fmt.Fprintln(os.Stderr, "-schema は必須です")
		os.Exit(2)
	}
	var funcRe *regexp.Regexp
	if *funcPat != "" {
		funcRe = regexp.MustCompile(*funcPat)
	}

	timer := newTimer()
	// 計測結果は -debug のときだけ出す。nil の timer は lap も print も何もしない
	if !*debug {
		timer = nil
	}

	schema, err := LoadSchemas(strings.Split(*schemaPaths, ","))
	if err != nil {
		fatal(err)
	}
	timer.lap("schema の解析", fmt.Sprintf("%d テーブル", len(schema.Tables)))
	if *tables != "" {
		if *format == "json" {
			if err := writeJSON(os.Stdout, buildJSONTables(schema, *tables)); err != nil {
				fatal(err)
			}
		} else {
			printTables(os.Stdout, schema, *tables, newPalette(*color))
		}
		timer.print()
		return
	}

	a, err := loadModule(*dir, schema, timer)
	if err != nil {
		fatal(err)
	}
	if *format != "json" {
		printLoadErrors(a.LoadErrors)
	}
	if *dumpPat != "" {
		re := regexp.MustCompile(*dumpPat)
		for fn := range ssautil.AllFunctions(a.prog) {
			if re.MatchString(fn.String()) {
				if _, err := fn.WriteTo(os.Stdout); err != nil {
					fatal(err)
				}
			}
		}
		return
	}
	a.BuildImplIndex()
	if *omniPath != "" {
		results, err := loadOmniResults(*omniPath)
		if err != nil {
			fatal(err)
		}
		reportOmniCheck(a.OmniCheck(results))
		timer.lap("Omni との突き合わせ", fmt.Sprintf("%d ケース", len(results)))
		timer.print()
		return
	}
	txs := a.FindTransactions()
	timer.lap("索引の構築・トランザクション境界の検出", fmt.Sprintf("トランザクション %d 件, 関数 %d 件, 具象型 %d 件", len(txs), len(a.funcs), len(a.candidates)))

	var targets []*Tx
	for _, tx := range txs {
		shown := (funcRe == nil || funcRe.MatchString(tx.Encl.String())) &&
			(*filePat == "" || strings.Contains(tx.Pos.Filename, *filePat))
		// 網羅チェックは全トランザクションの解析結果が必要なので、表示しないものも解析する
		if shown || *coverage {
			a.Analyze(tx)
		}
		if shown {
			if *format == "json" {
				tx.Callers = a.TxCallers(tx)
			}
			targets = append(targets, tx)
		}
	}
	timer.lap("mutation 数の算出", fmt.Sprintf("対象 %d 件, 解析した関数 %d 件", len(targets), len(a.summaries)))

	if *format == "json" {
		out := (&jsonBuilder{dir: *dir, verbose: *verbose}).report(targets)
		out.LoadErrors = a.LoadErrors
		if *coverage {
			unreached, outside := a.Coverage()
			timer.lap("網羅チェック", fmt.Sprintf("未到達 %d 件, トランザクション外のコミット %d 件", len(unreached), len(outside)))
			out.Coverage = jsonCoverageOf(unreached, outside, *dir)
		}
		if err := writeJSON(os.Stdout, out); err != nil {
			fatal(err)
		}
	} else {
		report(os.Stdout, targets, *dir, *verbose, newPalette(*color))
		if *coverage {
			unreached, outside := a.Coverage()
			timer.lap("網羅チェック", fmt.Sprintf("未到達 %d 件, トランザクション外のコミット %d 件", len(unreached), len(outside)))
			reportCoverage(unreached, outside, *dir)
		}
	}
	timer.print()
	if failed := failedTxs(targets, failVerdicts); failed > 0 {
		// JSON では件数を summary から読めるので、stdout と stderr を混ぜて読まれても壊れないようにメッセージは出さない
		if *format != "json" {
			fmt.Fprintf(os.Stderr, "-fail-on %s に当たるトランザクションが %d 件\n", *failOn, failed)
		}
		os.Exit(exitFailOn)
	}
}

// version は go install で入れたときのモジュールのバージョン（タグがなければ擬似バージョン）を返す。
// ソースからビルドしたときは (devel) と、分かればコミットを返す。
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	v := info.Main.Version
	if v != "" && v != "(devel)" {
		return v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return "(devel) " + s.Value
		}
	}
	return "(devel)"
}

// exitFailOn は -fail-on に当たるトランザクションがあったときの終了コード。引数の誤り（2）・読み込みの失敗（1）と区別する
const exitFailOn = 3

// parseFailOn は -fail-on の値を、判定（固定・件数次第など）の集合にする。
func parseFailOn(arg string) (map[string]bool, error) {
	out := map[string]bool{}
	if arg == "" {
		return out, nil
	}
	byKey := map[string]string{}
	for tag, key := range verdictKeys {
		byKey[key] = tag
	}
	for _, k := range strings.Split(arg, ",") {
		tag, ok := byKey[strings.TrimSpace(k)]
		if !ok {
			return nil, fmt.Errorf("-fail-on に指定できるのは fixed / dynamic / over / check です: %s", k)
		}
		out[tag] = true
	}
	return out, nil
}

// failedTxs は判定が verdicts のどれかに当たるトランザクションの数を返す。
func failedTxs(txs []*Tx, verdicts map[string]bool) int {
	n := 0
	for _, tx := range txs {
		if verdicts[verdict(tx.Summary)] {
			n++
		}
	}
	return n
}

// loadModule は dir のモジュール全体を読み込み、SSA を構築して Analyzer を返す。
// interface の実装が読み込み範囲から漏れると黙って 0 になるので、常にモジュール全体を読み込む。
func loadModule(dir string, schema *Schema, timer *timer) (*Analyzer, error) {
	pkgs, err := loadPackages(dir)
	if err != nil {
		return nil, err
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("%s にパッケージがありません", dir)
	}
	loadErrs := loadErrors(pkgs, dir)
	timer.lap("パッケージの読み込み・型チェック", fmt.Sprintf("%d パッケージ, エラー %d 件", len(pkgs), len(loadErrs)))

	modPrefix := ""
	for _, p := range pkgs {
		if p.Module != nil {
			modPrefix = p.Module.Path
			break
		}
	}
	prog := buildProgram(pkgs)
	timer.lap("SSA の構築", "module: "+modPrefix)
	a := NewAnalyzer(prog, schema, modPrefix)
	a.LoadErrors = loadErrs
	return a, nil
}

// LoadError はパッケージの読み込み・型チェックのエラー。
// 型チェックに失敗したパッケージは SSA にしない（buildProgram）ので、その中のトランザクションや書き込みは解析から漏れる。
type LoadError struct {
	Package string `json:"package"`
	// Pos は -dir からの相対パスと行・列。位置のないエラーでは空文字
	Pos string `json:"pos,omitempty"`
	Msg string `json:"message"`
}

// printLoadErrors は読み込み・型チェックのエラーを警告として stderr に出す。取りこぼしの手がかりになるので、-debug がなくても出す。
// JSON 出力では stdout と stderr を混ぜて読まれても壊れないように、ここではなく JSON の load_errors に入れる。
func printLoadErrors(errs []LoadError) {
	if len(errs) == 0 {
		return
	}
	pkgs := map[string]bool{}
	for _, e := range errs {
		pkgs[e.Package] = true
	}
	fmt.Fprintf(os.Stderr, "警告: %d パッケージで読み込み・型チェックのエラーが %d 件。エラーのあるパッケージは解析から漏れる\n", len(pkgs), len(errs))
	for _, e := range errs {
		pos := e.Pos
		if pos == "" {
			pos = e.Package
		}
		fmt.Fprintf(os.Stderr, "  %s: %s\n", pos, e.Msg)
	}
}

// loadErrors はパッケージの読み込み・型チェックのエラーを位置順に返す。
// 型エラーがあると go list も同じ内容をまとめたエラーを返すので、型エラーのあるパッケージでは型エラーだけを使う。
func loadErrors(pkgs []*packages.Package, dir string) []LoadError {
	errs := []LoadError{}
	for _, p := range pkgs {
		hasType := false
		for _, e := range p.Errors {
			hasType = hasType || e.Kind == packages.TypeError
		}
		for _, e := range p.Errors {
			if hasType && e.Kind != packages.TypeError {
				continue
			}
			pos := e.Pos
			if pos == "-" {
				pos = ""
			} else if i := strings.Index(pos, ".go:"); i >= 0 {
				pos = relPath(pos[:i+len(".go")], dir) + pos[i+len(".go"):]
			}
			errs = append(errs, LoadError{Package: p.PkgPath, Pos: pos, Msg: e.Msg})
		}
	}
	// パッケージ内は go/packages が返した順（位置順）のまま
	sort.SliceStable(errs, func(i, j int) bool { return errs[i].Package < errs[j].Package })
	return errs
}

// buildProgram は解析対象のパッケージをソースから、依存パッケージを型情報のみから SSA にする。
func buildProgram(pkgs []*packages.Package) *ssa.Program {
	prog := ssa.NewProgram(pkgs[0].Fset, ssa.InstantiateGenerics)
	created := map[*types.Package]bool{}
	for _, p := range pkgs {
		if p.Types != nil {
			created[p.Types] = true
		}
	}
	var createDep func(tp *types.Package)
	createDep = func(tp *types.Package) {
		if created[tp] {
			return
		}
		created[tp] = true
		for _, imp := range tp.Imports() {
			createDep(imp)
		}
		prog.CreatePackage(tp, nil, nil, true)
	}
	for _, p := range pkgs {
		if p.Types == nil || p.IllTyped {
			continue
		}
		for _, imp := range p.Types.Imports() {
			createDep(imp)
		}
	}
	for _, p := range pkgs {
		if p.Types == nil || p.IllTyped {
			continue
		}
		prog.CreatePackage(p.Types, p.Syntax, p.TypesInfo, false)
	}
	prog.Build()
	return prog
}

func reportCoverage(unreached, outside []WriteSite, dir string) {
	fmt.Printf("\n--- 網羅チェック: トランザクションから辿り着かない書き込み %d 件 ---\n", len(unreached))
	for _, s := range unreached {
		fmt.Printf("%s:%d  %s  %s\n    %s\n", relPath(s.Pos.Filename, dir), s.Pos.Line, shortFunc(s.Fn.String()), s.Call, s.Hint)
	}
	fmt.Printf("\n--- 網羅チェック: ReadWriteTransaction の外でのコミット %d 件（集計対象外） ---\n", len(outside))
	for _, s := range outside {
		cost := ""
		if s.Cost != nil {
			cost = "  = " + s.Cost.Cost.ShortString()
		}
		fmt.Printf("%s:%d  %s  %s%s\n", relPath(s.Pos.Filename, dir), s.Pos.Line, shortFunc(s.Fn.String()), s.Call, cost)
	}
}

// relPath は解析対象のモジュールのディレクトリからの相対パスを返す。
func relPath(path, dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return path
	}
	if rel, err := filepath.Rel(abs, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

func shortFunc(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

type lap struct {
	name, detail string
	d            time.Duration
}

type timer struct {
	start, last time.Time
	laps        []lap
}

func newTimer() *timer {
	now := time.Now()
	return &timer{start: now, last: now}
}

func (t *timer) lap(name, detail string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.laps = append(t.laps, lap{name, detail, now.Sub(t.last)})
	t.last = now
}

func (t *timer) print() {
	if t == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "\n--- 計測 ---")
	for _, l := range t.laps {
		fmt.Fprintf(os.Stderr, "%-36s %8.2fs  %s\n", l.name, l.d.Seconds(), l.detail)
	}
	fmt.Fprintf(os.Stderr, "%-36s %8.2fs\n", "合計", time.Since(t.start).Seconds())
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
