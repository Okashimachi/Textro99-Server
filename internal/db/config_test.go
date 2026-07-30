package db

import (
	"context"
	"os"
	"testing"
	"time"

	"textro99/internal/game"
)

// TestConfigStore_RoundTrip は実 Postgres に対する統合テスト。
// TEST_DATABASE_URL が未設定ならスキップする（CI では DB を用意しないので通常スキップ）。
// ローカルで実行するには:
//
//	TEST_DATABASE_URL='postgres://user:pass@localhost:5432/textro99?sslmode=disable' go test ./internal/db/ -run RoundTrip -v
func TestConfigStore_RoundTrip(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未設定のためスキップ（実DB統合テスト）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	// テスト分離: 既存テーブルを落としてから始める。
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS game_config`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	s := NewConfigStore(pool)

	// Migrate は冪等：2回呼んでもエラーにならず、seed は1回だけ。
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate(1): %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate(2, 冪等のはず): %v", err)
	}

	// seed 後は DefaultParameters が読める。
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load(seed後): %v", err)
	}
	if got.Stack.Limit != game.DefaultParameters().Stack.Limit {
		t.Fatalf("seed 値が既定と違う: got stack.limit=%d", got.Stack.Limit)
	}

	// Save→Load で往復。値を変えて反映されること。
	edited := game.DefaultParameters()
	edited.Stack.Limit = 7
	edited.Difficulty.MaxLevel = 12
	if err := s.Save(ctx, edited); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err = s.Load(ctx)
	if err != nil {
		t.Fatalf("Load(Save後): %v", err)
	}
	if got.Stack.Limit != 7 || got.Difficulty.MaxLevel != 12 {
		t.Fatalf("Save が反映されていない: got stack.limit=%d maxLevel=%d", got.Stack.Limit, got.Difficulty.MaxLevel)
	}

	// 破綻値は Save で弾かれ、DB は前の値のまま。
	bad := game.DefaultParameters()
	bad.Stack.Limit = 0
	if err := s.Save(ctx, bad); err == nil {
		t.Fatal("stack.limit=0 の Save はエラーになるべき")
	}
	got, err = s.Load(ctx)
	if err != nil {
		t.Fatalf("Load(不正Save後): %v", err)
	}
	if got.Stack.Limit != 7 {
		t.Fatalf("不正Saveで値が壊れた: got stack.limit=%d", got.Stack.Limit)
	}

	// 回帰: bot セクション追加前に保存された行（JSON に "bot" キーが無い）でも Load が
	// 失敗しない（本番incident: config-front の GET /api/params が 500 になった原因）。
	legacy := `{"stack":{"limit":7},"difficulty":{"maxLevel":12}}`
	if _, err := pool.Exec(ctx, `UPDATE game_config SET params = $1 WHERE id = 1`, legacy); err != nil {
		t.Fatalf("legacy行のセット: %v", err)
	}
	got, err = s.Load(ctx)
	if err != nil {
		t.Fatalf("bot キー無しの旧データで Load が失敗した（後方互換バグ）: %v", err)
	}
	if got.Bot != game.DefaultParameters().Bot {
		t.Fatalf("bot が既定値で補完されていない: %+v", got.Bot)
	}
	if got.Stack.Limit != 7 {
		t.Fatalf("旧データの既存値が保持されていない: got stack.limit=%d", got.Stack.Limit)
	}
}
