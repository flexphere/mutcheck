package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
	"golang.org/x/tools/go/packages"
)

// loadPackages は dir の ./... を読み込み、型チェックしたパッケージを返す。
//
// packages.Load で型まで読むと、依存パッケージの型を export data から読むために go list -export が
// 対象のパッケージをすべてコンパイルする。解析対象のパッケージはソースから型チェックするので export data は要らないが、
// 多くのパッケージから import されるパッケージを編集すると、依存元のコンパイルし直しで読み込みに数秒〜十数秒かかる。
// そこで、解析対象（./... に当たるパッケージ）は go list でファイルの一覧だけを得て自分でパース・型チェックし、
// export data はそれ以外の依存パッケージ（外部モジュール・標準ライブラリ）にだけ使う（export.go）。
// 外部の依存は編集しないので、コンパイルはいつもビルドキャッシュに当たり、場所も覚えておける。
func loadPackages(dir string) ([]*packages.Package, error) {
	env := readGoEnv(dir)
	roots, err := goList(dir, env)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, nil
	}
	isRoot := map[string]bool{}
	goMod := ""
	for _, lp := range roots {
		isRoot[lp.ImportPath] = true
		if lp.Module != nil && lp.Module.Main && goMod == "" {
			goMod = lp.Module.GoMod
		}
	}

	// 解析対象が直接 import する、解析対象の外のパッケージ。型は export data から読む
	extSet := map[string]bool{}
	for _, lp := range roots {
		for _, imp := range lp.Imports {
			if path := lp.resolve(imp); !isRoot[path] && path != "C" && path != "unsafe" {
				extSet[path] = true
			}
		}
	}

	fset := token.NewFileSet()
	var (
		ext    map[string]*types.Package
		extErr error
		parsed = make([]parsedPackage, len(roots))
	)
	g := errgroup.Group{}
	g.Go(func() error {
		ext, extErr = loadExportData(dir, fset, keys(extSet), exportKey(env, goMod))
		return nil
	})
	// パースは型チェックと違ってパッケージの順序に依存しないので、export data の読み込みと並行してまとめて行う
	parseFiles(fset, roots, parsed)
	_ = g.Wait()
	if extErr != nil {
		return nil, extErr
	}

	pkgs := make([]*packages.Package, len(roots))
	index := map[string]int{}
	for i, lp := range roots {
		index[lp.ImportPath] = i
		pkgs[i] = &packages.Package{
			ID:      lp.ImportPath,
			Name:    lp.Name,
			PkgPath: lp.ImportPath,
			Fset:    fset,
			Syntax:  parsed[i].files,
			Errors:  parsed[i].errs,
			Imports: map[string]*packages.Package{},
		}
		if lp.Module != nil {
			pkgs[i].Module = &packages.Module{Path: lp.Module.Path, Dir: lp.Module.Dir, GoVersion: lp.Module.GoVersion}
		}
		if lp.Error != nil {
			pkgs[i].Errors = append([]packages.Error{{Pos: lp.Error.Pos, Msg: lp.Error.Err, Kind: packages.ListError}}, pkgs[i].Errors...)
		}
	}

	// 依存するパッケージの型チェックが終わってから型チェックする。独立したパッケージは並行して進める
	done := make([]chan struct{}, len(roots))
	for i := range done {
		done[i] = make(chan struct{})
	}
	sizes := types.SizesFor("gc", build.Default.GOARCH)
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	for i, lp := range roots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done[i])
			deps := map[string]int{}
			for _, imp := range lp.Imports {
				path := lp.resolve(imp)
				if j, ok := index[path]; ok {
					<-done[j]
					deps[imp] = j
				}
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			typeCheck(pkgs[i], lp, deps, pkgs, ext, sizes)
		}()
	}
	wg.Wait()
	return pkgs, nil
}

// listedPackage は go list -json の出力のうち、読み込みに使うフィールド。
type listedPackage struct {
	ImportPath      string
	Name            string
	Dir             string
	GoFiles         []string
	CgoFiles        []string
	CompiledGoFiles []string
	Imports         []string
	ImportMap       map[string]string
	Module          *listedModule
	Error           *struct {
		Pos string
		Err string
	}
}

type listedModule struct {
	Path      string
	Main      bool
	Dir       string
	GoMod     string
	GoVersion string
}

// resolve は import 文のパスを、vendor などを考慮した実際のパッケージのパスにする。
func (lp *listedPackage) resolve(imp string) string {
	if p, ok := lp.ImportMap[imp]; ok {
		return p
	}
	return imp
}

// files は型チェックするファイル。cgo を使うパッケージは cgo が生成したファイル（CompiledGoFiles）を使う。
func (lp *listedPackage) files() []string {
	if len(lp.CompiledGoFiles) > 0 {
		return lp.CompiledGoFiles
	}
	return lp.GoFiles
}

// goList は ./... のパッケージの一覧を返す。依存パッケージは一覧にしない（export.go で辿る）。
// ふつうの構成なら go/build で走査し（scan.go）、それ以外は go list に任せる。
// -compiled は cgo の前処理を走らせるので、cgo を使うパッケージがあるときだけ付けて go list で取り直す。
func goList(dir string, env *goEnv) ([]*listedPackage, error) {
	out, err := scanPackages(dir, env)
	if err != nil {
		out, err = runGoList(dir, false)
	}
	if err != nil {
		return nil, err
	}
	for _, lp := range out {
		if len(lp.CgoFiles) > 0 {
			return runGoList(dir, true)
		}
	}
	return out, nil
}

func runGoList(dir string, compiled bool) ([]*listedPackage, error) {
	cmd := exec.Command("go", "list", "-e", fmt.Sprintf("-compiled=%t", compiled),
		"-json=ImportPath,Name,Dir,GoFiles,CgoFiles,CompiledGoFiles,Imports,ImportMap,Module,Error", "./...")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list: %v: %s", err, stderr.String())
	}
	var out []*listedPackage
	dec := json.NewDecoder(&stdout)
	for {
		lp := &listedPackage{}
		if err := dec.Decode(lp); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("go list の出力を読めません: %v", err)
		}
		out = append(out, lp)
	}
	return out, nil
}

type parsedPackage struct {
	files []*ast.File
	errs  []packages.Error
}

// parseFiles は解析対象のファイルを並行してパースする。
func parseFiles(fset *token.FileSet, roots []*listedPackage, out []parsedPackage) {
	type job struct{ pkg, file int }
	var jobs []job
	files := make([][]*ast.File, len(roots))
	errs := make([][]error, len(roots))
	for i, lp := range roots {
		files[i] = make([]*ast.File, len(lp.files()))
		errs[i] = make([]error, len(lp.files()))
		for j := range lp.files() {
			jobs = append(jobs, job{i, j})
		}
	}
	var g errgroup.Group
	g.SetLimit(runtime.GOMAXPROCS(0))
	for _, jb := range jobs {
		g.Go(func() error {
			lp := roots[jb.pkg]
			path := lp.files()[jb.file]
			if !strings.HasPrefix(path, "/") {
				path = lp.Dir + "/" + path
			}
			src, err := os.ReadFile(path)
			if err != nil {
				errs[jb.pkg][jb.file] = err
				return nil
			}
			f, err := parser.ParseFile(fset, path, src, parser.AllErrors|parser.SkipObjectResolution)
			files[jb.pkg][jb.file] = f
			errs[jb.pkg][jb.file] = err
			return nil
		})
	}
	_ = g.Wait()
	for i := range roots {
		for j, f := range files[i] {
			if f != nil {
				out[i].files = append(out[i].files, f)
			}
			if err := errs[i][j]; err != nil {
				out[i].errs = append(out[i].errs, parseErrors(err)...)
			}
		}
	}
}

// parseErrors はパースのエラーを 1 件ずつ packages.Error にする（packages.Load と同じ形）。
func parseErrors(err error) []packages.Error {
	if list, ok := err.(scanner.ErrorList); ok {
		out := make([]packages.Error, 0, len(list))
		for _, e := range list {
			out = append(out, packages.Error{Pos: e.Pos.String(), Msg: e.Msg, Kind: packages.ParseError})
		}
		return out
	}
	return []packages.Error{{Pos: "-", Msg: err.Error(), Kind: packages.ParseError}}
}

// typeCheck は p をソースから型チェックする。deps は import 文のパスから解析対象のパッケージ（pkgs の添字）へのマップ。
func typeCheck(p *packages.Package, lp *listedPackage, deps map[string]int, pkgs []*packages.Package, ext map[string]*types.Package, sizes types.Sizes) {
	p.TypesInfo = &types.Info{
		Types:        map[ast.Expr]types.TypeAndValue{},
		Defs:         map[*ast.Ident]types.Object{},
		Uses:         map[*ast.Ident]types.Object{},
		Implicits:    map[ast.Node]types.Object{},
		Instances:    map[*ast.Ident]types.Instance{},
		Scopes:       map[ast.Node]*types.Scope{},
		Selections:   map[*ast.SelectorExpr]*types.Selection{},
		FileVersions: map[*ast.File]string{},
	}
	illDep := false
	conf := types.Config{
		Sizes: sizes,
		Importer: importerFunc(func(path string) (*types.Package, error) {
			if path == "unsafe" {
				return types.Unsafe, nil
			}
			if j, ok := deps[path]; ok {
				dep := pkgs[j]
				p.Imports[path] = dep
				illDep = illDep || dep.IllTyped
				if dep.Types == nil {
					return nil, fmt.Errorf("%s を読み込めませんでした", path)
				}
				return dep.Types, nil
			}
			if t, ok := ext[lp.resolve(path)]; ok {
				return t, nil
			}
			return nil, fmt.Errorf("%s の型情報がありません", path)
		}),
		Error: func(err error) {
			if te, ok := err.(types.Error); ok {
				p.Errors = append(p.Errors, packages.Error{Pos: te.Fset.Position(te.Pos).String(), Msg: te.Msg, Kind: packages.TypeError})
				return
			}
			p.Errors = append(p.Errors, packages.Error{Pos: "-", Msg: err.Error(), Kind: packages.UnknownError})
		},
	}
	if p.Module != nil && p.Module.GoVersion != "" {
		conf.GoVersion = "go" + p.Module.GoVersion
	}
	p.Types, _ = conf.Check(lp.ImportPath, p.Fset, p.Syntax, p.TypesInfo)
	p.IllTyped = len(p.Errors) > 0 || illDep
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
