// Package room は【スパイン】試合の駆動役。1試合=1goroutineで、Connection からの入力(inbox)と
// tick を単一ループで直列処理し、game.Session（純粋コア）を回して Outbound を接続へ配信する。
//
// 依存: game(コア) と transport(通信) と proto(契約)。game/transport はここを import しない。
// tick 周期は GameParameters（session.tickIntervalMs）由来。時計は Clock で注入（本番=実時間/テスト=手動）。
package room

import (
	"context"
	"encoding/json"
	"time"

	"textro99/internal/game"
	"textro99/internal/proto"
	"textro99/internal/transport"
)

// Ticker / Clock は tick の時計を抽象化する（本番=実時間、テスト/シミュレーション=手動）。
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type Clock interface {
	NewTicker(d time.Duration) Ticker
	After(d time.Duration) <-chan time.Time
}

// RealClock は time.Ticker を使う本番用 Clock。
type RealClock struct{}

func (RealClock) NewTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

type inbound struct {
	pid game.PlayerId
	env proto.Envelope
}

// Room は1試合を駆動する。session/conns/tickMs/clock/publisher を注入し、Run(ctx) で回す。
type Room struct {
	session   *game.Session
	conns     map[game.PlayerId]transport.Connection
	tickMs    int
	clock     Clock
	publisher transport.StatePublisher // nil 可（盤面配信なし）
	inbox     chan inbound
	done      chan struct{}
	elapsedMs int64 // 配信間引き用の単調時計（tick を積算）
}

// New は Room を作る。conns は playerId→接続。tickMs は tick 周期(ms)。
// publisher は盤面の定期配信（#32）。nil なら配信しない。
func New(session *game.Session, conns map[game.PlayerId]transport.Connection, tickMs int, clock Clock, publisher transport.StatePublisher) *Room {
	if tickMs <= 0 {
		tickMs = 150
	}
	return &Room{
		session: session, conns: conns, tickMs: tickMs, clock: clock, publisher: publisher,
		inbox: make(chan inbound, 256), done: make(chan struct{}),
	}
}

// Run は試合を最後まで（Finished）駆動する。ctx キャンセルでも抜ける。
func (r *Room) Run(ctx context.Context) {
	defer close(r.done)
	defer r.closeConns() // 試合終了(Finished/ctxキャンセル問わず)後、リザルト画面に居座られても
	// サーバー側に接続を残さない（Room はここで役目を終える。以後このソケットへは何も送らない）。
	for pid, c := range r.conns {
		go r.readConn(pid, c)
	}

	r.dispatch(r.session.Start())
	if r.session.State() == game.Finished {
		return
	}

	ticker := r.clock.NewTicker(time.Duration(r.tickMs) * time.Millisecond)
	defer ticker.Stop()
	for r.session.State() != game.Finished {
		select {
		case <-ctx.Done():
			return
		case in := <-r.inbox:
			r.dispatch(r.handle(in))
		case <-ticker.C():
			r.elapsedMs += int64(r.tickMs)
			r.dispatch(r.session.Tick(r.tickMs))
			r.publish()
		}
	}

	// 試合終了後、フロントエンドのリザルト画面（カウントダウン15秒）が完了し
	// 自発的に切断するまでの猶予期間（20秒）を設ける。
	// これにより、即時切断によるフロントエンドの意図しない再接続（自動マッチング）を防ぐ。
	select {
	case <-ctx.Done():
	case <-r.clock.After(20 * time.Second):
	}
}

// publish は盤面スナップを（間引き判断は publisher 任せで）配信する。
func (r *Room) publish() {
	if r.publisher == nil {
		return
	}
	players, aliveCount := r.session.Snapshot()
	r.publisher.Publish(r.elapsedMs, players, aliveCount, r.conns)
}

// closeConns は Room 終了時に全接続を閉じる。試合終了(GameOver配信済み)後はこのソケットへ
// 二度とメッセージを送らないため、クライアントがリザルト画面を開いたまま放置しても
// サーバー側に無駄な接続を残さない（#84 と同種の"終わった状態が居座る"クラスの不具合の対策）。
// Close のエラーは診断上有用でないため無視する（既に切れている等）。
func (r *Room) closeConns() {
	for _, c := range r.conns {
		_ = c.Close()
	}
}

// readConn は1接続を読み続け、受信を inbox へ流す。接続 close / Room 終了で抜ける。
func (r *Room) readConn(pid game.PlayerId, c transport.Connection) {
	for {
		select {
		case env, ok := <-c.Receive():
			if !ok {
				return
			}
			select {
			case r.inbox <- inbound{pid: pid, env: env}:
			case <-r.done:
				return
			}
		case <-r.done:
			return
		}
	}
}

// handle は受信メッセージを型で振り分け、対応する session の step 関数を呼ぶ。
func (r *Room) handle(in inbound) []game.Outbound {
	switch in.env.Type {
	case proto.TypeDakenClearReport:
		var m proto.DakenClearReport
		if json.Unmarshal(in.env.Payload, &m) == nil {
			return r.session.ApplyDakenClear(in.pid, m)
		}
	case proto.TypeStrategySelect:
		var m proto.StrategySelect
		if json.Unmarshal(in.env.Payload, &m) == nil {
			return r.session.ApplyStrategy(in.pid, m)
		}
	}
	return nil
}

// dispatch は Outbound を Envelope に包んで宛先の接続へ送る。
func (r *Room) dispatch(out []game.Outbound) {
	for _, o := range out {
		env, ok := envelopeOf(o.Msg)
		if !ok {
			continue
		}
		if o.To.Broadcast {
			for _, c := range r.conns {
				_ = c.Send(env)
			}
			continue
		}
		if c := r.conns[o.To.PlayerId]; c != nil {
			_ = c.Send(env)
		}
	}
}

// envelopeOf は S2C の proto メッセージ値を、種別タグつき Envelope へ変換する。
func envelopeOf(msg any) (proto.Envelope, bool) {
	var typ string
	switch msg.(type) {
	case proto.Welcome:
		typ = proto.TypeWelcome
	case proto.MatchStart:
		typ = proto.TypeMatchStart
	case proto.DakenIssued:
		typ = proto.TypeDakenIssued
	case proto.DakenExpired:
		typ = proto.TypeDakenExpired
	case proto.ComboUpdated:
		typ = proto.TypeComboUpdated
	case proto.DifficultyUpdated:
		typ = proto.TypeDifficultyUpdated
	case proto.AttackIncoming:
		typ = proto.TypeAttackIncoming
	case proto.DakenStackUpdated:
		typ = proto.TypeDakenStackUpdated
	case proto.KoNotified:
		typ = proto.TypeKoNotified
	case proto.PlayerListUpdated:
		typ = proto.TypePlayerListUpdated
	case proto.PlayerListDelta:
		typ = proto.TypePlayerListDelta
	case proto.GameOver:
		typ = proto.TypeGameOver
	case proto.MatchmakingStatus:
		typ = proto.TypeMatchmakingStatus
	case proto.MatchEnd:
		typ = proto.TypeMatchEnd
	default:
		return proto.Envelope{}, false
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return proto.Envelope{}, false
	}
	return proto.Envelope{Type: typ, Payload: data}, true
}
