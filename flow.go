package main

import (
	"fmt"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ssa"
)

// 値を遡るときの深さの上限。引数→呼び出し元→引数…の連鎖が長いとき用。
const maxFlowDepth = 8

type retKey struct {
	fn  *ssa.Function
	idx int
}

type flowCtx struct {
	visited map[ssa.Value]bool
	depth   int
}

func newFlowCtx() *flowCtx { return &flowCtx{visited: map[ssa.Value]bool{}} }

func (fc *flowCtx) deeper() *flowCtx {
	return &flowCtx{visited: fc.visited, depth: fc.depth + 1}
}

// branch は排他的な経路を辿るための複製。経路間で辿った記録を共有すると、共通部分が後の経路で 0 になってしまう。
func (fc *flowCtx) branch() *flowCtx {
	v := make(map[ssa.Value]bool, len(fc.visited))
	for k := range fc.visited {
		v[k] = true
	}
	return &flowCtx{visited: v, depth: fc.depth}
}

// mutCost は v（*spanner.Mutation か []*spanner.Mutation）に含まれる mutation のコストを返す。
// v は関数 useFn の位置 use で使われる。v を作った位置が use より内側のループにあれば、そのループの記号を掛ける。
func (a *Analyzer) mutCost(v ssa.Value, useFn *ssa.Function, use token.Pos, fc *flowCtx) *Summary {
	s := newSummary()
	if v == nil || fc.visited[v] || fc.depth > maxFlowDepth {
		return s
	}
	fc.visited[v] = true

	fn := v.Parent()
	at := token.NoPos
	if instr, ok := v.(ssa.Instruction); ok {
		at = instr.Pos()
	}
	inner := use
	var syms []string
	if fn != nil && fn == useFn && at.IsValid() {
		syms = a.extraLoops(fn, at, use)
		inner = at
	}

	// append(base, elems...) は、ループの記号を追加した要素にだけ掛ける。base はループの外で作った分を含むため
	if call, ok := v.(*ssa.Call); ok && isAppend(call) {
		args := call.Call.Args
		base := a.mutCost(args[0], useFn, use, fc)
		s.sub(base, same)
		elems := a.mutCost(args[1], fn, inner, fc)
		s.sub(elems, func(n *Node) *Node { return loopNodes(syms, n) })
		return s
	}

	local := a.mutLocal(v, fn, inner, fc)
	s.sub(local, func(n *Node) *Node { return loopNodes(syms, n) })
	return s
}

func same(n *Node) *Node { return n }

func (a *Analyzer) mutLocal(v ssa.Value, fn *ssa.Function, use token.Pos, fc *flowCtx) *Summary {
	s := newSummary()
	add := func(sub *Summary) { s.sub(sub, same) }
	switch v := v.(type) {
	case *ssa.Const:
	case *ssa.Phi:
		// 合流点に入ってくる値は別々の経路から来るので、項ごとの最大値をとる。
		// ただしループで戻ってくる経路は、ループに入る前の値に追加した分なので足す。
		// continue などで戻ってくる経路が複数あるときは、1回の繰り返しではどれか1つなので、その中では最大値をとる
		var labels, backLabels []string
		var kids, backs []*Node
		for i, e := range v.Edges {
			sub := a.mutCost(e, fn, use, fc.branch())
			s.merge(sub)
			if v.Block().Dominates(v.Block().Preds[i]) {
				backLabels = append(backLabels, fmt.Sprintf("経路 %d", len(backLabels)+1))
				backs = append(backs, sub.Tree)
				continue
			}
			labels = append(labels, fmt.Sprintf("経路 %d", len(labels)+1))
			kids = append(kids, sub.Tree)
		}
		s.add(altNode("値の合流", a.filePos(v.Pos()), labels, kids))
		s.add(altNode("ループ内の分岐", a.filePos(v.Pos()), backLabels, backs))
	case *ssa.MakeInterface:
		add(a.mutCost(v.X, fn, use, fc))
	case *ssa.ChangeType:
		add(a.mutCost(v.X, fn, use, fc))
	case *ssa.Convert:
		add(a.mutCost(v.X, fn, use, fc))
	case *ssa.TypeAssert:
		add(a.mutCost(v.X, fn, use, fc))
	case *ssa.Slice:
		// []*Mutation{a, b} は配列の Alloc を slice したもの。要素は IndexAddr 経由で代入される
		if _, ok := v.X.(*ssa.Alloc); ok {
			add(a.elementStores(v.X, fn, use, fc))
		} else {
			add(a.mutCost(v.X, fn, use, fc))
		}
		add(a.elementStores(v, fn, use, fc))
	case *ssa.MakeSlice:
		add(a.elementStores(v, fn, use, fc))
	case *ssa.UnOp:
		if v.Op == token.MUL {
			add(a.loadCost(v.X, fn, use, fc))
		}
	case *ssa.Call:
		add(a.callFlow(v, 0, fn, use, fc))
	case *ssa.Extract:
		if call, ok := v.Tuple.(*ssa.Call); ok {
			add(a.callFlow(call, v.Index, fn, use, fc))
		}
	case *ssa.Parameter:
		add(a.paramFlow(v, fc))
	case *ssa.FreeVar:
		if b, mc := a.binding(v); b != nil {
			add(a.mutCost(b, mc.Parent(), mc.Pos(), fc.deeper()))
		}
	default:
		s.Notes[fmt.Sprintf("mutation の値を辿れない(%T) %s", v, a.pos(v.Pos()))] = true
	}
	return s
}

// elementStores は slice（または配列の Alloc）x の要素に代入された mutation のコスト。
func (a *Analyzer) elementStores(x ssa.Value, fn *ssa.Function, use token.Pos, fc *flowCtx) *Summary {
	s := newSummary()
	refs := x.Referrers()
	if refs == nil {
		return s
	}
	for _, ref := range *refs {
		ia, ok := ref.(*ssa.IndexAddr)
		if !ok || ia.X != x {
			continue
		}
		for _, r := range *ia.Referrers() {
			if st, ok := r.(*ssa.Store); ok && st.Addr == ia {
				s.sub(a.mutCost(st.Val, fn, use, fc), same)
			}
		}
	}
	return s
}

// loadCost はポインタ ptr が指す変数に代入された mutation のコスト。
func (a *Analyzer) loadCost(ptr ssa.Value, fn *ssa.Function, use token.Pos, fc *flowCtx) *Summary {
	s := newSummary()
	add := func(sub *Summary) { s.sub(sub, same) }
	switch p := ptr.(type) {
	case *ssa.Alloc:
		for _, val := range storesTo(p) {
			add(a.mutCost(val, fn, use, fc))
		}
		// muts[i] = x は、変数を読み出した slice への代入になる
		for _, l := range loadsOf(p) {
			add(a.elementStores(l, fn, use, fc))
		}
		// クロージャが捕捉して中で代入している場合
		for _, fv := range a.capturedAs(p) {
			for _, val := range storesTo(fv) {
				add(a.mutCost(val, fv.Parent(), token.NoPos, fc.deeper()))
			}
		}
	case *ssa.FreeVar:
		for _, val := range storesTo(p) {
			add(a.mutCost(val, fn, use, fc))
		}
		for _, l := range loadsOf(p) {
			add(a.elementStores(l, fn, use, fc))
		}
		if b, mc := a.binding(p); b != nil {
			add(a.loadCost(b, mc.Parent(), mc.Pos(), fc.deeper()))
		}
	default:
		s.Notes[fmt.Sprintf("mutation の値を辿れない(%T への格納) %s", ptr, a.pos(ptr.Pos()))] = true
	}
	return s
}

func loadsOf(addr ssa.Value) []ssa.Value {
	var out []ssa.Value
	refs := addr.Referrers()
	if refs == nil {
		return nil
	}
	for _, ref := range *refs {
		if u, ok := ref.(*ssa.UnOp); ok && u.Op == token.MUL && u.X == addr {
			out = append(out, u)
		}
	}
	return out
}

func storesTo(addr ssa.Value) []ssa.Value {
	var out []ssa.Value
	refs := addr.Referrers()
	if refs == nil {
		return nil
	}
	for _, ref := range *refs {
		if st, ok := ref.(*ssa.Store); ok && st.Addr == addr {
			out = append(out, st.Val)
		}
	}
	return out
}

// capturedAs は alloc を捕捉しているクロージャ側の FreeVar を返す。
func (a *Analyzer) capturedAs(alloc *ssa.Alloc) []*ssa.FreeVar {
	var out []*ssa.FreeVar
	for _, ref := range *alloc.Referrers() {
		mc, ok := ref.(*ssa.MakeClosure)
		if !ok {
			continue
		}
		fn, ok := mc.Fn.(*ssa.Function)
		if !ok {
			continue
		}
		for i, b := range mc.Bindings {
			if b == alloc && i < len(fn.FreeVars) {
				out = append(out, fn.FreeVars[i])
			}
		}
	}
	return out
}

// binding は FreeVar に束縛された外側の値と、その MakeClosure を返す。
func (a *Analyzer) binding(fv *ssa.FreeVar) (ssa.Value, *ssa.MakeClosure) {
	fn := fv.Parent()
	idx := -1
	for i, f := range fn.FreeVars {
		if f == fv {
			idx = i
		}
	}
	parent := fn.Parent()
	if idx < 0 || parent == nil {
		return nil, nil
	}
	for _, b := range parent.Blocks {
		for _, instr := range b.Instrs {
			if mc, ok := instr.(*ssa.MakeClosure); ok && mc.Fn == fn && idx < len(mc.Bindings) {
				return mc.Bindings[idx], mc
			}
		}
	}
	return nil, nil
}

// callFlow は call の戻り値（idx 番目）に含まれる mutation のコスト。
func (a *Analyzer) callFlow(call *ssa.Call, idx int, fn *ssa.Function, use token.Pos, fc *flowCtx) *Summary {
	common := &call.Call
	if recv, name, ok := spannerCallee(common); ok {
		if recv == "" && isMutationCtor(name) {
			return a.mutationCtorCost(call, name)
		}
		return newSummary()
	}
	if common.IsInvoke() {
		impls := a.implsOf(common)
		return a.called(methodLabel(common), call.Pos(), a.maxOver("実装の候補", impls, func(f *ssa.Function) *Summary { return a.retCost(f, idx) }))
	}
	if callee := common.StaticCallee(); callee != nil {
		if a.descendable(callee) {
			return a.called(funcLabel(callee), call.Pos(), a.retCost(callee, idx))
		}
		// モジュール外の関数（slices.Concat、lo.Flatten など）は、mutation を含む引数がそのまま戻り値に入るとみなす
		s := newSummary()
		for _, arg := range common.Args {
			if isMutationType(arg.Type()) {
				s.sub(a.mutCost(arg, fn, use, fc), same)
			}
			if f := funcOf(arg); f != nil && a.descendable(f) && returnsMutation(f.Signature) {
				sym := fmt.Sprintf("calls(%s)@%s", calleeName(callee), a.filePos(call.Pos()))
				s.sub(a.retCost(f, 0), func(n *Node) *Node {
					return loopNode(sym, callNode(calleeName(callee)+" に渡した "+funcLabel(f), a.filePos(call.Pos()), n))
				})
			}
		}
		return s
	}
	cands := a.descendables(a.funcCandidates(common.Value, 0))
	if len(cands) == 0 {
		s := newSummary()
		s.Notes["mutation を返す関数値の呼び出し先を特定できない "+a.pos(call.Pos())] = true
		return s
	}
	return a.called("関数値の呼び出し", call.Pos(), a.maxOver("関数値の候補", cands, func(f *ssa.Function) *Summary { return a.retCost(f, idx) }))
}

// retCost は fn の idx 番目の戻り値に含まれる mutation のコスト。return ごとに排他的なので最大値をとる。
func (a *Analyzer) retCost(fn *ssa.Function, idx int) *Summary {
	key := retKey{fn, idx}
	if s, ok := a.retSums[key]; ok {
		return s
	}
	if a.retBusy[key] {
		return newSummary()
	}
	a.retBusy[key] = true
	defer delete(a.retBusy, key)

	out := newSummary()
	var labels []string
	var kids []*Node
	for _, b := range fn.Blocks {
		ret, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return)
		if !ok || idx >= len(ret.Results) {
			continue
		}
		sub := a.mutCost(ret.Results[idx], fn, ret.Pos(), newFlowCtx())
		out.merge(sub)
		labels = append(labels, "return "+a.shortPos(ret.Pos()))
		kids = append(kids, sub.Tree)
	}
	out.add(altNode("return", "", labels, kids))
	a.retSums[key] = out
	return out
}

// paramFlow は引数 p に呼び出し元から渡された mutation のコスト。呼び出し元ごとに排他的なので最大値をとる。
func (a *Analyzer) paramFlow(p *ssa.Parameter, fc *flowCtx) *Summary {
	fn := p.Parent()
	idx := -1
	for i, q := range fn.Params {
		if q == p {
			idx = i
		}
	}
	out := newSummary()
	sites := a.callers[fn]
	if len(sites) == 0 {
		out.Notes["mutation を受け取る引数の呼び出し元が見つからない: "+fn.String()] = true
		return out
	}
	var labels []string
	var kids []*Node
	for _, site := range sites {
		args := site.Common().Args
		if idx < 0 || idx >= len(args) {
			continue
		}
		sub := a.mutCost(args[idx], site.Parent(), site.Pos(), fc.deeper())
		out.merge(sub)
		labels = append(labels, "呼び出し元 "+funcLabel(site.Parent())+" "+a.shortPos(site.Pos()))
		kids = append(kids, sub.Tree)
	}
	out.add(altNode("呼び出し元", "", labels, kids))
	return out
}

func (a *Analyzer) implsOf(common *ssa.CallCommon) []*ssa.Function {
	named, ok := types.Unalias(common.Value.Type()).(*types.Named)
	if !ok || !a.inModule(named.Obj().Pkg()) {
		return nil
	}
	iface, ok := named.Underlying().(*types.Interface)
	if !ok {
		return nil
	}
	key := named.String() + "." + common.Method.Name()
	impls, ok := a.implCache[key]
	if !ok {
		for _, cand := range a.candidatesWithMethod(common.Method.Name()) {
			if !types.Implements(cand, iface) {
				continue
			}
			sel := a.prog.MethodSets.MethodSet(cand).Lookup(common.Method.Pkg(), common.Method.Name())
			if sel == nil {
				continue
			}
			if fn := a.prog.MethodValue(sel); fn != nil {
				impls = append(impls, fn)
			}
		}
		a.implCache[key] = impls
	}
	return impls
}

// candidatesWithMethod は、name という名前のメソッドを持つ具象型の候補を candidates の順で返す。
// interface を実装する型は呼ばれたメソッドを必ず持つので、types.Implements を当てる前にこれで絞る
// （候補は数千あり、interface ごとに全部へ types.Implements を当てると解析の時間の多くを占める）。
func (a *Analyzer) candidatesWithMethod(name string) []types.Type {
	if a.byMethod == nil {
		a.byMethod = map[string][]types.Type{}
		for _, cand := range a.candidates {
			ms := a.prog.MethodSets.MethodSet(cand)
			seen := map[string]bool{}
			for i := 0; i < ms.Len(); i++ {
				n := ms.At(i).Obj().Name()
				if !seen[n] {
					seen[n] = true
					a.byMethod[n] = append(a.byMethod[n], cand)
				}
			}
		}
	}
	return a.byMethod[name]
}

// funcCandidates は関数値 v として呼ばれうる関数を、引数・捕捉変数・フィールドへの代入を遡って集める。
func (a *Analyzer) funcCandidates(v ssa.Value, depth int) []*ssa.Function {
	if v == nil || depth > maxFlowDepth {
		return nil
	}
	if fn := funcOf(v); fn != nil {
		return []*ssa.Function{fn}
	}
	var out []*ssa.Function
	seen := map[*ssa.Function]bool{}
	add := func(fns []*ssa.Function) {
		for _, fn := range fns {
			if !seen[fn] {
				seen[fn] = true
				out = append(out, fn)
			}
		}
	}
	switch v := v.(type) {
	case *ssa.Parameter:
		fn := v.Parent()
		for i, q := range fn.Params {
			if q != v {
				continue
			}
			for _, site := range a.callers[fn] {
				if args := site.Common().Args; i < len(args) {
					add(a.funcCandidates(args[i], depth+1))
				}
			}
		}
	case *ssa.FreeVar:
		if b, _ := a.binding(v); b != nil {
			add(a.funcCandidates(b, depth+1))
		}
	case *ssa.UnOp:
		if v.Op != token.MUL {
			break
		}
		switch x := v.X.(type) {
		case *ssa.Alloc:
			for _, val := range storesTo(x) {
				add(a.funcCandidates(val, depth+1))
			}
		case *ssa.FreeVar:
			if b, _ := a.binding(x); b != nil {
				if alloc, ok := b.(*ssa.Alloc); ok {
					for _, val := range storesTo(alloc) {
						add(a.funcCandidates(val, depth+1))
					}
				}
			}
		case *ssa.FieldAddr:
			if fv := fieldVar(x.X.Type(), x.Field); fv != nil {
				for _, val := range a.fieldStores[fv] {
					add(a.funcCandidates(val, depth+1))
				}
			}
		}
	case *ssa.Field:
		if fv := fieldVar(v.X.Type(), v.Field); fv != nil {
			for _, val := range a.fieldStores[fv] {
				add(a.funcCandidates(val, depth+1))
			}
		}
	case *ssa.Phi:
		for _, e := range v.Edges {
			add(a.funcCandidates(e, depth+1))
		}
	case *ssa.ChangeType:
		add(a.funcCandidates(v.X, depth+1))
	}
	return out
}

// fieldVar は構造体（またはそのポインタ）型 t の i 番目のフィールドを返す。
func fieldVar(t types.Type, i int) *types.Var {
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	st, ok := t.Underlying().(*types.Struct)
	if !ok || i >= st.NumFields() {
		return nil
	}
	return st.Field(i)
}

// extraLoops は at を囲むループのうち、use を囲んでいないものの記号を返す。
func (a *Analyzer) extraLoops(fn *ssa.Function, at, use token.Pos) []string {
	inUse := map[string]bool{}
	for _, l := range a.enclosingLoops(fn, use) {
		inUse[l.sym] = true
	}
	var out []string
	for _, l := range a.enclosingLoops(fn, at) {
		if !inUse[l.sym] {
			out = append(out, l.sym)
		}
	}
	return out
}

func isAppend(call *ssa.Call) bool {
	b, ok := call.Call.Value.(*ssa.Builtin)
	return ok && b.Name() == "append" && len(call.Call.Args) == 2
}

func isMutationType(t types.Type) bool {
	switch t := t.Underlying().(type) {
	case *types.Slice:
		return isMutationType(t.Elem())
	case *types.Pointer:
		n, ok := t.Elem().(*types.Named)
		return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == spannerPkg && n.Obj().Name() == "Mutation"
	}
	return false
}

func returnsMutation(sig *types.Signature) bool {
	for i := 0; i < sig.Results().Len(); i++ {
		if isMutationType(sig.Results().At(i).Type()) {
			return true
		}
	}
	return false
}
