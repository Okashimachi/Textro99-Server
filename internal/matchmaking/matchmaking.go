// Package matchmaking は【スパイン】試合前の待機・編成。人数下限＋カウントダウンで開始し、
// 定員不足を Bot で補完する（02_マッチング仕様）。待機者へ MatchmakingStatus を配信する。
//
// 依存: game(MatchingParams/PlayerId) / transport(Connection) / proto(契約)。
// 「試合をどう構築するか（session+room）」と「Bot枠をどう作るか」は Config のコールバックで
// 注入する（合成ルート #38 が渡す）。matchmaking 自身は「いつ・誰で始めるか」だけを持つ。
package matchmaking

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode"

	"textro99/internal/game"
	"textro99/internal/proto"
	"textro99/internal/transport"
)

// Player は待機中／開始対象の1名。Conn はサーバー側の接続ハンドル。
// Name は盤面表示名（サニタイズ済み・空ならフォールバックは試合構築側が決める）。
type Player struct {
	Id   game.PlayerId
	Conn transport.Connection
	Name string
}

// MaxDisplayNameLen は表示名の最大文字数（ルーン単位）。
const MaxDisplayNameLen = 24

// SanitizeDisplayName は受信した表示名を安全化する（#79）。
// 前後空白を除去し、制御文字（改行・タブ等）を落とし、MaxDisplayNameLen ルーンで切り詰める。
// 結果が空なら空文字を返す（フォールバック名の割り当ては呼び出し側の責務）。
func SanitizeDisplayName(raw string) string {
	cleaned := make([]rune, 0, len(raw))
	for _, r := range raw {
		if unicode.IsControl(r) {
			continue
		}
		cleaned = append(cleaned, r)
	}
	name := strings.TrimSpace(string(cleaned))
	rs := []rune(name)
	if len(rs) > MaxDisplayNameLen {
		name = strings.TrimSpace(string(rs[:MaxDisplayNameLen]))
	}
	return name
}

// Config は matchmaking の依存と数値。
type Config struct {
	// GetParams は現在のマッチング用パラメータを返す（動的リロード対応）。
	GetParams func() game.MatchingParams
	// After は指定時間後に発火するチャネルを返す（本番=time.After、テスト=手動）。
	After func(time.Duration) <-chan time.Time
	// Start は集まったプレイヤー（人間＋Bot補完）で試合を開始する（session+room 構築）。
	Start func(players []Player)
	// NewBot は Bot 枠を1つ作って返す（nil なら補完しない）。合成ルートが Pipe＋bot起動して返す。
	NewBot func() Player
}

type eventKind int

const (
	evJoin eventKind = iota
	evLeave
)

type event struct {
	kind eventKind
	p    Player
	id   game.PlayerId
}

// Matchmaker は単一の待機プールを回す。
type Matchmaker struct {
	cfg    Config
	events chan event // join/leave を単一FIFOで処理し、順序の曖昧さを避ける
}

// New は Matchmaker を作る。After 未指定なら time.After を使う。
func New(cfg Config) *Matchmaker {
	if cfg.After == nil {
		cfg.After = time.After
	}
	return &Matchmaker{cfg: cfg, events: make(chan event, 256)}
}

// Join は待機プールへ参加させる。
func (m *Matchmaker) Join(p Player) { m.events <- event{kind: evJoin, p: p, id: p.Id} }

// Leave は待機プールから離脱させる。
func (m *Matchmaker) Leave(id game.PlayerId) { m.events <- event{kind: evLeave, id: id} }

// Run は待機プールを回す。ctx キャンセルで終了。
//
// 開始条件（いずれか）: 人数が maxPlayers 到達（即開始）／ minPlayers 到達後 startCountdownMs 経過。
// カウントダウン中に minPlayers を割り込むとリセット。
func (m *Matchmaker) Run(ctx context.Context) {
	var pool []Player
	var countdown <-chan time.Time // nil のときカウントダウンなし
	var countdownStart time.Time   // カウントダウン開始時刻（残り時間算出用）
	var countdownMin int
	var countdownDurationMs int

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	update := func(forceBroadcast bool) {
		params := m.cfg.GetParams()
		min := params.MinPlayers
		max := params.MaxPlayers
		changed := false

		if max > 0 && len(pool) >= max {
			m.startMatch(pool, params)
			pool, countdown = nil, nil
			return
		}

		if countdown == nil && len(pool) >= min {
			countdownMin = min
			countdownDurationMs = params.StartCountdownMs
			countdownStart = time.Now()
			countdown = m.cfg.After(time.Duration(params.StartCountdownMs) * time.Millisecond)
			changed = true
		} else if countdown != nil && len(pool) < countdownMin {
			countdown = nil
			changed = true
		}

		if countdown != nil {
			params.MinPlayers = countdownMin
			params.StartCountdownMs = countdownDurationMs
		}

		if forceBroadcast || changed {
			m.broadcast(pool, countdown != nil, countdownStart, params)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return

		case ev := <-m.events:
			switch ev.kind {
			case evJoin:
				pool = append(pool, ev.p)
			case evLeave:
				pool = removePlayer(pool, ev.id)
			}
			update(true)
			
		case <-ticker.C:
			if len(pool) > 0 {
				update(true)
			}

		case <-countdown:
			m.startMatch(pool, m.cfg.GetParams())
			pool, countdown = nil, nil
		}
	}
}

// startMatch は Bot 補完してから Start を呼ぶ。
func (m *Matchmaker) startMatch(pool []Player, params game.MatchingParams) {
	players := append([]Player(nil), pool...)
	for len(players) < params.MinFill && m.cfg.NewBot != nil {
		players = append(players, m.cfg.NewBot())
	}
	if m.cfg.Start != nil {
		m.cfg.Start(players)
	}
}

// broadcast は待機者へ現在の MatchmakingStatus を配信する。
func (m *Matchmaker) broadcast(pool []Player, counting bool, countdownStart time.Time, params game.MatchingParams) {
	players := make([]proto.WaitingPlayer, len(pool))
	for i, p := range pool {
		name := p.Name
		if name == "" {
			name = string(p.Id)
		}
		players[i] = proto.WaitingPlayer{DisplayName: name}
	}
	status := proto.MatchmakingStatus{
		WaitingCount: len(pool),
		MinPlayers:   params.MinPlayers,
		Players:      players,
	}
	if counting {
		remaining := params.StartCountdownMs - int(time.Since(countdownStart).Milliseconds())
		if remaining < 0 {
			remaining = 0
		}
		status.CountdownMs = &remaining
	}
	data, err := json.Marshal(status)
	if err != nil {
		return
	}
	env := proto.Envelope{Type: proto.TypeMatchmakingStatus, Payload: data}
	for _, p := range pool {
		_ = p.Conn.Send(env)
	}
}

func removePlayer(pool []Player, id game.PlayerId) []Player {
	for i, p := range pool {
		if p.Id == id {
			return append(pool[:i], pool[i+1:]...)
		}
	}
	return pool
}
