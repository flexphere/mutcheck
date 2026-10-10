# mutcheck (PoC)

Go のコードから Spanner の `ReadWriteTransaction` を辿り、トランザクションごとの mutation 数を schema.sql から見積もる。

## インストール

Go 1.25 以上が必要。

```bash
go install github.com/flexphere/mutcheck@latest
mutcheck -version
```

`$(go env GOPATH)/bin`（`GOBIN` を設定していればそこ）に入る。ソースから使うときは `go build -o mutcheck .` でビルドする。

## 使い方

以下の例はリポジトリでビルドした `./mutcheck` で書いている。`go install` したときは `mutcheck` に読み替える。

```bash
# 全体
./mutcheck -schema path/to/schema.sql -dir path/to/module

# DB が複数ある場合は schema をカンマ区切りで指定する
./mutcheck -schema db1/schema.sql,db2/schema.sql -dir path/to/module

# 表示をトランザクションを含む関数・ファイルで絞り込む
./mutcheck -schema ... -dir ... -func 'CreateOrder'
./mutcheck -schema ... -dir ... -file internal/order/

# テーブルの単価だけ表示する
./mutcheck -schema ... -tables Orders,OrderItems

# 全テーブルの単価を表示する（schema ファイルの指定順、ファイル内ではテーブル名順）
./mutcheck -schema ... -tables all

# 取りこぼしの確認（後述）
./mutcheck -schema ... -dir ... -coverage

# Spanner Omni での実測との突き合わせ（後述）
./mutcheck -schema ... -dir ... -omnicheck omnicheck.json

# AI Agent や CI から使う（JSON で出し、超過・要確認があれば終了コード 3）
./mutcheck -schema ... -dir ... -format json -fail-on over,check
```

- `-v`: 内訳（書き込みごとの単価と回数）、単価の内訳、注記（未追跡の呼び出し、schema にないテーブルなど）を表示する
- `-tables <names>|all`: テーブルごとの単価の表を出して終了する。schema ファイルごとに見出しを付け、列は `columns`（生成列を含む列数）/ `indexes` / `fks`（外部キー。どのテーブルにもなければ出さない）/ `insert` / `update`（全列）/ `delete`（1 + indexes + fks）。`ON DELETE CASCADE` の子孫は親の行の下に字下げして、親の削除で子の行1行あたりに足される分（子のインデックス + 外部キー）を出す

  ```text
  === schema.sql
  table       columns  indexes   insert   update   delete
  Orders           12        3       15       18        4
    ON DELETE CASCADE  OrderItems  +2 × rows(OrderItems←Orders)
  ```

  `rows(子←親)` は解析結果の動的件数と同じ名前で、親の削除で消える子の行数を表す

- `-color auto|always|never`: 色を付けるか（既定は `auto`。標準出力がターミナルで `NO_COLOR` がないときだけ付ける）。`less -R` で読むときは `always`
- `-dump <regexp>`: デバッグ用。一致する関数の SSA を出力する
- `-coverage`: どのトランザクションからも辿り着かない書き込み箇所と、`ReadWriteTransaction` の外でのコミットを一覧にする
- `-omnicheck <json>`: omnicheck ライブラリが出力した JSON を読み、実測値と突き合わせる
- `-format text|json`: 出力形式（既定は `text`）。`json` の中身は後述。`-tables` と `-coverage` も JSON で出せる（`-omnicheck` と `-dump` はテキストのみ）
- `-fail-on <判定>`: 指定した判定（カンマ区切りで `fixed` / `dynamic` / `over` / `check`）のトランザクションが1件でもあれば、終了コード 3 で終わる。`-func` / `-file` で絞ったときは、表示したトランザクションだけを見る
- `-version`: バージョンを表示して終了する（`go install` で入れたときはモジュールのバージョン、ソースからビルドしたときは `(devel)` とコミット）
- `-debug`: 計測結果（各段階の所要時間、読み込んだパッケージ数と型エラーの件数など）を stderr に出す
- stdout には結果だけを出す。`-fail-on` に当たった件数とエラーは stderr に出す
- パッケージの読み込み・型チェックでエラーがあれば、`-debug` がなくても警告として stderr に出す。エラーのあるパッケージは SSA にしないので、その中のトランザクションや書き込みは解析から漏れる
- `-format json` のときは、型チェックのエラーを JSON の `load_errors` に入れ、`-fail-on` に当たった件数も stderr に出さない（件数は `summary` で分かる）。stdout と stderr を混ぜて読む AI Agent でも、終了コードが 0 か 3 なら出力全体を JSON として読める（`-debug` の計測結果は stderr に出る）
- 終了コード: 0 は正常、1 は schema やパッケージの読み込みの失敗、2 は引数の誤り、3 は `-fail-on` に当たるトランザクションがある。`-fail-on` を付けなければ、解析の結果（`超過` や `要確認` があるか）では変わらない
- 読み込みは常にモジュール全体（`./...`）。interface の実装が読み込み範囲から漏れると 0 になってしまうため、`-func` / `-file` は表示の絞り込みにだけ使う

## 出力

トランザクションごとに、合計の式・内訳（`-v`）・上限内の条件・動的件数の凡例を出す。上限の数え方が違うので、Commit と DML は分けて出す（[Spanner removes DML mutation limits](https://cloud.google.com/blog/products/databases/spanner-removes-dml-mutation-limits)、2026-09-10）。

`-v` を付けたときの例:

```text
■ OrderService.Place  [件数次第]
  app/order.go:42

  Commit  12 + 9·items / 80,000
    OrderRepository.Create  order.go:45
      insert  Orders  12 × 1  orders.yo.go:120
      insert  OrderItems  9 × items  order_repository.go:31
        └ 列 7 + インデックス 2
    ◇ 条件付き（if  order.go:50）
      [if req.Notify]
        ...

  上限内の条件
    items ≤ 8,887

  DML（1文ごとに上限 80,000）
    DML UPDATE  Stock  3·rows(Stock)  stock_repository.go:88
      → rows(Stock) ≤ 26,666

  動的件数
    items        len(items) 回のループ  order_repository.go:29
    rows(Stock)  Stock の影響を受ける行数  stock_repository.go:88

...

12 件: 固定 5 件 / 件数次第 6 件 / 超過 0 件 / 要確認 1 件（注記あり 3 件）

--- 要確認（mutation 数が 0） ---
app/report.go:20  ReportService.Load
    書き込みが見つからない（読み取りのみ、または追跡漏れ）
```

トランザクションはファイル順に出し、最後に判定ごとの件数と、要確認の一覧を出す。

- 見出しの判定: `固定`（動的件数なし）/ `件数次第`（動的件数あり）/ `超過`（定数部分だけで上限を超える）/ `要確認`（mutation 数が 0）
- `Commit`: Commit で送る mutation（Mutation API）の合計。1 コミットあたり 80,000 の上限がかかる
- 内訳（`-v` のときだけ）: 呼び出しをたどって、書き込みごとに「操作・テーブル・1回あたりの単価 × 回数・位置」と単価の内訳（`└ 列 7 + インデックス 2`）を出す。`-v` がなければ合計・上限内の条件・DML・動的件数だけを出す
- 呼び出しの段: SSA が作るラッパー（値レシーバのメソッドをポインタから呼ぶときのラッパー、メソッド値の `$bound` など）はソースにないので段にせず、中身を呼び出し元に直接つなぐ
- `◇`: 排他的な分岐（`if` / `switch`、interface の実装、値の合流など）。合計には分岐の項ごとの大きい方を使う。どの分岐にもある書き込みは分岐の外に出す。書き込みのない経路もある分岐は `条件付き` と出す
- `DML`: 文ごとに出す。DML は文ごとに 80,000 の上限がかかり、トランザクション全体では数えない。ループの中で実行しても回数は掛けない。`→` のあとに、その文が上限内に収まる条件を `上限内の条件` と同じ形で出す
- `上限内の条件`: 動的件数が1つなら上限に達する件数。複数なら不等式と、ほかの動的件数が 0 のときの各動的件数の上限（積でない項にある動的件数だけ）。位置だけが違う同じ名前の動的件数をまとめると1つになる場合は、その上限も推定として出す
- `動的件数`: ループの回数・DML で影響を受ける行数・CASCADE で消える子の行数など、実行時に決まる件数。式では短い名前で書き、凡例で位置と対応づける。同じ名前で位置が違うものには `#2`、`#3` を付ける

## 要確認

mutation 数が 0 のトランザクションは `⚠ 要確認` として出し、最後に一覧にする。理由は2種類ある。

- 書き込みが見つからない: 読み取りしかしていないか、書き込みへの経路を追えていない。読み取りだけなら `ReadOnlyTransaction` で済む
- 書き込みはあるが mutation 数が 0: `BufferWrite` や DML はあるが、渡された値を辿れていない。ツールの取りこぼしの可能性が高い

## JSON 出力（`-format json`）

AI Agent や CI から読むための出力。テキスト出力と同じ解析結果を構造化して出す。フィールド名と判定の値は互換性を保つ。

```json
{
  "limit": 80000,
  "transactions": [
    {
      "func": "OrderService.Place",
      "func_full": "(*example.com/app.OrderService).Place",
      "pos": "app/order.go:42",
      "verdict": "dynamic",
      "commit": {
        "expr": "12 + 9·items",
        "const": 12,
        "terms": [{ "coef": 9, "syms": ["items"], "max": 8887 }],
        "headroom": 79988,
        "limits": ["items ≤ 8,887"]
      },
      "dml": [
        {
          "op": "DML UPDATE",
          "table": "Stock",
          "pos": "app/stock_repository.go:88",
          "cost": { "expr": "3·rows(Stock)", "const": 0, "terms": [...], "headroom": 80000, "limits": ["rows(Stock) ≤ 26,666"] }
        }
      ],
      "symbols": [
        { "name": "items", "desc": "len(items) 回のループ", "pos": "app/order_repository.go:29" },
        { "name": "rows(Stock)", "desc": "Stock の影響を受ける行数", "pos": "app/stock_repository.go:88" }
      ],
      "notes": [],
      "callers": [
        { "pos": "app/order_handler.go:31", "func": "OrderHandler.Post" }
      ]
    }
  ],
  "summary": { "total": 1, "fixed": 0, "dynamic": 1, "over": 0, "check": 0, "with_notes": 0 },
  "coverage": { "unreached": [...], "outside": [...] },
  "load_errors": [
    { "package": "example.com/app/scripts/foo", "pos": "scripts/foo/main.go:41:3", "message": "x.Do undefined (...)" }
  ]
}
```

- `limit`: Commit 1回、および DML 1文あたりの mutation 数の上限
- `func_full`: `-func` が照合する関数の完全な名前。正規表現の記号をエスケープし `^...$` で囲んで `-func` に渡すと、この関数のトランザクションだけを表示できる
- `verdict`: 判定。`fixed`（固定）/ `dynamic`（件数次第）/ `over`（超過）/ `check`（要確認）。`check` のときは `check_reason` に理由を入れる
- `commit` / `dml[].cost`: mutation 数の式
  - `expr`: テキスト出力と同じ式
  - `const` と `terms`: 定数項と、動的件数の項（係数 `coef` × `syms` の積）
  - `headroom`: 上限から定数項を引いた残り。`terms` の合計がこれ以下なら上限内
  - `terms[].max`: ほかの項が 0 のときに、その項の動的件数（の積）がとれる最大値（`headroom / coef`）
  - `limits`: テキスト出力の「上限内の条件」と同じ文
- `dml`: DML の文ごと。`op` / `table` / `pos` は文の書き込み先
- `symbols`: 動的件数の凡例。式の中の名前（`name`）と、何の件数か（`desc`）・位置（`pos`）
- `pos`: どこでも `-dir` からの相対パスと行（`app/order.go:42`）。テキスト出力は読みやすさのためファイル名だけにしているが、JSON はそのままファイルを開けるようにパスを付ける
- `notes`: 注記。テキスト出力と違い `-v` がなくても出す
- `callers`: トランザクションを始める関数（クロージャなら外側の関数）を直接呼んでいる箇所。1 段だけ辿る。interface 経由の呼び出しは、メソッド名が同じで受け手の型がその interface を実装しているものを拾うので、実際には別の実装が呼ばれる箇所も含みうる
- `tree`（`-v` のときだけ）: 内訳の木。`kind` が `write`（書き込み。`unit` が1回あたりの mutation 数）/ `seq`（順に実行。合計は足し算）/ `alt`（排他的な分岐。`branches` が各分岐のラベルで、合計は項ごとの最大値。`optional` は書き込みのない経路もあること）/ `call`（呼び出し）/ `loop`（`sym` の回数だけ繰り返す）。どの節にも合計 `total` を入れる。DML の文ごとの木は `dml[].tree`
- `coverage`（`-coverage` のときだけ）: 網羅チェックの結果。`unreached`（辿り着かない書き込み。`hint` が理由の手がかり）と `outside`（`ReadWriteTransaction` の外でのコミット。`Apply` は `cost` に mutation 数）
- `load_errors`: パッケージの読み込み・型チェックのエラー（`package` / `pos` / `message`。位置のないエラーは `pos` なし）。エラーのあるパッケージの中のトランザクションは `transactions` に入らないので、空でなければ結果に取りこぼしがある

`-tables` を JSON で出すと、schema ファイルごとにテーブルの単価を出す。`cascade` は `ON DELETE CASCADE` で消える子で、親の削除に子の行1行あたり `per_row` が足される（`sym` は解析結果の動的件数と同じ名前）。孫は子の `cascade` に入れる。

```json
{
  "schemas": [
    {
      "path": "schema.sql",
      "tables": [
        {
          "name": "Orders", "columns": 12, "indexes": 3, "fks": 0, "insert": 15, "update": 18, "delete": 4,
          "cascade": [{ "table": "OrderItems", "per_row": 2, "sym": "rows(OrderItems←Orders)", "cascade": [] }]
        }
      ]
    }
  ],
  "missing": []
}
```

## 網羅チェック（`-coverage`）

モジュール内の書き込み箇所（`BufferWrite`・DML の `Update`・`BatchUpdate`）のうち、どのトランザクションの解析でも辿り着かなかったものを一覧にする。要確認（トランザクションから見て書き込みがない）の逆向きの確認で、取りこぼしを両側から見る。

辿り着かなかった理由の手がかりとして、次のどれかを付ける。

- 呼び出し元はあるがトランザクションから辿れない: ツールの取りこぼしの可能性が高い
- interface 経由の呼び出しはあるがトランザクションから辿れない: 同上
- interface 経由でも呼ばれていない / 呼び出し元なし: 未使用のコードの可能性

`Client.Apply` / `BatchWrite` のように `ReadWriteTransaction` を使わずにコミットする箇所は、集計対象外として別枠で出す（`Apply` は mutation 数も出す）。`-func` / `-file` で表示を絞っても、網羅チェックは全トランザクションを解析した上で行う。

## Spanner Omni との突き合わせ（`-omnicheck`）

`omnicheck` ライブラリで書き込み処理を Spanner Omni 上で実際に動かして記録した JSON を読み、次の3つを突き合わせる。

- 静的な式に、実際の件数を代入した値
- 実際に送られた mutation から、計算式で計算した値（単価の計算式の検証）
- `CommitStats` の実測値

```bash
./mutcheck -schema path/to/schema.sql -dir path/to/module -omnicheck omnicheck.json
```

判定は次の3つ。実測値があればそれと比べる。

- 一致: 静的な式の値が実測値と同じ
- 上限内: 静的な式の方が大きい（通らなかった `if` の分も足しているため）
- 過小評価: 静的な式の方が小さい。ツールの取りこぼしなので要調査

あわせて「計算式は実測と一致」かどうかで、単価の計算式そのものが正しいかを出す。JSON の作り方は `omnicheck/README.md`。

## 数え方

単価は Spanner Omni（`2026.r4-lts`）の `CommitStats` で実測して確かめた式（公式 quotas の [Note 3](https://docs.cloud.google.com/spanner/quotas#note3) / [Note 10](https://docs.cloud.google.com/spanner/quotas#note10) をもとに、当てはまる条件とインデックスの数え方を実測に合わせている）。

テーブル本体（1行あたり）:

- サーバー側で値を計算する列がない: 書き込んだ列 ∪ 主キー（Note 3）
- サーバー側で値を計算する列がある（insert の生成列、値を省略した DEFAULT 列、update で再計算される生成列）
  - Mutation API: 書き込んだ列 + 生成列 + すべての DEFAULT 列 + 主キー（Note 10。重複も数える）
  - DML: 書き込んだ列 ∪ 計算される列 ∪ 主キー（重複は数えない）

インデックス:

- insert: 1本につき 1（`NULL_FILTERED` で値が NULL なら実際は 0 だが、上限として数える）
- update: 値が変わりうるインデックス（更新する列か再計算される生成列をキー・STORING に含むもの。主キーは除く）1本につき 2。値が変わらなければ実際は 0 だが、上限として数える
- `InsertOrUpdate` / `Replace`: insert と update の大きい方
- delete: 1本につき 1（外部キーの backing index も数える）

そのほか:

- yo の `Insert()` / `Update()` は生成列以外の全列を書き込む
- delete: `1 + インデックス本数`。範囲削除と DML は `rows(Table@pos)` を掛ける
- `ON DELETE CASCADE` で消える子孫テーブルは、子の行そのものは数えず、子のインデックス分を `インデックス数 × rows(子←親)` として足す（Omni で実測して確認）
- commit timestamp の列は普通の列と同じ
- ループの中の呼び出しは、ループの回数を動的件数として掛ける（内部では `len(x)@pos` / `for@pos`。表示では `x` のような短い名前にして、位置は凡例に出す）
- interface 呼び出しは、モジュール内の実装（mock パッケージを除く）の項ごとの最大値をとる
- 関数値の呼び出し（トランザクション本体を含む）は、引数なら呼び出し元で渡された関数、捕捉変数なら外側で代入された関数、フィールドならそのフィールドに代入された関数を候補にし、項ごとの最大値をとる
- mutation は作った場所ではなく `BufferWrite` に渡された値を遡って数える（作ったが書き込まない mutation は数えず、トランザクション本体の外で作った mutation も数える）。遡るときは、slice リテラル・`append`・`make` と添字代入・戻り値（複数戻り値を含む）・引数（呼び出し元ごとの最大値）・クロージャの捕捉変数を辿る。分岐の合流点（SSA の Phi）は最大値をとり、ループで戻ってくる経路はループに入る前の値に足す
- 順に実行される文は足し、if/else・switch・select の分岐は項ごとの最大値をとる
- 複数の schema に同名テーブルがある場合は、どの DB への書き込みか静的に決まらないので、項ごとの最大値をとって注記を出す

## 既知の制限

- 分岐の最大値は項ごとにとるので、どの単一の経路よりも大きくなることがある（上限としての見積もり）
- update のインデックスは「値が変わる」として数えるので、値が変わらない更新では大きめに出る
- 単価は Spanner Omni での実測。本番の Spanner とまったく同じだという保証はない
- `if err != nil { return }` のような早期 return のあとの文も足すので、大きめに出る
- `BatchUpdate`、`Apply`、map やグローバル変数に入れた mutation・関数値は追跡しない

## 開発

go と golangci-lint のバージョンは `mise.toml` で揃える。CI（`.github/workflows/ci.yml`）も push・PR のたびに `mise run check` を実行する。

```bash
mise install      # go と golangci-lint を入れる
mise run test     # テスト
mise run lint     # golangci-lint（設定は .golangci.yml）
mise run check    # lint と test（CI と同じ）
mise run build    # ./mutcheck をビルド
```

本体と `omnicheck` は別 module なので、`test` と `lint` はそれぞれで実行する。`testdata/fixture` と `omnicheck/example` は解析対象のサンプルなので対象外。

workflow が使う action は、[gh-actions-lock](https://github.com/github/gh-actions-lock) でコミットに固定している（`.github/workflows/actions.lock`）。workflow の `uses` を追加・変更したときは、拡張を入れて lockfile を更新する。

```bash
gh extension install github/gh-actions-lock
gh actions-lock            # workflow を走査して lockfile を更新する
gh actions-lock --relock   # 固定しているバージョンを上流の最新コミットに上げる
```

`testdata/fixture` に書き方のパターンを1関数1トランザクションで並べ、`analyze_test.go` に期待値を書いている。
