package domain

import (
	"context"

	"cloud.google.com/go/spanner"
)

type Repository interface {
	Save(ctx context.Context, tx *spanner.ReadWriteTransaction) error
}

// Updater は値レシーバで実装する（repo.ValueRepository）。
type Updater interface {
	Update(ctx context.Context, tx *spanner.ReadWriteTransaction) error
}
