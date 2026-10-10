package main

import (
	"go/token"
	"go/types"
	"sort"

	"golang.org/x/tools/go/ssa"
)

// WriteSite はモジュール内の書き込み箇所。
type WriteSite struct {
	Pos  token.Position
	Fn   *ssa.Function
	Call string // 呼び出しの名前（BufferWrite など）
	Hint string // トランザクションから辿り着かなかった理由の手がかり
	Cost *Summary
}

// isWriteSite は ReadWriteTransaction 内での書き込みの呼び出しか。
func isWriteSite(recv, name string) bool {
	if recv != "ReadWriteTransaction" {
		return false
	}
	switch name {
	case "BufferWrite", "Update", "UpdateWithOptions", "BatchUpdate", "BatchUpdateWithOptions":
		return true
	}
	return false
}

// isOutsideCommit は ReadWriteTransaction を使わずにコミットする呼び出しか。
func isOutsideCommit(recv, name string) bool {
	return recv == "Client" && (name == "Apply" || name == "BatchWrite" || name == "BatchWriteWithOptions")
}

// Coverage は全トランザクションを Analyze したあとに呼び、
// どのトランザクションからも辿り着かなかった書き込み箇所と、ReadWriteTransaction の外でコミットする箇所を返す。
func (a *Analyzer) Coverage() (unreached, outside []WriteSite) {
	for _, fn := range a.funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				ci, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				recv, name, ok := spannerCallee(ci.Common())
				if !ok {
					continue
				}
				site := WriteSite{Pos: a.prog.Fset.Position(ci.Pos()), Fn: fn, Call: recv + "." + name}
				switch {
				case isWriteSite(recv, name) && !a.reached[ci]:
					site.Hint = a.unreachedHint(fn)
					unreached = append(unreached, site)
				case isOutsideCommit(recv, name):
					if args := ci.Common().Args; name == "Apply" && len(args) > 2 {
						site.Cost = a.mutCost(args[2], fn, ci.Pos(), newFlowCtx())
					}
					outside = append(outside, site)
				}
			}
		}
	}
	sortSites(unreached)
	sortSites(outside)
	return unreached, outside
}

func (a *Analyzer) unreachedHint(fn *ssa.Function) string {
	root := outermost(fn)
	if n := len(a.callers[root]); n > 0 {
		c := a.callers[root][0]
		return "呼び出し元はあるがトランザクションから辿れない（例: " + a.shortPos(c.Pos()) + " " + shortFunc(c.Parent().String()) + "）"
	}
	if root.Signature.Recv() != nil {
		if sites := a.invokeCallers(root); len(sites) > 0 {
			c := sites[0]
			return "interface 経由の呼び出しはあるがトランザクションから辿れない（例: " + a.shortPos(c.Pos()) + " " + shortFunc(c.Parent().String()) + "）"
		}
		return "interface 経由でも呼ばれていない（未使用の可能性）"
	}
	return "呼び出し元なし（未使用の可能性）"
}

// outermost はクロージャなら外側の関数を返す。呼び出し元は外側の関数で判断する。
func outermost(fn *ssa.Function) *ssa.Function {
	for fn.Parent() != nil {
		fn = fn.Parent()
	}
	return fn
}

// invokeCallers は、メソッド fn を interface 経由で呼んでいそうな箇所を返す。
// メソッド名が同じで、fn の受け手の型がその interface を実装しているものを拾う（実際には別の実装が呼ばれることもある）。
func (a *Analyzer) invokeCallers(fn *ssa.Function) []ssa.CallInstruction {
	recv := fn.Signature.Recv()
	if recv == nil {
		return nil
	}
	var sites []ssa.CallInstruction
	for _, c := range a.invokes[fn.Name()] {
		iface, ok := c.Common().Value.Type().Underlying().(*types.Interface)
		if ok && types.Implements(recv.Type(), iface) {
			sites = append(sites, c)
		}
	}
	return sites
}

// TxCallers は、トランザクションを始める関数（クロージャなら外側の関数）を直接呼んでいる箇所を返す。
// 呼び出し元の行にトランザクションのコストを出すためのもので、1 段だけ辿る。
func (a *Analyzer) TxCallers(tx *Tx) []TxCaller {
	root := outermost(tx.Encl)
	sites := append(append([]ssa.CallInstruction{}, a.callers[root]...), a.invokeCallers(root)...)
	var out []TxCaller
	seen := map[string]bool{}
	for _, c := range sites {
		pos := a.filePos(c.Pos())
		// 位置のない呼び出し（コンパイラが作るラッパー）は出さない
		if pos == "" || seen[pos] {
			continue
		}
		seen[pos] = true
		out = append(out, TxCaller{Pos: pos, Func: funcLabel(c.Parent())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pos < out[j].Pos })
	return out
}

func sortSites(sites []WriteSite) {
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].Pos.Filename != sites[j].Pos.Filename {
			return sites[i].Pos.Filename < sites[j].Pos.Filename
		}
		return sites[i].Pos.Line < sites[j].Pos.Line
	})
}
