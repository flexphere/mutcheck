# omnicheck

書き込み処理を Spanner Omni 上で実際に動かし、次のものを JSON に書き出すライブラリ。突き合わせは mutcheck 本体の `-omnicheck` で行う。

- Commit で送られた mutation（操作・テーブル・列・行数・キー数）
- DML で影響を受けた行数
- `CommitStats` の `MutationCount`（Spanner emulator は `CommitStats` を返さないので、Omni を使う）

mutcheck 本体とは別のモジュールなので、mutcheck は解析対象のアプリケーションに依存しない。

## 使い方

```bash
go get github.com/flexphere/mutcheck/omnicheck@latest
```

Go 1.26 以上が必要（依存する spanner クライアントの要件）。

解析対象のアプリケーション側に、ケースを定義して `omnicheck.Main` を呼ぶ `main` パッケージを用意する。Omni 用の設定には spanner クライアント v1.94 以上が必要なので、アプリケーションの spanner のバージョンが古い場合は、この `main` を独立したモジュールにする。

```go
package main

import (
	"context"

	"cloud.google.com/go/spanner"
	"github.com/flexphere/mutcheck/omnicheck"
)

func main() {
	repo := NewUserRepository()
	omnicheck.Main([]omnicheck.Case{
		{
			Name:    "UserRepository.CreateMulti",
			Func:    "(*example.com/app/repo.UserRepository).CreateMulti",
			Symbols: map[string]int{"len(users)": 3},
			Run: func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
				return repo.CreateMulti(ctx, tx, newUsers(3))
			},
		},
	})
}
```

- `Func`: mutcheck が静的な式を出す関数の SSA 名
- `Symbols`: 式の動的件数（`@位置` を除いた名前）に代入する値。`rows(Table)` は DML の実際の行数から mutcheck が補う
- `Setup`: 事前データの投入（別コミット。記録しない）
- `Run`: 記録するトランザクション（`ReturnCommitStats` を付けて実行する）

動く例は `example/`（mutcheck の `testdata/fixture` を使う）。

### フラグ

- `-schema`: DB に流す schema.sql
- `-out`: 結果の出力先（既定 `omnicheck.json`）
- `-endpoint`: Spanner Omni の接続先（既定 `localhost:15000`）
- `-cleanup`: 異常終了で残った DB（接頭辞 `omnicheck`）を削除して終了する

## Spanner Omni の準備

Omni のサーバーは Linux x86 版だけなので、Apple Silicon では Rosetta を有効にした colima プロファイルで動かす。

```bash
colima start --profile omni --vm-type vz --vz-rosetta --arch aarch64 --cpus 4 --memory 8 --disk 60
docker context use colima   # colima が現在のコンテキストを colima-omni に切り替えるので、元のコンテキストに戻す
docker --context colima-omni run -d --platform linux/amd64 --network host --name spanneromni \
  -v spanner:/spanner us-docker.pkg.dev/spanner-omni/images/spanner-omni:2026.r4-lts start-single-server
# ログに「Spanner is ready」が出るまで待つ（約 80 秒）
docker --context colima-omni logs spanneromni 2>&1 | grep 'Spanner is ready'

go run . -schema path/to/schema.sql -out omnicheck.json

# 使い終わったら VM ごと削除する
colima delete --profile omni --force
```

- 平文で接続し、project / instance は `default`
- Omni を使うには [Spanner Omni のライセンス条項](https://cloud.google.com/terms/spanner-omni-license-terms) への同意が必要

## 後片付け

- 実行のたびに新しい DB（`omnicheck<時刻>`）を作り、終わったら削除する
- 異常終了で DB が残った場合は `-cleanup` で、接頭辞 `omnicheck` の DB だけを削除する

## 結果の見方

- 一致: 静的な式に実際の件数を代入した値が、実測値と同じ
- 上限内: 静的な式の方が大きい。通らなかった `if` の分も足しているため（静的な式は上限として見積もる）
- 過小評価: 静的な式の方が小さい。ツールの取りこぼしなので要調査
- 計算式は実測と一致: 実際に送られた mutation から計算した値が `CommitStats` と同じ。単価の計算式が正しいことを表す

DML が2文以上あるケースは、実測値が全 DML の合計で、DML 1 文あたりの最大値とは比べられないので、実際の書き込みとの比較で判定する。
