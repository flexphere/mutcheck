package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/gcexportdata"
)

// 外部の依存（標準ライブラリ・外部モジュール）の型は、go list -export が作る export data から読む。
// export data はビルドキャッシュの中のファイルで、go.mod / go.sum / Go のバージョンが同じなら変わらないので、
// ファイルの場所と import の関係をディスクに残し、次からは go list を経ずに直接読む。

// exportEntry は 1 パッケージの export data の場所と、そのパッケージが import するパッケージ（解決済みのパス）。
type exportEntry struct {
	Path    string   `json:"path"`
	Export  string   `json:"export"`
	Imports []string `json:"imports"`
}

type exportCache struct {
	Key      string        `json:"key"`
	Packages []exportEntry `json:"packages"`
}

// loadExportData は paths のパッケージの型を export data から読む。
// key は覚えておいた場所が使えるかの判定に使う値で、空なら覚えておいた場所を使わない。
func loadExportData(dir string, fset *token.FileSet, paths []string, key string) (map[string]*types.Package, error) {
	if len(paths) == 0 {
		return map[string]*types.Package{}, nil
	}
	cachePath := exportCachePath(dir)
	if key != "" {
		if entries := readExportCache(cachePath, key, paths); entries != nil {
			if ext, err := readExports(fset, entries, paths); err == nil {
				return ext, nil
			}
			// ビルドキャッシュが消されたなどで読めなければ、go list で場所を取り直す
		}
	}
	entries, cacheable, err := listExports(dir, paths)
	if err != nil {
		return nil, err
	}
	if key != "" && cacheable {
		writeExportCache(cachePath, key, entries)
	}
	return readExports(fset, entries, paths)
}

// listExports は go list -export で paths とその依存の export data の場所を得る（コンパイルはビルドキャッシュに当たる）。
// 手元のディレクトリにあるモジュール（解析対象のモジュール自身や、replace で手元を指すもの）は中身が変わるので、
// それを含むときは cacheable を false にする。
func listExports(dir string, paths []string) (map[string]exportEntry, bool, error) {
	args := append([]string{"list", "-e", "-deps", "-export", "-json=ImportPath,Export,Imports,ImportMap,Module"}, paths...)
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, false, fmt.Errorf("go list -export: %v: %s", err, stderr.String())
	}
	entries := map[string]exportEntry{}
	cacheable := true
	dec := json.NewDecoder(&stdout)
	for {
		var lp struct {
			ImportPath string
			Export     string
			Imports    []string
			ImportMap  map[string]string
			Module     *struct {
				Main    bool
				Replace *struct{ Version string }
			}
		}
		if err := dec.Decode(&lp); err == io.EOF {
			break
		} else if err != nil {
			return nil, false, fmt.Errorf("go list -export の出力を読めません: %v", err)
		}
		if lp.Module != nil && (lp.Module.Main || (lp.Module.Replace != nil && lp.Module.Replace.Version == "")) {
			cacheable = false
		}
		e := exportEntry{Path: lp.ImportPath, Export: lp.Export}
		for _, imp := range lp.Imports {
			if p, ok := lp.ImportMap[imp]; ok {
				imp = p
			}
			e.Imports = append(e.Imports, imp)
		}
		entries[lp.ImportPath] = e
	}
	return entries, cacheable, nil
}

// readExports は paths のパッケージを export data から読む。
// export data は参照している依存パッケージの型も含むので、読むのは paths だけでよい。
// 同じ型が同じ *types.Package になるように 1 つの imports を共有し、依存されるパッケージから先に読む。
func readExports(fset *token.FileSet, entries map[string]exportEntry, paths []string) (map[string]*types.Package, error) {
	want := map[string]bool{}
	for _, p := range paths {
		want[p] = true
	}
	imports := map[string]*types.Package{}
	out := map[string]*types.Package{}
	visited := map[string]bool{}
	var visit func(path string) error
	visit = func(path string) error {
		if visited[path] {
			return nil
		}
		visited[path] = true
		e, ok := entries[path]
		if !ok {
			return nil
		}
		for _, imp := range e.Imports {
			if err := visit(imp); err != nil {
				return err
			}
		}
		if !want[path] {
			return nil
		}
		if e.Export == "" {
			return fmt.Errorf("%s の export data がありません", path)
		}
		f, err := os.Open(e.Export)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		r, err := gcexportdata.NewReader(f)
		if err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		pkg, err := gcexportdata.Read(r, fset, imports, path)
		if err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		out[path] = pkg
		return nil
	}
	sorted := append([]string{}, paths...)
	sort.Strings(sorted)
	for _, p := range sorted {
		if err := visit(p); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// exportKey は覚えておいた export data の場所が使えるかの判定に使う値。
// go.mod / go.sum の中身と、go env の値（Go のバージョン・GOOS / GOARCH・GOFLAGS など）が同じなら同じになる。
// go.mod が見つからないなど判定できないときは空を返す（覚えておいた場所を使わない）。
func exportKey(env *goEnv, goMod string) string {
	if env == nil || goMod == "" {
		return ""
	}
	h := sha256.New()
	for _, path := range []string{goMod, strings.TrimSuffix(goMod, ".mod") + ".sum"} {
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return ""
		}
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
	}
	_, _ = h.Write(env.raw)
	return hex.EncodeToString(h.Sum(nil))
}

func exportCachePath(dir string) string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	abs, _ := filepath.Abs(dir)
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(base, "mutcheck", "export-"+hex.EncodeToString(sum[:8])+".json")
}

// readExportCache は覚えておいた場所を読む。key が違う、paths のどれかがない、ファイルが消えているときは nil を返す。
func readExportCache(path, key string, paths []string) map[string]exportEntry {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c exportCache
	if json.Unmarshal(b, &c) != nil || c.Key != key {
		return nil
	}
	entries := make(map[string]exportEntry, len(c.Packages))
	for _, e := range c.Packages {
		entries[e.Path] = e
	}
	for _, p := range paths {
		e, ok := entries[p]
		if !ok || e.Export == "" {
			return nil
		}
		if _, err := os.Stat(e.Export); err != nil {
			return nil
		}
	}
	return entries
}

// writeExportCache は場所を残す。書けなくても解析は続ける（次も go list するだけ）。
func writeExportCache(path, key string, entries map[string]exportEntry) {
	if path == "" {
		return
	}
	c := exportCache{Key: key}
	for _, e := range entries {
		c.Packages = append(c.Packages, e)
	}
	sort.Slice(c.Packages, func(i, j int) bool { return c.Packages[i].Path < c.Packages[j].Path })
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	// 途中まで書いたファイルを読まないように、別名で書いてから置き換える
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}
