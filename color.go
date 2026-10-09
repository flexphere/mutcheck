package main

import (
	"os"
	"regexp"
	"strings"
)

// ANSI エスケープの色
const (
	colorRed     = "31"
	colorGreen   = "32"
	colorYellow  = "33"
	colorMagenta = "35"
	colorCyan    = "36"
	styleBold    = "1"
	styleDim     = "2"
)

// palette は出力に色を付ける。on が false なら何もしない。
type palette struct {
	on bool
}

// newPalette は -color の指定から palette を作る。auto は標準出力がターミナルで、NO_COLOR がないときだけ色を付ける。
func newPalette(mode string) palette {
	switch mode {
	case "always":
		return palette{on: true}
	case "never":
		return palette{}
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return palette{}
	}
	fi, err := os.Stdout.Stat()
	return palette{on: err == nil && fi.Mode()&os.ModeCharDevice != 0}
}

func (p palette) color(code, s string) string {
	if !p.on || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p palette) bold(s string) string    { return p.color(styleBold, s) }
func (p palette) dim(s string) string     { return p.color(styleDim, s) }
func (p palette) magenta(s string) string { return p.color(colorMagenta, s) }

// tag は判定を色分けして [ ] で囲む。
func (p palette) tag(tag string) string {
	code := map[string]string{tagFixed: colorGreen, tagSymbolic: colorYellow, tagExceeded: colorRed, tagZero: colorMagenta}[tag]
	return p.color(code, "["+tag+"]")
}

// op は書き込みの操作を色分けする。
func (p palette) op(op string) string {
	switch {
	case strings.HasPrefix(op, "DML"):
		return p.color(colorCyan, op)
	case op == "insert":
		return p.color(colorGreen, op)
	case op == "delete":
		return p.color(colorRed, op)
	}
	return p.color(colorYellow, op)
}

var boundRe = regexp.MustCompile(`≤ [0-9,]+`)

// boldBounds は上限内の条件の「≤ N」を太字にする。
func (p palette) boldBounds(s string) string {
	if !p.on {
		return s
	}
	return boundRe.ReplaceAllStringFunc(s, p.bold)
}
