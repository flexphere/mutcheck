// Package app は mutcheck のテスト用に、トランザクションの書き方のパターンを1関数1トランザクションで並べたもの。
// 期待値は analyze_test.go にある。
package app

import (
	"context"
	"slices"

	"cloud.google.com/go/spanner"
	"example.com/fixture/domain"
	"example.com/fixture/model"
)

func SimpleInsert(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		p := &model.Parent{}
		return tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx)})
	})
}

func UpdateAll(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		p := &model.Parent{}
		return tx.BufferWrite([]*spanner.Mutation{p.Update(ctx)})
	})
}

func GeneratedInsert(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		g := &model.Gen{}
		return tx.BufferWrite([]*spanner.Mutation{g.Insert(ctx)})
	})
}

func LoopInsert(ctx context.Context, c *spanner.Client, ps []model.Parent) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		var muts []*spanner.Mutation
		for _, p := range ps {
			muts = append(muts, p.Insert(ctx))
		}
		return tx.BufferWrite(muts)
	})
}

func NestedLoop(ctx context.Context, c *spanner.Client, ps []model.Parent, cs []model.Child) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		var muts []*spanner.Mutation
		for range ps {
			for _, ch := range cs {
				muts = append(muts, ch.Insert(ctx))
			}
		}
		return tx.BufferWrite(muts)
	})
}

func IfElse(ctx context.Context, c *spanner.Client, cond bool) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		if cond {
			p := &model.Parent{}
			return tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx)})
		} else {
			ch := &model.Child{}
			return tx.BufferWrite([]*spanner.Mutation{ch.Insert(ctx), ch.Insert(ctx)})
		}
	})
}

func Switch(ctx context.Context, c *spanner.Client, kind int) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		switch kind {
		case 1:
			p := &model.Parent{}
			return tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx)})
		case 2:
			ch := &model.Child{}
			return tx.BufferWrite([]*spanner.Mutation{ch.Insert(ctx)})
		}
		return nil
	})
}

func Sequence(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		p := &model.Parent{}
		if err := tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx)}); err != nil {
			return err
		}
		ch := &model.Child{}
		return tx.BufferWrite([]*spanner.Mutation{ch.Insert(ctx)})
	})
}

func Interface(ctx context.Context, c *spanner.Client, repo domain.Repository) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		return repo.Save(ctx, tx)
	})
}

func DMLDelete(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		stmt := spanner.NewStatement("DELETE FROM Parent WHERE OrgID = @orgID")
		stmt.Params["orgID"] = "x"
		_, err := tx.Update(ctx, stmt)
		return err
	})
}

func DMLUpdateStructLit(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		_, err := tx.Update(ctx, spanner.Statement{
			SQL:    "UPDATE Gen SET Phone = @phone WHERE ID = @id",
			Params: map[string]any{"phone": "x", "id": "y"},
		})
		return err
	})
}

func DMLInsertValues(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		_, err := tx.Update(ctx, spanner.NewStatement(
			"INSERT INTO Parent (OrgID, ParentID, Name, Status) VALUES ('a', 'b', 'c', 1), ('a', 'd', 'e', 1)"))
		return err
	})
}

func DeleteSingleKey(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		p := &model.Parent{}
		return tx.BufferWrite([]*spanner.Mutation{p.Delete(ctx)})
	})
}

func DeleteRange(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		return tx.BufferWrite([]*spanner.Mutation{
			spanner.Delete("Parent", spanner.KeyRange{Start: spanner.Key{"a"}, End: spanner.Key{"b"}}),
		})
	})
}

func ClosureToExternal(ctx context.Context, c *spanner.Client, ps []model.Parent) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		_ = slices.ContainsFunc(ps, func(p model.Parent) bool {
			_ = tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx)})
			return false
		})
		return nil
	})
}

func DirectClosure(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		save := func() error {
			ch := &model.Child{}
			return tx.BufferWrite([]*spanner.Mutation{ch.Insert(ctx)})
		}
		if err := save(); err != nil {
			return err
		}
		return save()
	})
}

func buildMutations(ctx context.Context) []*spanner.Mutation {
	p := &model.Parent{}
	ch := &model.Child{}
	return []*spanner.Mutation{p.Insert(ctx), ch.Insert(ctx)}
}

func HelperReturnsMutations(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		return tx.BufferWrite(buildMutations(ctx))
	})
}

func BodyAsVariable(ctx context.Context, c *spanner.Client) {
	body := func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		p := &model.Parent{}
		return tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx)})
	}
	c.ReadWriteTransaction(ctx, body)
}

// NestedTx は外側と内側で別のコミットになるので、外側には内側の書き込みを含めない。
func NestedTx(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
			ch := &model.Child{}
			return tx.BufferWrite([]*spanner.Mutation{ch.Insert(ctx)})
		})
		p := &model.Parent{}
		return tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx)})
	})
}

// OutsideBody はトランザクション本体の外で mutation を作る（既知の取りこぼし）。
func OutsideBody(ctx context.Context, c *spanner.Client) {
	p := &model.Parent{}
	muts := []*spanner.Mutation{p.Insert(ctx)}
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		return tx.BufferWrite(muts)
	})
}

type saver struct {
	save func(ctx context.Context, tx *spanner.ReadWriteTransaction) error
}

func newSaver() *saver {
	return &saver{save: func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		p := &model.Parent{}
		return tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx)})
	}}
}

// FuncValue はフィールドに入った関数値を呼ぶ（既知の取りこぼし）。
func FuncValue(ctx context.Context, c *spanner.Client) {
	s := newSaver()
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		return s.save(ctx, tx)
	})
}

// CreatedNotBuffered は作ったが BufferWrite しない mutation を含む。flow では数えない。
func CreatedNotBuffered(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		p := &model.Parent{}
		_ = p.Insert(ctx)
		ch := &model.Child{}
		return tx.BufferWrite([]*spanner.Mutation{ch.Insert(ctx)})
	})
}

func PrefilledAppend(ctx context.Context, c *spanner.Client, cs []model.Child) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		p := &model.Parent{}
		muts := []*spanner.Mutation{p.Insert(ctx)}
		for _, ch := range cs {
			muts = append(muts, ch.Insert(ctx))
		}
		return tx.BufferWrite(muts)
	})
}

func MakeIndexAssign(ctx context.Context, c *spanner.Client, ps []model.Parent) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		muts := make([]*spanner.Mutation, len(ps))
		for i, p := range ps {
			muts[i] = p.Insert(ctx)
		}
		return tx.BufferWrite(muts)
	})
}

func writeAll(tx *spanner.ReadWriteTransaction, muts []*spanner.Mutation) error {
	return tx.BufferWrite(muts)
}

func ParamMutations(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		p := &model.Parent{}
		return writeAll(tx, []*spanner.Mutation{p.Insert(ctx)})
	})
}

func buildWithErr(ctx context.Context) ([]*spanner.Mutation, error) {
	p := &model.Parent{}
	ch := &model.Child{}
	return []*spanner.Mutation{p.Insert(ctx), ch.Insert(ctx)}, nil
}

func TupleHelper(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		muts, err := buildWithErr(ctx)
		if err != nil {
			return err
		}
		return tx.BufferWrite(muts)
	})
}

func BufferInLoop(ctx context.Context, c *spanner.Client, ps []model.Parent) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		for _, p := range ps {
			if err := tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx)}); err != nil {
				return err
			}
		}
		return nil
	})
}

// job はメソッド値を引数で渡し、受け取った側のトランザクション本体で呼ぶ。
type job struct{}

func (j *job) finish(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
	p := &model.Parent{}
	return tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx)})
}

func (j *job) Run(ctx context.Context, c *spanner.Client) {
	j.withTx(ctx, c, j.finish)
}

func (j *job) withTx(ctx context.Context, c *spanner.Client, f func(ctx context.Context, tx *spanner.ReadWriteTransaction) error) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		return f(ctx, tx)
	})
}

// runTx はトランザクション本体を引数で受け取って、そのまま渡す。
func runTx(ctx context.Context, c *spanner.Client, body func(ctx context.Context, tx *spanner.ReadWriteTransaction) error) {
	c.ReadWriteTransaction(ctx, body)
}

func BodyParam(ctx context.Context, c *spanner.Client) {
	runTx(ctx, c, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		ch := &model.Child{}
		return tx.BufferWrite([]*spanner.Mutation{ch.Insert(ctx)})
	})
}

// PhiExclusive は分岐ごとに別の mutation を代入した変数を書き込む。合流点では最大値をとる。
func PhiExclusive(ctx context.Context, c *spanner.Client, found bool) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		var mut *spanner.Mutation
		if found {
			p := &model.Parent{}
			mut = p.Update(ctx)
		} else {
			ch := &model.Child{}
			mut = ch.Insert(ctx)
		}
		return tx.BufferWrite([]*spanner.Mutation{mut})
	})
}

// PhiSharedBase は共通の base に、片方の分岐でだけ追加する。共通部分を両方の経路で数える。
func PhiSharedBase(ctx context.Context, c *spanner.Client, more bool) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		p := &model.Parent{}
		muts := []*spanner.Mutation{p.Insert(ctx)}
		if more {
			ch := &model.Child{}
			muts = append(muts, ch.Insert(ctx))
		}
		return tx.BufferWrite(muts)
	})
}

// CapturedIndexAssign はトランザクション本体の外で make と添字代入をし、本体では BufferWrite だけをする。
func CapturedIndexAssign(ctx context.Context, c *spanner.Client, ps []model.Parent) {
	muts := make([]*spanner.Mutation, len(ps))
	for i := range ps {
		muts[i] = ps[i].Insert(ctx)
	}
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		return tx.BufferWrite(muts)
	})
	muts = nil
}

// ReadOnlyInRW は読み取りしかしない。要確認（書き込みが見つからない）になる。
func ReadOnlyInRW(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		_, err := tx.ReadRow(ctx, "Parent", spanner.Key{"a", "b"}, []string{"Name"})
		return err
	})
}

// UntraceableBuffer は map に入れた mutation を書き込む。要確認（書き込みはあるが 0）になる。
func UntraceableBuffer(ctx context.Context, c *spanner.Client, byKey map[string]*spanner.Mutation) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		return tx.BufferWrite([]*spanner.Mutation{byKey["a"]})
	})
}

// orphanWrite はどこからも呼ばれない。網羅チェックで未到達として出る。
func orphanWrite(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
	p := &model.Parent{}
	return tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx)})
}

// ApplyOutside は ReadWriteTransaction を使わずにコミットする。網羅チェックで別枠に出る。
func ApplyOutside(ctx context.Context, c *spanner.Client) {
	p := &model.Parent{}
	_, _ = c.Apply(ctx, []*spanner.Mutation{p.Insert(ctx)})
}

// DMLInLoop はループで DML を実行する。DML は文ごとの上限なので、回数は掛けない。
func DMLInLoop(ctx context.Context, c *spanner.Client, orgIDs []string) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		for _, id := range orgIDs {
			stmt := spanner.NewStatement("DELETE FROM Child WHERE OrgID = @orgID")
			stmt.Params["orgID"] = id
			if _, err := tx.Update(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
}

// MixedDMLAndMutation は mutation と DML を両方使う。Commit 側と DML 側に分けて数える。
func MixedDMLAndMutation(ctx context.Context, c *spanner.Client) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		if _, err := tx.Update(ctx, spanner.NewStatement("UPDATE Parent SET Status = 1 WHERE OrgID = 'a'")); err != nil {
			return err
		}
		p := &model.Parent{}
		return tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx)})
	})
}

// ValueReceiver は値レシーバで実装した interface を呼ぶ。SSA が作るポインタ版のラッパーは内訳の段にせず、候補も1つにまとまる。
func ValueReceiver(ctx context.Context, c *spanner.Client, u domain.Updater) {
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		return u.Update(ctx, tx)
	})
}

// MethodValue はメソッド値を関数の引数として渡す。SSA が作るラッパー（$bound）は内訳の段にしない。
func MethodValue(ctx context.Context, c *spanner.Client) {
	r := &parentWriter{}
	c.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		return callWithTx(ctx, tx, r.Save)
	})
}

type parentWriter struct{}

func (w *parentWriter) Save(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
	p := &model.Parent{}
	return tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx)})
}

func callWithTx(ctx context.Context, tx *spanner.ReadWriteTransaction, f func(context.Context, *spanner.ReadWriteTransaction) error) error {
	return f(ctx, tx)
}
