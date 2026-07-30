// Package app は【合成寄り】試合の組み立て（session+room の構築、Bot枠の生成、作戦の登録）を
// 再利用可能・テスト可能な形で提供する。cmd/server/main.go はこれと transport/matchmaking を
// 薄く配線するだけにする。
package app

import (
	"context"
	"fmt"
	"math/rand"
	"sync/atomic"
	"time"

	"textro99/internal/bot"
	"textro99/internal/game"
	"textro99/internal/matchmaking"
	"textro99/internal/odai"
	"textro99/internal/room"
	"textro99/internal/store"
	"textro99/internal/targeting"
	"textro99/internal/transport"
)

// Deps は試合を組むための依存一式（合成ルートが用意して注入する）。
type Deps struct {
	Params     game.GameParameters
	Strategies map[int]game.TargetingStrategy
	Words      game.WordSource
	Store      store.ResultStore
	Clock      room.Clock
}

// DefaultStrategies は作戦0〜9の全実装を登録した集合を返す。
func DefaultStrategies() map[int]game.TargetingStrategy {
	return map[int]game.TargetingStrategy{
		0: targeting.SplitAttackStrategy{},
		1: targeting.CounterStrategy{},
		2: targeting.FinisherStrategy{},
		3: targeting.BadgeHunterStrategy{},
		4: targeting.RandomStrategy{},
		5: targeting.RevengeStrategy{},
		6: targeting.TallPoppyStrategy{},
		7: targeting.NeighborStrategy{},
		8: targeting.PileOnStrategy{},
		9: targeting.PacifistHunterStrategy{},
	}
}

// DefaultDeps は DefaultLoader 相当の内蔵デフォルトで Deps を組む（solo/検証用の手軽な既定）。
func DefaultDeps() Deps {
	return Deps{
		Params:     game.DefaultParameters(),
		Strategies: DefaultStrategies(),
		Words:      odai.NewStaticPool(),
		Store:      store.Noop{},
		Clock:      room.RealClock{},
	}
}

var seedSeq atomic.Int64

func newRng() *rand.Rand {
	return rand.New(rand.NewSource(time.Now().UnixNano() + seedSeq.Add(1)))
}

var matchSeq atomic.Int64

func nextMatchID() string { return fmt.Sprintf("m-%d", matchSeq.Add(1)) }

// RunMatch は players で1試合を構築して最後まで駆動する（ctx キャンセルでも抜ける）。
// 呼び出し側で go RunMatch(...) する想定。
func RunMatch(ctx context.Context, d Deps, players []matchmaking.Player) {
	inits := make([]game.PlayerInit, 0, len(players))
	conns := make(map[game.PlayerId]transport.Connection, len(players))
	for _, p := range players {
		name := p.Name
		if name == "" { // 名前未指定はフォールバックで接続IDを使う（識別性を保つ・#79）
			name = string(p.Id)
		}
		inits = append(inits, game.PlayerInit{Id: p.Id, DisplayName: name})
		conns[p.Id] = p.Conn
	}
	sess := game.NewSession(nextMatchID(), d.Params, d.Strategies, d.Words, newRng(), inits)
	pub := transport.NewFullPublisher(d.Params.Session.PublishIntervalMs)
	rm := room.New(sess, conns, d.Params.Session.TickIntervalMs, d.Clock, pub)
	rm.Run(ctx)
}

// NewBotPlayer は Bot 枠を1つ作る。内部で Pipe を張り、client 側を Bot が駆動し、
// server 側を試合に組み込む matchmaking.Player として返す。
func NewBotPlayer(ctx context.Context, id game.PlayerId, cfg bot.Config) matchmaking.Player {
	srv, cli := transport.Pipe()
	b := bot.New(cli, cfg, newRng())
	go b.Run(ctx)
	// Bot は人間と区別できる表示名にする（#79）。id 例 "p-7" → "BOT p-7"。
	return matchmaking.Player{Id: id, Conn: srv, Name: "BOT " + string(id)}
}
