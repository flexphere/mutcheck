// Package omnicheck は、書き込み処理を Spanner Omni 上で実際に動かし、
// Commit で送られた mutation・DML で影響を受けた行数・CommitStats の実測値を JSON に書き出す。
// 突き合わせは mutcheck 本体の -omnicheck で行う。
//
// 利用する側は Case を定義して Main を呼ぶ main パッケージを用意する。
package omnicheck

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"github.com/cloudspannerecosystem/memefish"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
)

// TxFunc はトランザクションの本体。
type TxFunc = func(ctx context.Context, tx *spanner.ReadWriteTransaction) error

// Case は1つの書き込み処理を1トランザクションで実行する。
type Case struct {
	Name string
	// Func は mutcheck が静的な式を出す関数の SSA 名（例: "(*example.com/app/repo.UserRepository).Create"）
	Func string
	// Symbols は式の記号（@位置を除いた名前。例: "len(users)"）に代入する値。rows(Table) は DML の実際の行数から mutcheck が補う
	Symbols map[string]int
	// Setup は事前データの投入。別のコミットで実行し、記録しない
	Setup TxFunc
	// Run は記録するトランザクション。ReturnCommitStats を付けて実行する
	Run TxFunc
}

// Result は1ケース分の記録。mutcheck 本体の omnicheck.go と同じ形にする。
type Result struct {
	Name      string         `json:"name"`
	Func      string         `json:"func"`
	Symbols   map[string]int `json:"symbols"`
	Mutations []Mutation     `json:"mutations"`
	DML       []DML          `json:"dml"`
	Error     string         `json:"error,omitempty"`
	// MutationCount は CommitStats の実測値
	MutationCount int64 `json:"mutation_count"`
}

type Mutation struct {
	Op      string   `json:"op"`
	Table   string   `json:"table"`
	Columns []string `json:"columns,omitempty"`
	Rows    int      `json:"rows,omitempty"`
	Keys    int      `json:"keys,omitempty"`
	Ranges  int      `json:"ranges,omitempty"`
}

type DML struct {
	SQL  string `json:"sql"`
	Rows int64  `json:"rows"`
}

// recorder は Commit と DML の RPC を記録する。
type recorder struct {
	mu        sync.Mutex
	mutations []Mutation
	dml       []DML
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mutations, r.dml = nil, nil
}

func (r *recorder) intercept(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	err := invoker(ctx, method, req, reply, cc, opts...)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch req := req.(type) {
	case *spannerpb.CommitRequest:
		// Abort でリトライされた場合は最後の Commit だけを残す
		r.mutations = nil
		for _, m := range req.Mutations {
			r.mutations = append(r.mutations, toMutation(m))
		}
	case *spannerpb.ExecuteSqlRequest:
		rs, ok := reply.(*spannerpb.ResultSet)
		if ok && rs.GetStats() != nil && isDML(req.Sql) {
			r.dml = append(r.dml, DML{SQL: req.Sql, Rows: rs.GetStats().GetRowCountExact()})
		}
	}
	return nil
}

func isDML(sql string) bool {
	_, err := memefish.ParseDML("", sql)
	return err == nil
}

func toMutation(m *spannerpb.Mutation) Mutation {
	write := func(op string, w *spannerpb.Mutation_Write) Mutation {
		return Mutation{Op: op, Table: w.Table, Columns: w.Columns, Rows: len(w.Values)}
	}
	switch op := m.Operation.(type) {
	case *spannerpb.Mutation_Insert:
		return write("insert", op.Insert)
	case *spannerpb.Mutation_Update:
		return write("update", op.Update)
	case *spannerpb.Mutation_InsertOrUpdate:
		return write("insert_or_update", op.InsertOrUpdate)
	case *spannerpb.Mutation_Replace:
		return write("replace", op.Replace)
	case *spannerpb.Mutation_Delete_:
		ks := op.Delete.KeySet
		return Mutation{Op: "delete", Table: op.Delete.Table, Keys: len(ks.GetKeys()), Ranges: len(ks.GetRanges())}
	}
	return Mutation{Op: "unknown"}
}

// dbPrefix は omnicheck が作る DB 名の接頭辞。後片付けではこの接頭辞の DB だけを消す
const dbPrefix = "omnicheck"

// Spanner Omni の single-server は project / instance が default
const instancePath = "projects/default/instances/default"

// omni は Spanner Omni への接続。
type omni struct {
	endpoint string
}

func (o omni) cfg() spanner.ClientConfig {
	return spanner.ClientConfig{Type: spanner.OMNI, UsePlainText: true}
}

func (o omni) adminClient(ctx context.Context) (*database.DatabaseAdminClient, error) {
	return database.NewDatabaseAdminClientWithConfig(ctx, o.cfg(), option.WithEndpoint(o.endpoint))
}

func (o omni) client(ctx context.Context, dbPath string, opts ...option.ClientOption) (*spanner.Client, error) {
	return spanner.NewClientWithConfig(ctx, dbPath, o.cfg(), append(opts, option.WithEndpoint(o.endpoint))...)
}

// Main はフラグを解釈し、cases を順に実行して結果を JSON に書き出す。
func Main(cases []Case) {
	schemaPath := flag.String("schema", "", "schema.sql のパス")
	out := flag.String("out", "omnicheck.json", "結果の出力先")
	cleanup := flag.Bool("cleanup", false, "omnicheck が作った DB（接頭辞 "+dbPrefix+"）が残っていれば削除して終了する")
	endpoint := flag.String("endpoint", "localhost:15000", "Spanner Omni の接続先")
	flag.Parse()
	o := omni{endpoint: *endpoint}
	// Omni での検証はこの設定（multiplexed session を無効）で行ったので、そのまま使う
	for _, k := range []string{"GOOGLE_CLOUD_SPANNER_MULTIPLEXED_SESSIONS", "GOOGLE_CLOUD_SPANNER_MULTIPLEXED_SESSIONS_FOR_RW", "GOOGLE_CLOUD_SPANNER_MULTIPLEXED_SESSIONS_PARTITIONED_OPS"} {
		if err := os.Setenv(k, "false"); err != nil {
			log.Fatal(err)
		}
	}

	ctx := context.Background()
	if *cleanup {
		cleanupDatabases(ctx, o)
		return
	}
	dbPath, err := createDatabase(ctx, o, *schemaPath)
	if err != nil {
		log.Fatal(err)
	}
	defer dropDatabase(ctx, o, dbPath)
	rec := &recorder{}
	client, err := o.client(ctx, dbPath, option.WithGRPCDialOption(grpc.WithChainUnaryInterceptor(rec.intercept)))
	if err != nil {
		log.Fatal(err)
	}

	opts := spanner.TransactionOptions{CommitOptions: spanner.CommitOptions{ReturnCommitStats: true}}
	var results []Result
	for _, c := range cases {
		res := Result{Name: c.Name, Func: c.Func, Symbols: c.Symbols}
		if c.Setup != nil {
			if _, err := client.ReadWriteTransaction(ctx, c.Setup); err != nil {
				res.Error = "setup: " + err.Error()
				results = append(results, res)
				continue
			}
		}
		rec.reset()
		resp, err := client.ReadWriteTransactionWithOptions(ctx, c.Run, opts)
		switch {
		case err != nil:
			res.Error = err.Error()
		case resp.CommitStats == nil:
			res.Error = "CommitStats が返らなかった"
		default:
			res.MutationCount = resp.CommitStats.MutationCount
		}
		res.Mutations, res.DML = rec.mutations, rec.dml
		results = append(results, res)
		fmt.Printf("%-55s mutations=%d dml=%d commit_stats=%d %s\n", c.Name, len(res.Mutations), len(res.DML), res.MutationCount, res.Error)
	}

	client.Close()

	b, _ := json.MarshalIndent(results, "", "  ")
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		log.Println(err)
		return
	}
	fmt.Println("wrote", *out)
}

// cleanupDatabases は omnicheck が作った DB をすべて削除する。異常終了で残った DB の掃除用。
func cleanupDatabases(ctx context.Context, o omni) {
	dc, err := o.adminClient(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = dc.Close() }()
	it := dc.ListDatabases(ctx, &databasepb.ListDatabasesRequest{Parent: instancePath})
	for {
		db, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		name := db.Name[strings.LastIndex(db.Name, "/")+1:]
		if !strings.HasPrefix(name, dbPrefix) {
			continue
		}
		dropDatabase(ctx, o, db.Name)
		fmt.Println("dropped", db.Name)
	}
}

func dropDatabase(ctx context.Context, o omni, dbPath string) {
	dc, err := o.adminClient(ctx)
	if err != nil {
		log.Println("drop database:", err)
		return
	}
	defer func() { _ = dc.Close() }()
	if err := dc.DropDatabase(ctx, &databasepb.DropDatabaseRequest{Database: dbPath}); err != nil {
		log.Println("drop database:", err)
	}
}

// createDatabase は毎回新しい DB を作り、schema を流す。
func createDatabase(ctx context.Context, o omni, schemaPath string) (string, error) {
	src, err := os.ReadFile(schemaPath)
	if err != nil {
		return "", err
	}
	ddls, err := memefish.ParseDDLs(schemaPath, string(src))
	if err != nil {
		return "", err
	}
	var stmts []string
	for _, d := range ddls {
		stmts = append(stmts, d.SQL())
	}

	dc, err := o.adminClient(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = dc.Close() }()
	db := fmt.Sprintf("%s%d", dbPrefix, time.Now().Unix())
	dop, err := dc.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{
		Parent:          instancePath,
		CreateStatement: "CREATE DATABASE `" + db + "`",
		ExtraStatements: stmts,
	})
	if err != nil {
		return "", err
	}
	if _, err := dop.Wait(ctx); err != nil {
		return "", err
	}
	return instancePath + "/databases/" + db, nil
}
