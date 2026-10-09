// example は omnicheck の使い方の例。mutcheck の testdata/fixture の repository を実行する。
//
//	go run . -schema ../../testdata/fixture/schema.sql -out omnicheck.json
//	cd ../../testdata/fixture && mutcheck -schema schema.sql -omnicheck <path>/omnicheck.json
package main

import (
	"context"

	"cloud.google.com/go/spanner"
	"example.com/fixture/repo"
	"github.com/flexphere/mutcheck/omnicheck"
)

func main() {
	parentRepo := &repo.ParentRepository{}
	childRepo := &repo.ChildRepository{}
	omnicheck.Main([]omnicheck.Case{
		{
			Name: "ParentRepository.Save（1行 insert）",
			Func: "(*example.com/fixture/repo.ParentRepository).Save",
			Run: func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
				return parentRepo.Save(ctx, tx)
			},
		},
		{
			// Child は Parent に interleave しているので、前のケースで作った親の行の下に insert する
			Name: "ChildRepository.Save（interleave した子テーブルへの 1行 insert）",
			Func: "(*example.com/fixture/repo.ChildRepository).Save",
			Run: func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
				return childRepo.Save(ctx, tx)
			},
		},
	})
}
