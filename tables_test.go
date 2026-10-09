package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPrintTables(t *testing.T) {
	var b bytes.Buffer
	printTables(&b, mustLoadSchemas(t, fixtureSchema, fixtureDupSchema), "all", palette{})
	got := b.String()
	for _, w := range []string{
		"=== " + fixtureSchema + "\ntable   columns  indexes   insert   update   delete\n",
		"Parent        4        1        5        6        2\n  ON DELETE CASCADE  Child  +1 × rows(Child←Parent)\n",
		"=== " + fixtureDupSchema + "\n",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("%q がない\n%s", w, got)
		}
	}
	// fixture には外部キーがないので fks 列は出さない
	if strings.Contains(got, "fks") {
		t.Errorf("外部キーがないのに fks 列がある\n%s", got)
	}
}

// TestPrintTablesFKAndNestedCascade は、外部キーがあれば fks 列を出し、孫の CASCADE をもう一段字下げすることを確かめる。
func TestPrintTablesFKAndNestedCascade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.sql")
	ddl := `
CREATE TABLE A (ID STRING(36) NOT NULL) PRIMARY KEY (ID);
CREATE TABLE B (ID STRING(36) NOT NULL, BID STRING(36) NOT NULL) PRIMARY KEY (ID, BID), INTERLEAVE IN PARENT A ON DELETE CASCADE;
CREATE INDEX BByBID ON B (BID);
CREATE TABLE C (ID STRING(36) NOT NULL, BID STRING(36) NOT NULL, CID STRING(36) NOT NULL) PRIMARY KEY (ID, BID, CID), INTERLEAVE IN PARENT B ON DELETE CASCADE;
CREATE TABLE D (ID STRING(36) NOT NULL, AID STRING(36) NOT NULL, CONSTRAINT FK_DA FOREIGN KEY (AID) REFERENCES A (ID)) PRIMARY KEY (ID);
`
	if err := os.WriteFile(path, []byte(ddl), 0o644); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	printTables(&b, mustLoadSchemas(t, path), "all", palette{})
	got := b.String()
	for _, w := range []string{
		"table  columns  indexes      fks   insert   update   delete\n",
		"A            1        0        0        1        1        1\n  ON DELETE CASCADE  B    +1 × rows(B←A)\n    ON DELETE CASCADE  C  +0 × rows(C←B)\n",
		"D            2        0        1 ",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("%q がない\n%s", w, got)
		}
	}
}

// TestPrintTablesColor は色を付けても桁揃えが崩れず、色なしのときはエスケープを出さないことを確かめる。
func TestPrintTablesColor(t *testing.T) {
	s := mustLoadSchemas(t, fixtureSchema)
	var plain, colored bytes.Buffer
	printTables(&plain, s, "all", palette{})
	printTables(&colored, s, "all", palette{on: true})
	if strings.Contains(plain.String(), "\x1b[") {
		t.Error("色なしなのにエスケープがある")
	}
	if got := ansiRe.ReplaceAllString(colored.String(), ""); got != plain.String() {
		t.Errorf("色を外すと色なしの出力と一致しない\n%s\n---\n%s", got, plain.String())
	}
	for _, w := range []string{"\x1b[1mParent", "\x1b[31m       2\x1b[0m", "\x1b[36mON DELETE CASCADE\x1b[0m", "\x1b[31m+1 × rows(Child←Parent)\x1b[0m"} {
		if !strings.Contains(colored.String(), w) {
			t.Errorf("%q がない\n%s", w, colored.String())
		}
	}
}

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")
