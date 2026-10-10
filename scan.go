package main

import (
	"encoding/json"
	"errors"
	"go/build"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/sync/errgroup"
)

// go list ./... は解析対象のパッケージだけを一覧にする場合でも、依存パッケージの import をすべて解決するので
// 大きなモジュールでは 0.5 秒ほどかかる。解析対象のパッケージに要るのはファイルの一覧と import だけなので、
// ふつうの構成のモジュールでは go/build でディレクトリを走査して自分で作る。
// go list と同じ結果になると言い切れない構成（vendor・go.work・cgo など）では go list に任せる。

// goEnv は go env の値のうち、ディレクトリの走査と export data の場所を覚えておく判定に使うもの。
type goEnv struct {
	GOVERSION    string
	GOOS         string
	GOARCH       string
	GOFLAGS      string
	GOEXPERIMENT string
	CGO_ENABLED  string
	GOWORK       string
	GOMOD        string
	// raw は go env の出力そのもの。export data の場所を覚えておく判定（exportKey）に使う
	raw []byte
}

// readGoEnv は dir での go env の値を返す。go env が失敗したら nil を返す（走査も場所を覚えておくのもやめる）。
func readGoEnv(dir string) *goEnv {
	cmd := exec.Command("go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "GOFLAGS", "GOEXPERIMENT", "CGO_ENABLED", "GOWORK", "GOMOD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	env := &goEnv{raw: out}
	if json.Unmarshal(out, env) != nil {
		return nil
	}
	return env
}

// errScanUnsupported は go/build での走査では go list と同じ結果にならない構成のとき返す。
var errScanUnsupported = errors.New("go/build での走査に対応していない構成")

// scanPackages は dir の ./... に当たるパッケージを go/build で走査して、go list と同じ形で返す。
// 対応していない構成やエラーのあるパッケージがあれば errScanUnsupported を返すので、go list で取り直す。
func scanPackages(dir string, env *goEnv) ([]*listedPackage, error) {
	if env == nil || env.GOMOD == "" || env.GOMOD == os.DevNull {
		return nil, errScanUnsupported
	}
	// go.work があると ./... の解決が変わる。GOEXPERIMENT はビルドタグに効く
	if (env.GOWORK != "" && env.GOWORK != "off") || env.GOEXPERIMENT != "" {
		return nil, errScanUnsupported
	}
	tags, ok := buildTags(env.GOFLAGS)
	if !ok {
		return nil, errScanUnsupported
	}
	releaseTags, ok := releaseTagsOf(env.GOVERSION)
	if !ok {
		return nil, errScanUnsupported
	}
	modRoot := filepath.Dir(env.GOMOD)
	// vendor があると import の解決（ImportMap）が変わる
	if _, err := os.Stat(filepath.Join(modRoot, "vendor", "modules.txt")); err == nil {
		return nil, errScanUnsupported
	}
	data, err := os.ReadFile(env.GOMOD)
	if err != nil {
		return nil, errScanUnsupported
	}
	mf, err := modfile.ParseLax(env.GOMOD, data, nil)
	// ignore ディレクティブは ./... から外すディレクトリを変える
	if err != nil || mf.Module == nil || len(mf.Ignore) > 0 {
		return nil, errScanUnsupported
	}
	goVersion := ""
	if mf.Go != nil {
		goVersion = mf.Go.Version
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, errScanUnsupported
	}
	dirs, err := packageDirs(absDir)
	if err != nil {
		return nil, errScanUnsupported
	}

	ctx := build.Default
	ctx.GOOS, ctx.GOARCH = env.GOOS, env.GOARCH
	ctx.CgoEnabled = env.CGO_ENABLED == "1"
	ctx.BuildTags = tags
	ctx.ReleaseTags = releaseTags

	out := make([]*listedPackage, len(dirs))
	errs := make([]error, len(dirs))
	var g errgroup.Group
	g.SetLimit(runtime.GOMAXPROCS(0))
	for i, d := range dirs {
		g.Go(func() error {
			p, err := ctx.ImportDir(d, 0)
			var noGo *build.NoGoError
			if errors.As(err, &noGo) {
				// テストだけのディレクトリは go list もファイルのないパッケージとして出す。
				// ビルドタグで全ファイルが外れるディレクトリは出さない
				if len(p.TestGoFiles) == 0 && len(p.XTestGoFiles) == 0 {
					return nil
				}
			} else if err != nil {
				// 複数のパッケージがある・import 文が読めないなど。エラーの形は go list に任せる
				errs[i] = err
				return nil
			}
			rel, err := filepath.Rel(modRoot, d)
			if err != nil || strings.HasPrefix(rel, "..") {
				errs[i] = errScanUnsupported
				return nil
			}
			importPath := mf.Module.Mod.Path
			if rel != "." {
				importPath += "/" + filepath.ToSlash(rel)
			}
			lp := &listedPackage{
				ImportPath: importPath,
				Name:       p.Name,
				Dir:        d,
				GoFiles:    p.GoFiles,
				CgoFiles:   p.CgoFiles,
				Imports:    p.Imports,
			}
			lp.Module = &listedModule{Path: mf.Module.Mod.Path, Main: true, Dir: modRoot, GoMod: env.GOMOD, GoVersion: goVersion}
			out[i] = lp
			return nil
		})
	}
	_ = g.Wait()
	var pkgs []*listedPackage
	for i, lp := range out {
		if errs[i] != nil {
			return nil, errScanUnsupported
		}
		if lp != nil {
			pkgs = append(pkgs, lp)
		}
	}
	// go list と同じく import パスの順にする
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].ImportPath < pkgs[j].ImportPath })
	return pkgs, nil
}

// packageDirs は ./... が辿るディレクトリを返す。go list と同じく、. や _ で始まるもの・testdata・vendor・
// 別のモジュール（go.mod のあるディレクトリ）は辿らず、シンボリックリンクも辿らない。
func packageDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root {
			name := d.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
		}
		dirs = append(dirs, path)
		return nil
	})
	return dirs, err
}

// buildTags は GOFLAGS の -tags を返す。ファイルの一覧に効くかもしれない他のフラグがあれば ok を false にする。
func buildTags(goflags string) ([]string, bool) {
	var tags []string
	for _, f := range strings.Fields(goflags) {
		name, value, _ := strings.Cut(strings.TrimLeft(f, "-"), "=")
		switch name {
		case "tags":
			tags = append(tags, strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' })...)
		case "modcacherw", "trimpath", "buildvcs", "v", "x", "p":
		case "mod":
			if value == "vendor" {
				return nil, false
			}
		default:
			return nil, false
		}
	}
	return tags, true
}

// releaseTagsOf は go env GOVERSION（go1.26.4 など）から go1.1 〜 go1.N のリリースタグを作る。
// mutcheck をビルドした Go と go コマンドの Go が違っても、go コマンドと同じタグでファイルを選ぶため。
func releaseTagsOf(goVersion string) ([]string, bool) {
	v, ok := strings.CutPrefix(goVersion, "go1.")
	if !ok {
		return nil, false
	}
	// go1.26.4 / go1.27rc1 / go1.26-devel_xxx などの、マイナー番号まで
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	minor, err := strconv.Atoi(v[:end])
	if err != nil || minor < 1 {
		return nil, false
	}
	tags := make([]string, 0, minor)
	for i := 1; i <= minor; i++ {
		tags = append(tags, "go1."+strconv.Itoa(i))
	}
	return tags, true
}
