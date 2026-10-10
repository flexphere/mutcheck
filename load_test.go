package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// 整数・文字のリテラルだけが大量に並ぶ複合リテラルだけ要素を空にする
func TestStripByteLiterals(t *testing.T) {
	elts := func(n int, f func(i int) string) string {
		var b strings.Builder
		for i := range n {
			b.WriteString(f(i) + ", ")
		}
		return b.String()
	}
	hex := func(i int) string { return fmt.Sprintf("0x%02x", i%256) }
	src := "package p\n\n" +
		"var desc = []byte{" + elts(minStrippedElts, hex) + "}\n" +
		"var chars = []rune{" + elts(minStrippedElts, func(int) string { return "'a'" }) + "}\n" +
		"var nested = [][]byte{{" + elts(minStrippedElts, hex) + "}}\n" +
		"var small = []byte{" + elts(minStrippedElts-1, hex) + "}\n" +
		"var sized = [...]byte{" + elts(minStrippedElts, hex) + "}\n" +
		"var names = []string{" + elts(minStrippedElts, func(i int) string { return fmt.Sprintf("%q", fmt.Sprint("T", i)) }) + "}\n" +
		"var mixed = []int{" + elts(minStrippedElts-1, hex) + "len(desc)}\n"
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	stripByteLiterals(f)

	got := map[string]int{}
	for _, d := range f.Decls {
		vs := d.(*ast.GenDecl).Specs[0].(*ast.ValueSpec)
		cl := vs.Values[0].(*ast.CompositeLit)
		if vs.Names[0].Name == "nested" {
			cl = cl.Elts[0].(*ast.CompositeLit)
		}
		got[vs.Names[0].Name] = len(cl.Elts)
	}
	want := map[string]int{
		"desc":   0,
		"chars":  0,
		"nested": 0,
		"small":  minStrippedElts - 1,
		"sized":  minStrippedElts,
		"names":  minStrippedElts,
		"mixed":  minStrippedElts,
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s: 要素数 %d, want %d", name, got[name], n)
		}
	}
}
