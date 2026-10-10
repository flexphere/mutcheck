package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cloudspannerecosystem/memefish"
	memeast "github.com/cloudspannerecosystem/memefish/ast"
	"golang.org/x/tools/go/ssa"
)

const spannerPkg = "cloud.google.com/go/spanner"

type Summary struct {
	// Cost は Commit で送る mutation（Mutation API）の合計。1 コミットあたり 80,000 の上限がかかる。Tree から計算する
	Cost Cost
	// DML は DML 1 文あたりの mutation 数の最大値（項ごと）。DML は文ごとに 80,000 の上限がかかり、トランザクション全体では数えない。Stmts から計算する
	DML Cost
	// Tree は Commit 分の内訳
	Tree *Node
	// Stmts は DML の文ごとの内訳
	Stmts []*Node
	Notes map[string]bool
	// Writes は BufferWrite・DML などの書き込みの呼び出しを辿れたか。Cost が 0 のときに理由を区別するのに使う
	Writes bool
}

func newSummary() *Summary { return &Summary{Cost: Cost{}, DML: Cost{}, Notes: map[string]bool{}} }

// add は Commit 分の内訳に n を足す。
func (s *Summary) add(n *Node) {
	if n.isZero() {
		return
	}
	s.Tree = seqNode(s.Tree, n)
	s.Cost = s.Tree.Total()
}

// addStmt は DML の文を1つ加える。
func (s *Summary) addStmt(n *Node) {
	if n.isZero() {
		return
	}
	for _, m := range s.Stmts {
		if m == n {
			return
		}
	}
	s.Stmts = append(s.Stmts, n)
	s.DML.Max(n.total)
}

// merge は o の注記・DML の文などを s に取り込む。Commit 分の内訳は add で別に足す。
// DML は文ごとの上限なので、ループの回数を掛けたり足したりせず、文の一覧として持つ。
func (s *Summary) merge(o *Summary) {
	for _, n := range o.Stmts {
		s.addStmt(n)
	}
	for n := range o.Notes {
		s.Notes[n] = true
	}
	s.Writes = s.Writes || o.Writes
}

// sub は o を取り込み、Commit 分の内訳を f で包んで足す。
func (s *Summary) sub(o *Summary, f func(*Node) *Node) {
	s.merge(o)
	s.add(f(o.Tree))
}

// ZeroReason は mutation 数が 0 のときに要確認として出す理由を返す。0 でなければ空文字。
func (s *Summary) ZeroReason() string {
	for _, c := range []Cost{s.Cost, s.DML} {
		for _, v := range c {
			if v != 0 {
				return ""
			}
		}
	}
	if s.Writes {
		return "書き込みはあるが mutation 数が 0（値を辿れていない可能性）"
	}
	return "書き込みが見つからない（読み取りのみ、または追跡漏れ）"
}

type Tx struct {
	Pos     token.Position
	Encl    *ssa.Function
	Bodies  []*ssa.Function // 本体の候補。関数値で渡されて複数に解決された場合は最大値をとる
	Summary *Summary
	Callers []TxCaller // トランザクションを始める関数を直接呼んでいる箇所（JSON 出力用）
}

// TxCaller はトランザクションを始める関数の呼び出し元。Pos は「パス:行」
type TxCaller struct {
	Pos  string
	Func string
}

type loopInfo struct {
	start, end token.Pos
	sym        string
}

type Analyzer struct {
	prog      *ssa.Program
	schema    *Schema
	modPrefix string
	modPkgs   []*ssa.Package

	funcs       []*ssa.Function
	callers     map[*ssa.Function][]ssa.CallInstruction
	fieldStores map[*types.Var][]ssa.Value
	retSums     map[retKey]*Summary
	retBusy     map[retKey]bool
	reached     map[ssa.CallInstruction]bool     // トランザクションの解析で辿り着いた書き込み箇所
	invokes     map[string][]ssa.CallInstruction // interface 経由の呼び出しをメソッド名で引く

	summaries  map[*ssa.Function]*Summary
	inProgress map[*ssa.Function]bool
	loops      map[*ssa.Function][]loopInfo
	candidates []types.Type
	implCache  map[string][]*ssa.Function

	// LoadErrors はパッケージの読み込み・型チェックのエラー（loadModule が入れる）。エラーのあるパッケージは解析から漏れる
	LoadErrors []LoadError
}

func NewAnalyzer(prog *ssa.Program, schema *Schema, modPrefix string) *Analyzer {
	a := &Analyzer{
		prog:        prog,
		schema:      schema,
		modPrefix:   modPrefix,
		callers:     map[*ssa.Function][]ssa.CallInstruction{},
		fieldStores: map[*types.Var][]ssa.Value{},
		retSums:     map[retKey]*Summary{},
		retBusy:     map[retKey]bool{},
		reached:     map[ssa.CallInstruction]bool{},
		invokes:     map[string][]ssa.CallInstruction{},
		summaries:   map[*ssa.Function]*Summary{},
		inProgress:  map[*ssa.Function]bool{},
		loops:       map[*ssa.Function][]loopInfo{},
		implCache:   map[string][]*ssa.Function{},
	}
	for _, p := range prog.AllPackages() {
		if a.inModule(p.Pkg) {
			a.modPkgs = append(a.modPkgs, p)
		}
	}
	return a
}

func (a *Analyzer) inModule(p *types.Package) bool {
	if p == nil {
		return false
	}
	path := p.Path()
	return (path == a.modPrefix || strings.HasPrefix(path, a.modPrefix+"/")) && !isMockPkg(p)
}

func isMockPkg(p *types.Package) bool {
	return strings.Contains(p.Path(), "/mock") || strings.HasPrefix(p.Name(), "mock")
}

// BuildImplIndex は解析に使う索引を作る。
// interface 呼び出しの解決に使う具象型の一覧と、関数値・引数を辿るための呼び出し元とフィールド代入の索引。
func (a *Analyzer) BuildImplIndex() {
	a.funcs = a.moduleFuncs()
	for _, fn := range a.funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				switch instr := instr.(type) {
				case ssa.CallInstruction:
					if callee := instr.Common().StaticCallee(); callee != nil {
						a.callers[callee] = append(a.callers[callee], instr)
					}
					if c := instr.Common(); c.IsInvoke() {
						a.invokes[c.Method.Name()] = append(a.invokes[c.Method.Name()], instr)
					}
				case *ssa.Store:
					if fa, ok := instr.Addr.(*ssa.FieldAddr); ok {
						if v := fieldVar(fa.X.Type(), fa.Field); v != nil {
							a.fieldStores[v] = append(a.fieldStores[v], instr.Val)
						}
					}
				}
			}
		}
	}
	for _, p := range a.modPkgs {
		for _, m := range p.Members {
			t, ok := m.(*ssa.Type)
			if !ok {
				continue
			}
			T := t.Type()
			if types.IsInterface(T) {
				continue
			}
			if n, ok := T.(*types.Named); ok && n.TypeParams().Len() > 0 {
				continue
			}
			a.candidates = append(a.candidates, T, types.NewPointer(T))
		}
	}
}

// moduleFuncs はモジュール内の関数・メソッド・クロージャを列挙する。
func (a *Analyzer) moduleFuncs() []*ssa.Function {
	var out []*ssa.Function
	seen := map[*ssa.Function]bool{}
	var visit func(fn *ssa.Function)
	visit = func(fn *ssa.Function) {
		if fn == nil || seen[fn] {
			return
		}
		seen[fn] = true
		out = append(out, fn)
		for _, anon := range fn.AnonFuncs {
			visit(anon)
		}
	}
	for _, p := range a.modPkgs {
		for _, m := range p.Members {
			switch m := m.(type) {
			case *ssa.Function:
				visit(m)
			case *ssa.Type:
				for _, T := range []types.Type{m.Type(), types.NewPointer(m.Type())} {
					ms := a.prog.MethodSets.MethodSet(T)
					for i := 0; i < ms.Len(); i++ {
						fn := a.prog.MethodValue(ms.At(i))
						if fn != nil && fn.Pkg == p {
							visit(fn)
						}
					}
				}
			}
		}
	}
	return out
}

// FindTransactions はモジュール内の ReadWriteTransaction 呼び出しを列挙する。BuildImplIndex のあとに呼ぶ。
func (a *Analyzer) FindTransactions() []*Tx {
	var txs []*Tx
	for _, fn := range a.funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				ci, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				recv, name, ok := spannerCallee(ci.Common())
				if !ok || recv != "Client" || (name != "ReadWriteTransaction" && name != "ReadWriteTransactionWithOptions") {
					continue
				}
				tx := &Tx{Pos: a.prog.Fset.Position(ci.Pos()), Encl: fn}
				if args := ci.Common().Args; len(args) > 2 {
					tx.Bodies = a.funcCandidates(args[2], 0)
				}
				txs = append(txs, tx)
			}
		}
	}
	sort.Slice(txs, func(i, j int) bool {
		if txs[i].Pos.Filename != txs[j].Pos.Filename {
			return txs[i].Pos.Filename < txs[j].Pos.Filename
		}
		return txs[i].Pos.Line < txs[j].Pos.Line
	})
	return txs
}

func (a *Analyzer) Analyze(tx *Tx) {
	if len(tx.Bodies) == 0 {
		s := newSummary()
		s.Notes["トランザクション本体の関数を特定できない"] = true
		tx.Summary = s
		return
	}
	tx.Summary = a.maxOver("本体の候補", tx.Bodies, a.summarize)
}

// maxOver は候補の関数ごとの結果の、項ごとの最大値をとる。kind は分岐の種類として内訳に出す。
func (a *Analyzer) maxOver(kind string, fns []*ssa.Function, f func(*ssa.Function) *Summary) *Summary {
	out := newSummary()
	var labels []string
	var kids []*Node
	for _, fn := range fns {
		sub := f(fn)
		out.merge(sub)
		labels = append(labels, funcLabel(fn))
		kids = append(kids, sub.Tree)
	}
	out.add(altNode(kind, "", labels, kids))
	return out
}

func (a *Analyzer) summarize(fn *ssa.Function) *Summary {
	if s, ok := a.summaries[fn]; ok {
		return s
	}
	if a.inProgress[fn] {
		s := newSummary()
		s.Notes["再帰呼び出し(0として計算): "+fn.String()] = true
		return s
	}
	a.inProgress[fn] = true
	defer delete(a.inProgress, fn)

	s := newSummary()
	calls := map[token.Pos]*Node{}
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			ci, ok := instr.(ssa.CallInstruction)
			if !ok {
				continue
			}
			c := a.callCost(fn, ci)
			if c == nil {
				continue
			}
			n := c.Tree
			for _, l := range a.enclosingLoops(fn, ci.Pos()) {
				n = loopNode(l.sym, n)
			}
			s.merge(c)
			calls[ci.Pos()] = seqNode(calls[ci.Pos()], n)
		}
	}
	s.add(a.combineByBranches(fnBody(fn), calls))
	a.summaries[fn] = s
	return s
}

func fnBody(fn *ssa.Function) *ast.BlockStmt {
	root := fn.Syntax()
	if o := fn.Origin(); root == nil && o != nil {
		root = o.Syntax()
	}
	switch r := root.(type) {
	case *ast.FuncDecl:
		return r.Body
	case *ast.FuncLit:
		return r.Body
	}
	return nil
}

// combineByBranches は呼び出しごとの内訳を AST の構造に沿って合成する。
// 順に実行される文は足し、if/else・switch の case・select の case のように排他的な分岐は項ごとの最大値をとる。
// ソースのない関数や、どの文にも属さない位置の呼び出しは単純に足す。
func (a *Analyzer) combineByBranches(body *ast.BlockStmt, calls map[token.Pos]*Node) *Node {
	used := map[token.Pos]bool{}
	in := func(start, end token.Pos) *Node {
		m := map[int]*Node{}
		for p, c := range calls {
			if p.IsValid() && start <= p && p < end {
				m[int(p)] = c
				used[p] = true
			}
		}
		return seqNode(sortedNodes(m)...)
	}
	var stmt func(s ast.Stmt) *Node
	stmts := func(list []ast.Stmt) *Node {
		var out []*Node
		for _, s := range list {
			out = append(out, stmt(s))
		}
		return seqNode(out...)
	}
	clauses := func(kind string, at token.Pos, body *ast.BlockStmt) *Node {
		var labels []string
		var kids []*Node
		for _, cl := range body.List {
			switch cl := cl.(type) {
			case *ast.CaseClause:
				labels = append(labels, caseLabel(cl.List))
				kids = append(kids, seqNode(in(cl.Pos(), cl.Colon+1), stmts(cl.Body)))
			case *ast.CommClause:
				label := "default"
				if cl.Comm != nil {
					label = "case"
				}
				labels = append(labels, label)
				kids = append(kids, seqNode(in(cl.Pos(), cl.Colon+1), stmts(cl.Body)))
			}
		}
		return altNode(kind, a.filePos(at), labels, kids)
	}
	stmt = func(s ast.Stmt) *Node {
		switch s := s.(type) {
		case nil:
			return nil
		case *ast.BlockStmt:
			return stmts(s.List)
		case *ast.IfStmt:
			c := in(s.Pos(), s.Body.Pos())
			// else がなければ、書き込みのない else として扱う
			labels := []string{"if " + clip(types.ExprString(s.Cond)), "else"}
			kids := []*Node{stmt(s.Body), stmt(s.Else)}
			return seqNode(c, altNode("if", a.filePos(s.Pos()), labels, kids))
		case *ast.SwitchStmt:
			return seqNode(in(s.Pos(), s.Body.Lbrace), clauses("switch", s.Pos(), s.Body))
		case *ast.TypeSwitchStmt:
			return seqNode(in(s.Pos(), s.Body.Lbrace), clauses("switch", s.Pos(), s.Body))
		case *ast.SelectStmt:
			return seqNode(in(s.Pos(), s.Body.Lbrace), clauses("select", s.Pos(), s.Body))
		case *ast.ForStmt:
			return seqNode(in(s.Pos(), s.Body.Pos()), stmt(s.Body))
		case *ast.RangeStmt:
			return seqNode(in(s.Pos(), s.Body.Pos()), stmt(s.Body))
		case *ast.LabeledStmt:
			return stmt(s.Stmt)
		}
		return in(s.Pos(), s.End())
	}

	var total *Node
	if body != nil {
		total = stmt(body)
	}
	rest := map[int]*Node{}
	for p, c := range calls {
		if !used[p] {
			rest[int(p)] = c
		}
	}
	return seqNode(total, seqNode(sortedNodes(rest)...))
}

func caseLabel(list []ast.Expr) string {
	if len(list) == 0 {
		return "default"
	}
	var parts []string
	for _, e := range list {
		parts = append(parts, types.ExprString(e))
	}
	return "case " + clip(strings.Join(parts, ", "))
}

// clip は分岐のラベルに使う式を短くする。
func clip(s string) string {
	r := []rune(s)
	if len(r) > 60 {
		return string(r[:57]) + "..."
	}
	return s
}

func (a *Analyzer) callCost(fn *ssa.Function, ci ssa.CallInstruction) *Summary {
	common := ci.Common()
	if recv, name, ok := spannerCallee(common); ok {
		return a.spannerCost(fn, ci, recv, name)
	}
	if common.IsInvoke() {
		return a.invokeCost(common)
	}
	callee := common.StaticCallee()
	if callee == nil {
		if !hasTxParam(common.Signature()) {
			return nil
		}
		cands := a.descendables(a.funcCandidates(common.Value, 0))
		if len(cands) == 0 {
			s := newSummary()
			s.Notes[fmt.Sprintf("tx を受け取る関数値の呼び出し先を特定できない %s", a.pos(ci.Pos()))] = true
			return s
		}
		return a.called("関数値の呼び出し", ci.Pos(), a.maxOver("関数値の候補", cands, a.summarize))
	}
	if a.descendable(callee) {
		return a.called(funcLabel(callee), ci.Pos(), a.summarize(callee))
	}

	// モジュール外の関数（lo.ForEach、errgroup.Go など）にクロージャを渡している場合は、呼ばれる回数を記号にして中身を数える
	var out *Summary
	for _, arg := range common.Args {
		fn := funcOf(arg)
		if fn == nil || !a.descendable(fn) {
			continue
		}
		sub := a.summarize(fn)
		if len(sub.Cost) == 0 && len(sub.Notes) == 0 {
			continue
		}
		if out == nil {
			out = newSummary()
		}
		sym := fmt.Sprintf("calls(%s)@%s", calleeName(callee), a.filePos(ci.Pos()))
		out.sub(sub, func(n *Node) *Node {
			return loopNode(sym, callNode(calleeName(callee)+" に渡した "+funcLabel(fn), a.filePos(ci.Pos()), n))
		})
	}
	return out
}

// called は呼び出し先の解析結果 sub を、呼び出しの節で包んだ結果を返す。sub はキャッシュされているので書き換えない。
func (a *Analyzer) called(label string, pos token.Pos, sub *Summary) *Summary {
	out := newSummary()
	out.sub(sub, func(n *Node) *Node { return callNode(label, a.filePos(pos), n) })
	return out
}

func (a *Analyzer) descendables(fns []*ssa.Function) []*ssa.Function {
	var out []*ssa.Function
	for _, fn := range fns {
		if a.descendable(fn) {
			out = append(out, fn)
		}
	}
	return out
}

func (a *Analyzer) descendable(fn *ssa.Function) bool {
	if fn.Pkg != nil {
		return a.inModule(fn.Pkg.Pkg)
	}
	if o := fn.Origin(); o != nil && o.Pkg != nil {
		return a.inModule(o.Pkg.Pkg)
	}
	// メソッド値やラッパーなどの合成関数
	return fn.Synthetic != "" && len(fn.Blocks) > 0
}

func (a *Analyzer) invokeCost(common *ssa.CallCommon) *Summary {
	impls := a.implsOf(common)
	if len(impls) == 0 {
		if hasTxParam(common.Signature()) {
			s := newSummary()
			s.Notes["実装が見つからない interface: "+types.TypeString(common.Value.Type(), nil)+"."+common.Method.Name()] = true
			return s
		}
		return nil
	}
	return a.called(methodLabel(common), common.Pos(), a.maxOver("実装の候補", impls, a.summarize))
}

func (a *Analyzer) spannerCost(fn *ssa.Function, ci ssa.CallInstruction, recv, name string) *Summary {
	if recv == "" && isMutationCtor(name) {
		// mutation は作った場所ではなく、BufferWrite に渡された時点で、渡された値を遡って数える
		return nil
	}
	if isWriteSite(recv, name) {
		a.reached[ci] = true
	}
	if recv == "ReadWriteTransaction" && name == "BufferWrite" {
		s := newSummary()
		s.Writes = true
		if len(ci.Common().Args) >= 2 {
			sub := a.mutCost(ci.Common().Args[1], fn, ci.Pos(), newFlowCtx())
			s.sub(sub, func(n *Node) *Node { return callNode("BufferWrite", a.filePos(ci.Pos()), n) })
		}
		return s
	}
	common := ci.Common()
	args := common.Args
	s := newSummary()
	switch {
	case recv == "ReadWriteTransaction" && (name == "Update" || name == "UpdateWithOptions"):
		if len(args) < 3 {
			return nil
		}
		s.Writes = true
		stmt := newSummary()
		a.dmlCost(stmt, args[2], ci)
		s.merge(stmt)
		s.addStmt(stmt.Tree)
	case recv == "ReadWriteTransaction" && strings.HasPrefix(name, "BatchUpdate"):
		s.Writes = true
		s.Notes["BatchUpdate は未対応 "+a.pos(ci.Pos())] = true
	case recv == "Client" && strings.HasPrefix(name, "ReadWriteTransaction"):
		s.Notes["トランザクション内で別トランザクションを開始(別コミットなので合計に含めない) "+a.pos(ci.Pos())] = true
	default:
		return nil
	}
	return s
}

func isMutationCtor(name string) bool {
	return strings.HasPrefix(name, "Insert") || strings.HasPrefix(name, "Replace") || strings.HasPrefix(name, "Update") || name == "Delete"
}

// mutationCtorCost は spanner.Insert などで作る mutation 1つのコスト。
func (a *Analyzer) mutationCtorCost(ci ssa.CallInstruction, name string) *Summary {
	args := ci.Common().Args
	s := newSummary()
	ts := a.tableArg(s, args, 0, ci)
	switch {
	case strings.HasPrefix(name, "InsertOrUpdate") || strings.HasPrefix(name, "Replace"):
		op := "insert_or_update"
		if strings.HasPrefix(name, "Replace") {
			op = "replace"
		}
		a.tableCost(s, ts, op, ci, func(t *Table) (Cost, string) {
			return Const(t.UpsertCost(nil)), t.UpsertDetail(nil)
		})
	case strings.HasPrefix(name, "Insert"):
		a.tableCost(s, ts, "insert", ci, func(t *Table) (Cost, string) {
			return Const(t.InsertCost(nil)), t.InsertDetail(nil)
		})
	case strings.HasPrefix(name, "Update"):
		a.tableCost(s, ts, "update", ci, func(t *Table) (Cost, string) {
			return Const(t.UpdateCost(nil)), t.UpdateDetail(nil)
		})
	case name == "Delete":
		single := len(args) > 1 && isSingleKey(args[1])
		a.tableCost(s, ts, "delete", ci, func(t *Table) (Cost, string) {
			var c Cost
			if single {
				c = Const(1 + t.DeleteIndexCost())
			} else {
				c = Cost{"": 1}
				c.Add(Const(t.DeleteIndexCost()).Mul(a.rowsSym(t, ci)))
			}
			c.Add(a.cascadeCost(s, t, ci))
			return c, t.DeleteDetail()
		})
	}
	return s
}

// tableCost は書き込み1つを s に加える。f はテーブルごとの1回あたりのコストと単価の内訳を返す。
// DB が異なる schema に同名テーブルがあると、どの DB への書き込みかは静的に決まらないので、項ごとの最大値をとる。
func (a *Analyzer) tableCost(s *Summary, ts []*Table, op string, ci ssa.CallInstruction, f func(t *Table) (Cost, string)) {
	if len(ts) > 1 {
		s.Notes[fmt.Sprintf("複数の schema に同名テーブル %s（最大値で計算）", ts[0].Name)] = true
	}
	var labels []string
	var kids []*Node
	for _, t := range ts {
		c, detail := f(t)
		labels = append(labels, "schema "+t.Source)
		kids = append(kids, writeNode(op, t.Name, c, detail, a.filePos(ci.Pos())))
	}
	s.add(altNode("同名テーブル", "", labels, kids))
}

func (a *Analyzer) tableArg(s *Summary, args []ssa.Value, i int, ci ssa.CallInstruction) []*Table {
	if len(args) <= i {
		return nil
	}
	name, ok := constString(args[i])
	if !ok {
		s.Notes["テーブル名が定数でない "+a.pos(ci.Pos())] = true
		return nil
	}
	return a.lookup(s, name)
}

func (a *Analyzer) lookup(s *Summary, name string) []*Table {
	ts := a.schema.Lookup(name)
	if len(ts) == 0 {
		s.Notes["schema にないテーブル: "+name] = true
	}
	return ts
}

func (a *Analyzer) dmlCost(s *Summary, stmt ssa.Value, ci ssa.CallInstruction) {
	sql, ok := stmtSQL(stmt)
	if !ok {
		s.Notes["DML の SQL が定数でない "+a.pos(ci.Pos())] = true
		return
	}
	dml, err := memefish.ParseDML("", sql)
	if err != nil {
		s.Notes["DML をパースできない "+a.pos(ci.Pos())] = true
		return
	}
	switch d := dml.(type) {
	case *memeast.Insert:
		var cols []string
		for _, c := range d.Columns {
			cols = append(cols, c.Name)
		}
		a.tableCost(s, a.lookup(s, pathName(d.TableName)), "DML INSERT", ci, func(t *Table) (Cost, string) {
			per := t.DMLInsertCost(cols)
			if v, ok := d.Input.(*memeast.ValuesInput); ok {
				return Const(per * len(v.Rows)), fmt.Sprintf("%s（1行あたり）× VALUES %d 行", t.DMLInsertDetail(cols), len(v.Rows))
			}
			return Const(per).Mul(a.rowsSym(t, ci)), t.DMLInsertDetail(cols) + "（1行あたり）"
		})
	case *memeast.Update:
		var cols []string
		for _, u := range d.Updates {
			cols = append(cols, u.Path[len(u.Path)-1].Name)
		}
		a.tableCost(s, a.lookup(s, pathName(d.TableName)), "DML UPDATE", ci, func(t *Table) (Cost, string) {
			return Const(t.DMLUpdateCost(cols)).Mul(a.rowsSym(t, ci)), t.DMLUpdateDetail(cols) + "（1行あたり）"
		})
	case *memeast.Delete:
		a.tableCost(s, a.lookup(s, pathName(d.TableName)), "DML DELETE", ci, func(t *Table) (Cost, string) {
			c := Const(1 + t.DeleteIndexCost()).Mul(a.rowsSym(t, ci))
			c.Add(a.cascadeCost(s, t, ci))
			return c, t.DeleteDetail() + "（1行あたり）"
		})
	}
}

// cascadeCost は t の削除で ON DELETE CASCADE により消える子孫テーブルの、インデックス分のコスト。
// 子の行そのものは公式どおり数えないが、子のインデックスの変更は個別に数えるとみなす（公式に明文化されていない推定）。
func (a *Analyzer) cascadeCost(s *Summary, t *Table, ci ssa.CallInstruction) Cost {
	out := Cost{}
	for _, c := range t.Children {
		if !c.Cascade {
			continue
		}
		if n := c.DeleteIndexCost(); n > 0 {
			out.Add(Const(n).Mul(fmt.Sprintf("rows(%s←%s)@%s", c.Name, t.Name, a.filePos(ci.Pos()))))
			s.Notes[fmt.Sprintf("%s の削除で CASCADE 削除される %s のインデックス分を rows(%s←%s) として計上（推定）", t.Name, c.Name, c.Name, t.Name)] = true
		}
		out.Add(a.cascadeCost(s, c, ci))
	}
	return out
}

func (a *Analyzer) rowsSym(t *Table, ci ssa.CallInstruction) string {
	return fmt.Sprintf("rows(%s)@%s", t.Name, a.filePos(ci.Pos()))
}

func (a *Analyzer) enclosingLoops(fn *ssa.Function, pos token.Pos) []loopInfo {
	if !pos.IsValid() {
		return nil
	}
	loops, ok := a.loops[fn]
	if !ok {
		loops = a.collectLoops(fn)
		a.loops[fn] = loops
	}
	var out []loopInfo
	for _, l := range loops {
		if l.start <= pos && pos < l.end {
			out = append(out, l)
		}
	}
	return out
}

func (a *Analyzer) collectLoops(fn *ssa.Function) []loopInfo {
	root := fn.Syntax()
	if o := fn.Origin(); root == nil && o != nil {
		root = o.Syntax()
	}
	if root == nil {
		return nil
	}
	var loops []loopInfo
	ast.Inspect(root, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			// クロージャは別の ssa.Function として扱う
			return n == root
		case *ast.RangeStmt:
			loops = append(loops, loopInfo{n.Body.Pos(), n.Body.End(), fmt.Sprintf("len(%s)@%s", types.ExprString(n.X), a.filePos(n.Pos()))})
		case *ast.ForStmt:
			loops = append(loops, loopInfo{n.Body.Pos(), n.Body.End(), "for@" + a.filePos(n.Pos())})
		}
		return true
	})
	return loops
}

func (a *Analyzer) pos(p token.Pos) string {
	return a.prog.Fset.Position(p).String()
}

// filePos は内訳の木や動的件数に持たせる位置（パス:行）。位置がなければ空文字。
// テキスト出力では basePos でファイル名だけに縮め、JSON では解析対象のモジュールからの相対パスにする。
func (a *Analyzer) filePos(p token.Pos) string {
	pp := a.prog.Fset.Position(p)
	if !pp.IsValid() {
		return ""
	}
	return fmt.Sprintf("%s:%d", pp.Filename, pp.Line)
}

// shortPos は分岐のラベルや注記の文中に書く位置（ファイル名:行）。
func (a *Analyzer) shortPos(p token.Pos) string {
	return basePos(a.filePos(p))
}

// basePos は「パス:行」をファイル名と行だけにする。
func basePos(pos string) string {
	if pos == "" {
		return ""
	}
	return filepath.Base(pos)
}

// spannerCallee は cloud.google.com/go/spanner の関数・メソッド呼び出しなら、レシーバ型名と関数名を返す。
func spannerCallee(common *ssa.CallCommon) (recv, name string, ok bool) {
	var obj *types.Func
	if common.IsInvoke() {
		return "", "", false
	}
	fn := common.StaticCallee()
	if fn == nil {
		return "", "", false
	}
	obj, _ = fn.Object().(*types.Func)
	if obj == nil && fn.Origin() != nil {
		obj, _ = fn.Origin().Object().(*types.Func)
	}
	if obj == nil || obj.Pkg() == nil || obj.Pkg().Path() != spannerPkg {
		return "", "", false
	}
	if r := obj.Signature().Recv(); r != nil {
		t := r.Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		if n, ok := t.(*types.Named); ok {
			recv = n.Obj().Name()
		}
	}
	return recv, obj.Name(), true
}

func funcOf(v ssa.Value) *ssa.Function {
	switch v := v.(type) {
	case *ssa.Function:
		return v
	case *ssa.MakeClosure:
		fn, _ := v.Fn.(*ssa.Function)
		return fn
	}
	return nil
}

// funcLabel は内訳に出す関数名。メソッドは「型名.メソッド名」、クロージャは外側の関数名に $1 などを付ける。
func funcLabel(fn *ssa.Function) string {
	if p := fn.Parent(); p != nil && strings.HasPrefix(fn.Name(), p.Name()) {
		return funcLabel(p) + fn.Name()[len(p.Name()):]
	}
	if recv := fn.Signature.Recv(); recv != nil {
		t := recv.Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		if n, ok := t.(*types.Named); ok {
			return n.Obj().Name() + "." + fn.Name()
		}
	}
	if fn.Pkg != nil {
		return fn.Pkg.Pkg.Name() + "." + fn.Name()
	}
	return fn.Name()
}

// methodLabel は interface 経由の呼び出しを「interface 名.メソッド名」で返す。
func methodLabel(common *ssa.CallCommon) string {
	if n, ok := types.Unalias(common.Value.Type()).(*types.Named); ok {
		return n.Obj().Name() + "." + common.Method.Name()
	}
	return common.Method.Name()
}

func calleeName(fn *ssa.Function) string {
	if obj := fn.Object(); obj != nil && obj.Pkg() != nil {
		return obj.Pkg().Name() + "." + obj.Name()
	}
	return fn.Name()
}

func hasTxParam(sig *types.Signature) bool {
	for i := 0; i < sig.Params().Len(); i++ {
		if p, ok := sig.Params().At(i).Type().(*types.Pointer); ok {
			if n, ok := p.Elem().(*types.Named); ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == spannerPkg && n.Obj().Name() == "ReadWriteTransaction" {
				return true
			}
		}
	}
	return false
}

func constString(v ssa.Value) (string, bool) {
	c, ok := v.(*ssa.Const)
	if !ok || c.Value == nil || c.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(c.Value), true
}

func isSingleKey(v ssa.Value) bool {
	mi, ok := v.(*ssa.MakeInterface)
	if !ok {
		return false
	}
	n, ok := mi.X.Type().(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == spannerPkg && n.Obj().Name() == "Key"
}

// stmtSQL は spanner.Statement の値から SQL 文字列の定数を辿る。
func stmtSQL(v ssa.Value) (string, bool) {
	switch v := v.(type) {
	case *ssa.Call:
		if _, name, ok := spannerCallee(&v.Call); ok && name == "NewStatement" && len(v.Call.Args) > 0 {
			return constString(v.Call.Args[0])
		}
	case *ssa.UnOp:
		if v.Op != token.MUL {
			return "", false
		}
		alloc, ok := v.X.(*ssa.Alloc)
		if !ok {
			return "", false
		}
		for _, ref := range *alloc.Referrers() {
			// stmt := spanner.NewStatement(...) のあとで stmt.Params を書き換えると、変数全体への代入になる
			if store, ok := ref.(*ssa.Store); ok && store.Addr == alloc {
				if sql, ok := stmtSQL(store.Val); ok {
					return sql, true
				}
				continue
			}
			fa, ok := ref.(*ssa.FieldAddr)
			if !ok {
				continue
			}
			st, ok := fa.X.Type().(*types.Pointer).Elem().Underlying().(*types.Struct)
			if !ok || st.Field(fa.Field).Name() != "SQL" {
				continue
			}
			for _, r := range *fa.Referrers() {
				if store, ok := r.(*ssa.Store); ok {
					return constString(store.Val)
				}
			}
		}
	}
	return "", false
}
