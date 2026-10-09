package repo

import (
	"context"

	"cloud.google.com/go/spanner"
	"example.com/fixture/model"
)

// ParentRepository は Parent を1行書く（7）。
type ParentRepository struct{}

func (r *ParentRepository) Save(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
	p := &model.Parent{}
	return tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx)})
}

// ChildRepository は Child を1行書く（8）。
type ChildRepository struct{}

func (r *ChildRepository) Save(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
	c := &model.Child{}
	return tx.BufferWrite([]*spanner.Mutation{c.Insert(ctx)})
}

// ValueRepository は値レシーバのメソッドで Parent を全列 update する（6）。
// ポインタで返すので、SSA はポインタ版のラッパー (*ValueRepository).Update を作る。
type ValueRepository struct{}

func NewValueRepository() *ValueRepository { return &ValueRepository{} }

func (r ValueRepository) Update(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
	p := &model.Parent{}
	return tx.BufferWrite([]*spanner.Mutation{p.Update(ctx)})
}
