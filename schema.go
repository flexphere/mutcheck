package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/cloudspannerecosystem/memefish"
	"github.com/cloudspannerecosystem/memefish/ast"
)

type Column struct {
	Name       string
	Generated  bool
	HasDefault bool
	// Deps は生成列の式が参照している列（小文字）。生成列以外は空
	Deps map[string]bool
}

type Index struct {
	Name         string
	Columns      map[string]bool // インデックスのキー列 ∪ テーブルの主キー ∪ STORING 列
	Own          map[string]bool // インデックスのキー列 ∪ STORING 列（主キーは含めない）。update で値が変わるかの判定に使う
	NullFiltered bool
}

type Table struct {
	Name     string
	Columns  []*Column
	PK       []string
	Indexes  []*Index
	FKCount  int    // 外部キーの backing index として数える
	Parent   string // INTERLEAVE IN PARENT
	Cascade  bool
	Children []*Table
	Source   string // 定義元の schema ファイル
}

// Schema は複数の schema ファイルのテーブルを保持する。DB が異なれば同名のテーブルがありうるので、名前ごとに複数持つ。
type Schema struct {
	Tables map[string][]*Table
	// Paths は読み込んだ schema ファイル（指定した順）
	Paths []string
}

func LoadSchemas(paths []string) (*Schema, error) {
	s := &Schema{Tables: map[string][]*Table{}}
	for _, p := range paths {
		f, err := loadSchemaFile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		for k, t := range f.Tables {
			s.Tables[k] = append(s.Tables[k], t)
		}
		s.Paths = append(s.Paths, p)
	}
	return s, nil
}

// AllTables は全テーブルを、schema ファイルの指定順、ファイル内ではテーブル名順に返す。
func (s *Schema) AllTables() []*Table {
	var out []*Table
	for _, p := range s.Paths {
		var ts []*Table
		for _, list := range s.Tables {
			for _, t := range list {
				if t.Source == p {
					ts = append(ts, t)
				}
			}
		}
		sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
		out = append(out, ts...)
	}
	return out
}

func (s *Schema) Lookup(name string) []*Table {
	return s.Tables[strings.ToLower(name)]
}

// schemaFile は1つの schema ファイル内のテーブル。親子関係はファイル内で解決する。
type schemaFile struct {
	Tables map[string]*Table
}

func loadSchemaFile(path string) (*schemaFile, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ddls, err := memefish.ParseDDLs(path, string(src))
	if err != nil {
		return nil, fmt.Errorf("parse ddl: %w", err)
	}
	s := &schemaFile{Tables: map[string]*Table{}}
	var indexes []*ast.CreateIndex
	for _, d := range ddls {
		switch d := d.(type) {
		case *ast.CreateTable:
			t := &Table{Name: pathName(d.Name), Source: path}
			for _, c := range d.Columns {
				t.Columns = append(t.Columns, columnOf(c))
			}
			for _, k := range d.PrimaryKeys {
				t.PK = append(t.PK, k.Name.Name)
			}
			for _, tc := range d.TableConstraints {
				if _, ok := tc.Constraint.(*ast.ForeignKey); ok {
					t.FKCount++
				}
			}
			if d.Cluster != nil {
				t.Parent = pathName(d.Cluster.TableName)
				t.Cascade = d.Cluster.OnDelete == ast.OnDeleteCascade
			}
			s.Tables[strings.ToLower(t.Name)] = t
		case *ast.AlterTable:
			t := s.Table(pathName(d.Name))
			if t == nil {
				continue
			}
			switch a := d.TableAlteration.(type) {
			case *ast.AddColumn:
				t.Columns = append(t.Columns, columnOf(a.Column))
			case *ast.AddTableConstraint:
				if _, ok := a.TableConstraint.Constraint.(*ast.ForeignKey); ok {
					t.FKCount++
				}
			}
		case *ast.CreateIndex:
			indexes = append(indexes, d)
		}
	}
	for _, d := range indexes {
		t := s.Table(pathName(d.TableName))
		if t == nil {
			continue
		}
		idx := &Index{Name: pathName(d.Name), Columns: map[string]bool{}, Own: map[string]bool{}, NullFiltered: d.NullFiltered}
		for _, k := range d.Keys {
			idx.Own[strings.ToLower(k.Name.Name)] = true
		}
		if d.Storing != nil {
			for _, c := range d.Storing.Columns {
				idx.Own[strings.ToLower(c.Name)] = true
			}
		}
		for c := range idx.Own {
			idx.Columns[c] = true
		}
		for _, pk := range t.PK {
			idx.Columns[strings.ToLower(pk)] = true
			// 主キーは update で変わらないので、値が変わるかの判定からは外す
			delete(idx.Own, strings.ToLower(pk))
		}
		t.Indexes = append(t.Indexes, idx)
	}
	for _, t := range s.Tables {
		if p := s.Table(t.Parent); p != nil {
			p.Children = append(p.Children, t)
		}
	}
	return s, nil
}

func columnOf(c *ast.ColumnDef) *Column {
	col := &Column{Name: c.Name.Name}
	switch d := c.DefaultSemantics.(type) {
	case *ast.GeneratedColumnExpr:
		col.Generated = true
		col.Deps = map[string]bool{}
		ast.Inspect(d.Expr, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				col.Deps[strings.ToLower(id.Name)] = true
			}
			return true
		})
	case *ast.ColumnDefaultExpr, *ast.IdentityColumn, *ast.AutoIncrement:
		col.HasDefault = true
	}
	return col
}

func pathName(p *ast.Path) string {
	if p == nil || len(p.Idents) == 0 {
		return ""
	}
	return p.Idents[len(p.Idents)-1].Name
}

func (s *schemaFile) Table(name string) *Table {
	return s.Tables[strings.ToLower(name)]
}

func (t *Table) WritableColumns() []string {
	var cols []string
	for _, c := range t.Columns {
		if !c.Generated {
			cols = append(cols, c.Name)
		}
	}
	return cols
}

// 以下の単価は Spanner Omni（2026.r4-lts）の CommitStats で実測して確かめた式。
// 公式 quotas の Note 3（影響する列数、主キーを含む）と Note 10（生成列・DEFAULT 列）をもとに、
// Note 10 が当てはまる条件とインデックスの数え方を実測に合わせている。

func lowerSet(cols []string) map[string]bool {
	set := map[string]bool{}
	for _, c := range cols {
		set[strings.ToLower(c)] = true
	}
	return set
}

// writeKind は書き込みの経路。Mutation API と DML で、サーバー側が値を計算する列の数え方が違う。
type writeKind int

const (
	viaMutation writeKind = iota
	viaDML
)

// rowPart はテーブル本体の1行あたりの mutation 数。computed はサーバー側で値を計算する列。
//   - computed がなければ Note 3 の「書き込んだ列 ∪ 主キー」
//   - Mutation API で computed があれば Note 10 の「書き込んだ列 + 生成列 + DEFAULT 列 + 主キー」（重複も数える）
//   - DML で computed があれば「書き込んだ列 ∪ computed ∪ 主キー」（重複は数えない）
func (t *Table) rowPart(written, computed []string, kind writeKind) int {
	set := lowerSet(written)
	if len(computed) > 0 && kind == viaMutation {
		var gen, def int
		for _, c := range t.Columns {
			if c.Generated {
				gen++
			}
			if c.HasDefault {
				def++
			}
		}
		return len(set) + gen + def + len(t.PK)
	}
	for _, c := range computed {
		set[strings.ToLower(c)] = true
	}
	for _, pk := range t.PK {
		set[strings.ToLower(pk)] = true
	}
	return len(set)
}

// insertComputed は insert でサーバー側が値を計算する列。生成列は常に計算され、DEFAULT 列は値を省略したときに計算される。
func (t *Table) insertComputed(written []string) []string {
	set := lowerSet(written)
	var out []string
	for _, c := range t.Columns {
		if c.Generated || (c.HasDefault && !set[strings.ToLower(c.Name)]) {
			out = append(out, c.Name)
		}
	}
	return out
}

// recomputed は update で再計算される生成列（式が更新する列を参照しているもの）を返す。
func (t *Table) recomputed(written []string) []string {
	set := lowerSet(written)
	var out []string
	for _, c := range t.Columns {
		if !c.Generated {
			continue
		}
		for d := range c.Deps {
			if set[d] {
				out = append(out, c.Name)
				break
			}
		}
	}
	return out
}

// touchedIndexes は update で値が変わりうるインデックスの本数。更新する列か、再計算される生成列をキー・STORING に含むものを数える。
func (t *Table) touchedIndexes(written []string) int {
	set := lowerSet(append(append([]string{}, written...), t.recomputed(written)...))
	n := 0
	for _, idx := range t.Indexes {
		for c := range idx.Own {
			if set[c] {
				n++
				break
			}
		}
	}
	return n
}

// insertParts は insert の1行あたりの単価を、テーブル本体の列の分とインデックスの分に分けて返す。
func (t *Table) insertParts(cols []string, kind writeKind) (row, idx int) {
	if cols == nil {
		cols = t.WritableColumns()
	}
	return t.rowPart(cols, t.insertComputed(cols), kind), len(t.Indexes)
}

// updateParts は update の1行あたりの単価を、テーブル本体の列の分とインデックスの分に分けて返す。
func (t *Table) updateParts(cols []string, kind writeKind) (row, idx int) {
	if cols == nil {
		cols = t.WritableColumns()
	}
	return t.rowPart(cols, t.recomputed(cols), kind), 2 * t.touchedIndexes(cols)
}

func (t *Table) insertCost(cols []string, kind writeKind) int {
	row, idx := t.insertParts(cols, kind)
	return row + idx
}

func (t *Table) updateCost(cols []string, kind writeKind) int {
	row, idx := t.updateParts(cols, kind)
	return row + idx
}

// InsertCost は新しい行を Mutation API で cols に insert するときのコスト。cols が nil なら生成列以外の全列（yo の Insert）。
// インデックスは1本につき1（NULL_FILTERED で値が NULL なら実際は 0 になるが、上限として数える）。
func (t *Table) InsertCost(cols []string) int { return t.insertCost(cols, viaMutation) }

// UpdateCost は Mutation API で cols を update するときのコスト。cols が nil なら生成列以外の全列（yo の Update）。
// 値が変わりうるインデックスは1本につき2（古いエントリの削除と新しいエントリの追加）。値が変わらなければ実際は 0 になるが、上限として数える。
func (t *Table) UpdateCost(cols []string) int { return t.updateCost(cols, viaMutation) }

// UpsertCost は InsertOrUpdate / Replace のコスト。行があるかどうかで insert か update になるので、大きい方をとる。
func (t *Table) UpsertCost(cols []string) int {
	return max(t.InsertCost(cols), t.UpdateCost(cols))
}

// DMLInsertCost / DMLUpdateCost は DML の INSERT / UPDATE の1行あたりのコスト。
// DML の UPDATE は SET に主キーを書かないが、主キーは影響する列として数える。
func (t *Table) DMLInsertCost(cols []string) int { return t.insertCost(cols, viaDML) }
func (t *Table) DMLUpdateCost(cols []string) int { return t.updateCost(cols, viaDML) }

// DeleteIndexCost は削除1行あたりにかかるインデックス分のコスト。テーブル本体の 1 は含まない。
func (t *Table) DeleteIndexCost() int {
	return len(t.Indexes) + t.FKCount
}

func partsDetail(row, idx int) string {
	if idx == 0 {
		return fmt.Sprintf("列 %d", row)
	}
	return fmt.Sprintf("列 %d + インデックス %d", row, idx)
}

// 以下は単価の内訳の説明。-v で表示する。

func (t *Table) InsertDetail(cols []string) string {
	return partsDetail(t.insertParts(cols, viaMutation))
}
func (t *Table) UpdateDetail(cols []string) string {
	return partsDetail(t.updateParts(cols, viaMutation))
}
func (t *Table) DMLInsertDetail(cols []string) string {
	return partsDetail(t.insertParts(cols, viaDML))
}
func (t *Table) DMLUpdateDetail(cols []string) string {
	return partsDetail(t.updateParts(cols, viaDML))
}

func (t *Table) UpsertDetail(cols []string) string {
	return fmt.Sprintf("insert（%s）と update（%s）の大きい方", t.InsertDetail(cols), t.UpdateDetail(cols))
}

func (t *Table) DeleteDetail() string {
	s := "行 1"
	if n := len(t.Indexes); n > 0 {
		s += fmt.Sprintf(" + インデックス %d", n)
	}
	if t.FKCount > 0 {
		s += fmt.Sprintf(" + 外部キー %d", t.FKCount)
	}
	return s
}
