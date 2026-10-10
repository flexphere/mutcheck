package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// go/build での走査が go list と同じ一覧を返すこと
func TestScanPackagesMatchesGoList(t *testing.T) {
	edge := t.TempDir()
	files := map[string]string{
		"go.mod":                 "module example.com/edge\n\ngo 1.25\n",
		"root.go":                "package edge\n\nimport _ \"example.com/edge/ok\"\n",
		"ok/a.go":                "package ok\n\nimport (\n\t\"fmt\"\n\t_ \"strings\"\n)\n\nvar _ = fmt.Sprint\n",
		"ok/b.go":                "package ok\n\nimport _ \"fmt\"\n",
		"ok/a_test.go":           "package ok\n\nimport _ \"testing\"\n",
		"ok/linux.go":            "//go:build never\n\npackage ok\n\nimport _ \"os\"\n",
		"testonly/a_test.go":     "package testonly\n",
		"excluded/a.go":          "//go:build never\n\npackage excluded\n",
		"docsonly/README":        "hi\n",
		"vendor/x/x.go":          "package x\n",
		"sub/vendor/y/y.go":      "package y\n",
		"nested/go.mod":          "module example.com/nested\n",
		"nested/a.go":            "package nested\n",
		"_skip/a.go":             "package skip\n",
		".hidden/a.go":           "package hidden\n",
		"testdata/a.go":          "package testdata\n",
		"deep/er/pkg/main.go":    "package main\n",
		"deep/er/pkg/old_go1.go": "//go:build go1.1\n\npackage main\n",
	}
	for name, content := range files {
		path := filepath.Join(edge, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, dir := range []string{fixtureDir, edge, filepath.Join(edge, "deep")} {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			scanned, err := scanPackages(dir, readGoEnv(dir))
			if err != nil {
				t.Fatalf("scanPackages: %v", err)
			}
			listed, err := runGoList(dir, false)
			if err != nil {
				t.Fatalf("go list: %v", err)
			}
			if got, want := summarize(scanned), summarize(listed); !reflect.DeepEqual(got, want) {
				t.Errorf("一覧が go list と違う\n got: %v\nwant: %v", got, want)
			}
		})
	}
}

// go list と同じ結果にならない構成では go list に任せる
func TestScanPackagesUnsupported(t *testing.T) {
	cases := map[string]map[string]string{
		"複数のパッケージ": {
			"go.mod":  "module example.com/multi\n\ngo 1.25\n",
			"m/a.go":  "package a\n",
			"m/b.go":  "package b\n",
			"ok/a.go": "package ok\n",
		},
		"vendor": {
			"go.mod":             "module example.com/vendored\n\ngo 1.25\n",
			"a.go":               "package a\n",
			"vendor/modules.txt": "",
		},
		"ignore ディレクティブ": {
			"go.mod": "module example.com/ignored\n\ngo 1.25\n\nignore ./node_modules\n",
			"a.go":   "package a\n",
		},
		"import 文が読めない": {
			"go.mod": "module example.com/broken\n\ngo 1.25\n",
			"a.go":   "package a\n\nimport (\n",
		},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range files {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := scanPackages(dir, readGoEnv(dir)); err != errScanUnsupported {
				t.Errorf("err = %v, want errScanUnsupported", err)
			}
		})
	}
}

func TestBuildTags(t *testing.T) {
	cases := []struct {
		goflags string
		want    []string
		ok      bool
	}{
		{"", nil, true},
		{"-tags=a,b -modcacherw", []string{"a", "b"}, true},
		{"-mod=mod -trimpath", nil, true},
		{"-mod=vendor", nil, false},
		{"-gcflags=-N", nil, false},
	}
	for _, c := range cases {
		got, ok := buildTags(c.goflags)
		if ok != c.ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("buildTags(%q) = %v, %v; want %v, %v", c.goflags, got, ok, c.want, c.ok)
		}
	}
}

func TestReleaseTagsOf(t *testing.T) {
	if got, ok := releaseTagsOf("go1.3.1"); !ok || !reflect.DeepEqual(got, []string{"go1.1", "go1.2", "go1.3"}) {
		t.Errorf("go1.3.1: %v, %v", got, ok)
	}
	if got, ok := releaseTagsOf("go1.27rc1"); !ok || len(got) != 27 {
		t.Errorf("go1.27rc1: %v, %v", got, ok)
	}
	if _, ok := releaseTagsOf("devel +abc"); ok {
		t.Error("devel は対応しない")
	}
}

type pkgSummary struct {
	ImportPath, Name, Dir string
	GoFiles, Imports      []string
	Module                listedModule
}

func summarize(pkgs []*listedPackage) []pkgSummary {
	out := make([]pkgSummary, 0, len(pkgs))
	for _, lp := range pkgs {
		// nil と空のスライスは区別しない
		s := pkgSummary{ImportPath: lp.ImportPath, Name: lp.Name, Dir: lp.Dir, GoFiles: append([]string{}, lp.GoFiles...), Imports: append([]string{}, lp.Imports...)}
		if lp.Module != nil {
			s.Module = *lp.Module
		}
		out = append(out, s)
	}
	return out
}
