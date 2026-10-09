// Package mock は mockgen の生成物を模したもの。解析対象から除外されることを確認する。
package mock

import (
	"context"

	"cloud.google.com/go/spanner"
	"example.com/fixture/model"
)

type MockRepository struct{}

func (m *MockRepository) Save(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
	p := &model.Parent{}
	return tx.BufferWrite([]*spanner.Mutation{p.Insert(ctx), p.Insert(ctx), p.Insert(ctx)})
}
