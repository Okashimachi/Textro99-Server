package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"textro99/internal/game"
)

// ConfigStore は Postgres に GameParameters を単一行(JSONB)で保持し、game.ConfigProvider を実装する。
// 併せて config-front からの保存(Save, #50) にも使う。設定は小さな単一オブジェクトなので
// 正規化せず JSONB 1行で持つ（GameParameters をそのまま marshal/unmarshal でき、検証も struct 単位）。
type ConfigStore struct {
	pool     *pgxpool.Pool
	lastGood game.GameParameters
}

// NewConfigStore は ConfigStore を作る。
func NewConfigStore(pool *pgxpool.Pool) *ConfigStore { return &ConfigStore{pool: pool} }

// Migrate は game_config テーブルを作成し、空なら内蔵デフォルトで seed する（冪等）。
// 起動時に1回呼ぶ想定。既存レコードがあれば seed はスキップ（運用中の値を壊さない）。
func (s *ConfigStore) Migrate(ctx context.Context) error {
	const ddl = `CREATE TABLE IF NOT EXISTS game_config (
		id         int PRIMARY KEY DEFAULT 1,
		params     jsonb NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now(),
		CONSTRAINT game_config_singleton CHECK (id = 1)
	)`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("db: migrate: %w", err)
	}
	data, err := json.Marshal(game.DefaultParameters())
	if err != nil {
		return fmt.Errorf("db: seed marshal: %w", err)
	}
	const seed = `INSERT INTO game_config (id, params) VALUES (1, $1)
		ON CONFLICT (id) DO NOTHING`
	if _, err := s.pool.Exec(ctx, seed, data); err != nil {
		return fmt.Errorf("db: seed: %w", err)
	}
	return nil
}

// Load は現在の GameParameters を返す。取得・デコード・検証のいずれかに失敗しても
// 内蔵デフォルト＋理由err を返す（＝第1返り値は常に有効。決定E / 04仕様7章）。
func (s *ConfigStore) Load(ctx context.Context) (game.GameParameters, error) {
	def := game.DefaultParameters()
	var data []byte
	err := s.pool.QueryRow(ctx, `SELECT params FROM game_config WHERE id = 1`).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return def, fmt.Errorf("db: config 行が無い（Migrate未実行?）")
	}
	if err != nil {
		if s.lastGood.Session.TickIntervalMs != 0 {
			return s.lastGood, fmt.Errorf("db: config 取得エラー、前回成功値(キャッシュ)を返します: %w", err)
		}
		return def, fmt.Errorf("db: config 取得: %w", err)
	}
	var gp game.GameParameters
	if err := json.Unmarshal(data, &gp); err != nil {
		if s.lastGood.Session.TickIntervalMs != 0 {
			return s.lastGood, fmt.Errorf("db: config デコードエラー、前回成功値(キャッシュ)を返します: %w", err)
		}
		return def, fmt.Errorf("db: config デコード: %w", err)
	}
	// セクション追加前に保存された行の後方互換（追加分はゼロ値→既定値で補う）。
	gp = gp.BackfillLegacyDefaults()
	if err := gp.Validate(); err != nil {
		if s.lastGood.Session.TickIntervalMs != 0 {
			return s.lastGood, fmt.Errorf("db: config 検証エラー、前回成功値(キャッシュ)を返します: %w", err)
		}
		return def, fmt.Errorf("db: config 検証: %w", err)
	}
	s.lastGood = gp
	return gp, nil
}

// Save は config-front(#50) からの保存。検証してから UPSERT する。検証失敗時は保存せず err。
func (s *ConfigStore) Save(ctx context.Context, gp game.GameParameters) error {
	if err := gp.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(gp)
	if err != nil {
		return fmt.Errorf("db: save marshal: %w", err)
	}
	const up = `INSERT INTO game_config (id, params, updated_at) VALUES (1, $1, now())
		ON CONFLICT (id) DO UPDATE SET params = EXCLUDED.params, updated_at = now()`
	if _, err := s.pool.Exec(ctx, up, data); err != nil {
		return fmt.Errorf("db: save: %w", err)
	}
	return nil
}

// コンパイル時に game.ConfigProvider 充足を保証する。
var _ game.ConfigProvider = (*ConfigStore)(nil)
