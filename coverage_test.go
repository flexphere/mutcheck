package main

import (
	"strings"
	"testing"
)

func TestCoverage(t *testing.T) {
	a, _ := loadFixture(t, fixtureSchema)
	unreached, outside := a.Coverage()

	if len(unreached) != 1 {
		for _, s := range unreached {
			t.Logf("未到達: %s %s", s.Fn, s.Call)
		}
		t.Fatalf("未到達 %d 件（orphanWrite の1件だけを期待）", len(unreached))
	}
	u := unreached[0]
	if u.Fn.Name() != "orphanWrite" || u.Call != "ReadWriteTransaction.BufferWrite" || !strings.Contains(u.Hint, "呼び出し元なし") {
		t.Errorf("未到達 = %s %s %q", u.Fn, u.Call, u.Hint)
	}

	if len(outside) != 1 {
		t.Fatalf("トランザクション外のコミット %d 件（ApplyOutside の1件だけを期待）", len(outside))
	}
	o := outside[0]
	if o.Fn.Name() != "ApplyOutside" || o.Call != "Client.Apply" || o.Cost == nil || costString(o.Cost.Cost) != "5" {
		t.Errorf("トランザクション外のコミット = %s %s %v", o.Fn, o.Call, o.Cost)
	}
}
