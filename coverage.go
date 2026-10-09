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
	// クロージャは外側の関数で判断する
	root := fn
	for root.Parent() != nil {
		root = root.Parent()
	}
	if n := len(a.callers[root]); n > 0 {
		c := a.callers[root][0]
		return "呼び出し元はあるがトランザクションから辿れない（例: " + a.shortPos(c.Pos()) + " " + shortFunc(c.Parent().String()) + "）"
	}
	if recv := root.Signature.Recv(); recv != nil {
		for _, c := range a.invokes[root.Name()] {
			iface, ok := c.Common().Value.Type().Underlying().(*types.Interface)
			if ok && types.Implements(recv.Type(), iface) {
				return "interface 経由の呼び出しはあるがトランザクションから辿れない（例: " + a.shortPos(c.Pos()) + " " + shortFunc(c.Parent().String()) + "）"
			}
		}
		return "interface 経由でも呼ばれていない（未使用の可能性）"
	}
	return "呼び出し元なし（未使用の可能性）"
}

func sortSites(sites []WriteSite) {
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].Pos.Filename != sites[j].Pos.Filename {
			return sites[i].Pos.Filename < sites[j].Pos.Filename
		}
		return sites[i].Pos.Line < sites[j].Pos.Line
	})
}
