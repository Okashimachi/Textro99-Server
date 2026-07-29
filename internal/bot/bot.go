// Package bot は【#35】サーバー内で自動入力する仮想クライアント（CPU）。
//
// 人間と同じ transport.Connection に乗り、session からは区別されない。人数補完・デモ・
// 切断補完・99人負荷検証(#50)の"燃料"。強さは Config（クリア間隔・ミス率・攻撃頻度）で表現する。
//
// ⚠ チート検証と衝突しないよう、Bot は**サーバー発行の実在 dakenId** に対してのみクリア報告する
// （MatchStart / DakenIssued で受け取った dakenId を保持し、それを打ち切ったと報告する）。
package bot

import (
	"context"
	"encoding/json"
	"math/rand"
	"time"

	"textro99/internal/proto"
	"textro99/internal/transport"
)

// Config は Bot の強さ・振る舞い。数値は呼び出し側が与える（ロジックに直書きしない）。
type Config struct {
	ClearIntervalMs int     // 1ダケンを打ち切るのにかける平均ms
	MissRate        float64 // ミス確率(0..1)
}

// DefaultConfig は無難な既定値。
func DefaultConfig() Config { return Config{ClearIntervalMs: 500, MissRate: 0.05} }

// Bot は1接続を自動操作する。
type Bot struct {
	conn    transport.Connection
	cfg     Config
	rng     *rand.Rand
	pending []proto.DakenId // 保持中（未クリア）の実在 dakenId
}

// New は Bot を作る。conn は自分の（クライアント側）接続。
func New(conn transport.Connection, cfg Config, rng *rand.Rand) *Bot {
	return &Bot{conn: conn, cfg: cfg, rng: rng}
}

// Run は Bot を駆動する。受信でお題を貯め、一定間隔で1つずつクリア報告し、たまに攻撃する。
// GameOver 受信・接続切断・ctx キャンセルで終了する。
//
// 終了時に接続を Close する。これにより、脱落して読むのをやめた Bot へサーバーが
// ブロードキャストし続けても、Send が即エラー（ブロックしない）になり、room ループが
// 死んだ Bot で詰まらない（99人スケールで重要）。
func (b *Bot) Run(ctx context.Context) {
	defer func() { _ = b.conn.Close() }()
	iv := time.Duration(b.cfg.ClearIntervalMs) * time.Millisecond
	if iv <= 0 {
		iv = 500 * time.Millisecond
	}
	ticker := time.NewTicker(iv)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-b.conn.Receive():
			if !ok {
				return
			}
			if b.onMessage(env) {
				return // GameOver
			}
		case <-ticker.C:
			b.act()
		}
	}
}

// onMessage はサーバーメッセージから保持お題を更新する。GameOver なら true。
func (b *Bot) onMessage(env proto.Envelope) bool {
	switch env.Type {
	case proto.TypeMatchStart:
		var m proto.MatchStart
		if json.Unmarshal(env.Payload, &m) == nil {
			b.pending = append(b.pending, m.InitialDaken.DakenId)
		}
	case proto.TypeDakenIssued:
		var m proto.DakenIssued
		if json.Unmarshal(env.Payload, &m) == nil {
			for _, d := range m.Daken {
				b.pending = append(b.pending, d.DakenId)
			}
		}
	case proto.TypeDakenExpired:
		var m proto.DakenExpired
		if json.Unmarshal(env.Payload, &m) == nil {
			b.removePending(m.DakenId)
		}
	case proto.TypeGameOver:
		return true
	}
	return false
}

// act は保持お題を1つクリア報告する。攻撃はサーバー側でクリア起点に自動発火する（#77）。
func (b *Bot) act() {
	if len(b.pending) == 0 {
		return
	}
	id := b.pending[0]
	b.pending = b.pending[1:]

	miss := 0
	if b.rng.Float64() < b.cfg.MissRate {
		miss = 1
	}
	b.send(proto.TypeDakenClearReport, proto.DakenClearReport{DakenId: id, IsMiss: miss > 0, MissCount: miss})
}

func (b *Bot) send(typ string, msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	_ = b.conn.Send(proto.Envelope{Type: typ, Payload: data})
}

func (b *Bot) removePending(id proto.DakenId) {
	for i, x := range b.pending {
		if x == id {
			b.pending = append(b.pending[:i], b.pending[i+1:]...)
			return
		}
	}
}
