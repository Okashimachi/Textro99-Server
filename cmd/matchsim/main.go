// Command matchsim は「実サーバー(/ws)＋実WebSocket」で1試合を最後まで流し、
// 各プレイヤーが受け取る全 S2C メッセージを時系列でコンソールに出す検証ハーネス。
//
// WebClient と同じ手順（接続→Welcome→MatchmakingJoin(表示名)→MatchStart→
// DakenClearReport で打鍵報告→GameOver）を複数のスクリプト済みクライアントで再現する。
// これで proto 通りに動くか（ゲーム開始/打鍵/コンボ/攻撃予告/被弾/KO/優勝/結果）を
// 主張ではなく実挙動で確認する。
//
//	go run ./cmd/matchsim
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"textro99/internal/app"
	"textro99/internal/game"
	"textro99/internal/matchmaking"
	"textro99/internal/proto"
	"textro99/internal/transport"
)

// ── 共有ロガー（試合開始からの相対時刻つき・行の混線を防ぐ） ──────────────
type logger struct {
	mu       sync.Mutex
	start    time.Time
	seen     map[string]int // 観測した type ごとの件数（末尾チェックリスト用）
	routeErr int            // 本人宛でないお題を受け取った回数（0であるべき＝ルーティング正当性）
}

func newLogger() *logger { return &logger{start: time.Now(), seen: map[string]int{}} }

func (l *logger) routeViolation() {
	l.mu.Lock()
	l.routeErr++
	l.mu.Unlock()
}

func (l *logger) log(who, format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ms := time.Since(l.start).Milliseconds()
	fmt.Printf("[t+%5dms][%-6s] %s\n", ms, who, fmt.Sprintf(format, a...))
}

func (l *logger) mark(typ string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[typ]++
}

func main() {
	scenario := flag.String("scenario", "brawl", "brawl=3人殴り合い / timeout=1人放置(時間切れ/トラップ) / selfko=被弾ゼロ自滅(KO実行者=nil)")
	flag.Parse()
	log := newLogger()

	params := game.DefaultParameters()
	params.Attack.MinComboToAttack = 0
	params.Odai.LookaheadCount = 3
	params.Odai.DamageInsertOffset = 2
	params.Session.TickIntervalMs = 100
	params.Session.PublishIntervalMs = 300
	params.Matching.MinPlayers = 3
	params.Matching.MaxPlayers = 3 // 3人そろったら即開始
	params.Matching.StartCountdownMs = 500

	var specs []clientSpec
	switch *scenario {
	case "timeout":
		// 1人が放置→通常お題が時間切れ→積み残し→トラップ誘発→自滅(KO実行者なし)を観測(#78)。
		params.Stack.Limit = 12
		params.Stack.TrapTriggerInterval = 3
		params.Odai.BaseTimeLimitMs = 1000 // すぐ時間切れ
		params.Odai.MinTimeLimitMs = 800
		params.Attack.WarningGraceMs = 1500
		specs = []clientSpec{
			{name: "リーセ", rateMs: 150, missRate: 0.0},
			{name: "コウハイ", rateMs: 200, missRate: 0.1},
			{name: "ネオチ", idle: true}, // 放置＝一切打鍵しない
		}
	case "selfko":
		// 放置プレイヤーが被弾ゼロ・時間切れ積み残しのみで上限到達→自滅(KO実行者=nil)を観測。
		// grace を極端に長くして、放置者が死ぬまで誰の攻撃も着弾させない。
		params.Stack.Limit = 3
		params.Stack.TrapTriggerInterval = 99 // トラップノイズを避ける
		params.Odai.BaseTimeLimitMs = 500
		params.Odai.MinTimeLimitMs = 500
		params.Odai.PerLevelReductionMs = 0
		params.Attack.WarningGraceMs = 8000 // 自滅前に着弾させない
		specs = []clientSpec{
			{name: "リーセ", rateMs: 150, missRate: 0.0},
			{name: "コウハイ", rateMs: 180, missRate: 0.1},
			{name: "ネオチ", idle: true}, // 被弾ゼロで時間切れだけで落ちる
		}
	default: // brawl
		params.Stack.Limit = 8
		params.Attack.WarningGraceMs = 600
		params.Attack.PowerToDakenRate = 0.35
		params.Difficulty.GlobalIntervalMs = 3000
		specs = []clientSpec{
			{name: "リーセ", rateMs: 120, missRate: 0.00},  // 強：速い・ノーミス → 優勝候補
			{name: "コウハイ", rateMs: 210, missRate: 0.15}, // 中
			{name: "ゲスト", rateMs: 360, missRate: 0.45},  // 弱：遅い・ミス多 → 脱落候補
		}
	}
	fmt.Printf("========== scenario=%s ==========\n", *scenario)

	deps := app.DefaultDeps()
	deps.Params = params

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ── 実サーバー: match モードの /ws を httptest で立てる（main.go の配線を最小再現） ──
	var ids int
	var idMu sync.Mutex
	nextID := func() game.PlayerId {
		idMu.Lock()
		defer idMu.Unlock()
		ids++
		return game.PlayerId(fmt.Sprintf("p-%d", ids))
	}
	mm := matchmaking.New(matchmaking.Config{
		GetParams: func() game.MatchingParams { return params.Matching },
		Start: func(players []matchmaking.Player) {
			log.log("SERVER", "▶ 試合開始 players=%d", len(players))
			go app.RunMatch(ctx, deps, players)
		},
		MinFill: 0, // Bot 補完なし（人間3人で開始）
	})
	go mm.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := transport.Accept(w, r, transport.AcceptOptions{AllowAll: true})
		if err != nil {
			return
		}
		id := nextID()
		// Welcome
		wd, _ := json.Marshal(proto.Welcome{PlayerId: id})
		_ = conn.Send(proto.Envelope{Type: proto.TypeWelcome, Payload: wd})
		// 表示名を1通だけ読む（server の awaitJoinName 相当・#79）
		name := ""
		select {
		case env, ok := <-conn.Receive():
			if ok && env.Type == proto.TypeMatchmakingJoin {
				var m proto.MatchmakingJoin
				if json.Unmarshal(env.Payload, &m) == nil {
					name = matchmaking.SanitizeDisplayName(m.DisplayName)
				}
			}
		case <-time.After(2 * time.Second):
		}
		log.log("SERVER", "参加 id=%s name=%q", id, name)
		mm.Join(matchmaking.Player{Id: id, Conn: conn, Name: name})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	// ── スクリプト済みクライアント ──
	var wg sync.WaitGroup
	for i, sp := range specs {
		wg.Add(1)
		go func(sp clientSpec, seed int64) {
			defer wg.Done()
			runClient(ctx, log, wsURL, sp, rand.New(rand.NewSource(seed)))
		}(sp, int64(1000+i))
	}
	wg.Wait()

	// ── 観測サマリ（proto の各段が出たか） ──
	log.summary()
}

type clientSpec struct {
	name     string
	rateMs   int
	missRate float64
	idle     bool // true=一切打鍵しない（時間切れ経路の検証用・#78）
}

// runClient は1クライアントを実WSで駆動し、受信を全部ログし、保持ダケンを打鍵報告する。
func runClient(ctx context.Context, log *logger, wsURL string, sp clientSpec, rng *rand.Rand) {
	conn, err := transport.Dial(ctx, wsURL)
	if err != nil {
		log.log(sp.name, "接続失敗: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()

	// 接続直後に表示名を送る（WebClient と同じ・#79）
	jd, _ := json.Marshal(proto.MatchmakingJoin{DisplayName: sp.name})
	_ = conn.Send(proto.Envelope{Type: proto.TypeMatchmakingJoin, Payload: jd})

	var mu sync.Mutex
	pending := []proto.DakenId{} // 保持中の実在 dakenId（FIFOでクリア）
	started := false
	self := proto.PlayerId("")

	push := func(d proto.DakenInstance) {
		mu.Lock()
		pending = append(pending, d.DakenId)
		mu.Unlock()
	}
	remove := func(id proto.DakenId) {
		mu.Lock()
		for i, x := range pending {
			if x == id {
				pending = append(pending[:i], pending[i+1:]...)
				break
			}
		}
		mu.Unlock()
	}

	rate := sp.rateMs
	if rate <= 0 {
		rate = 1000 // idle でも ticker 自体は回す（打鍵はしない）
	}
	ticker := time.NewTicker(time.Duration(rate) * time.Millisecond)
	defer ticker.Stop()
	recv := conn.Receive()

	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-recv:
			if !ok {
				return
			}
			log.mark(env.Type)
			if done := handle(log, sp.name, env, &self, &started, push, remove); done {
				return // GameOver
			}
		case <-ticker.C:
			if !started || sp.idle {
				continue // idle は打鍵しない → お題が時間切れになる（#78 経路）
			}
			mu.Lock()
			if len(pending) == 0 {
				mu.Unlock()
				continue
			}
			id := pending[0]
			pending = pending[1:]
			mu.Unlock()
			miss := 0
			if rng.Float64() < sp.missRate {
				miss = 1
			}
			cd, _ := json.Marshal(proto.DakenClearReport{DakenId: id, IsMiss: miss > 0, MissCount: miss, ElapsedMs: int(time.Since(log.start).Milliseconds())})
			_ = conn.Send(proto.Envelope{Type: proto.TypeDakenClearReport, Payload: cd})
		}
	}
}

// handle は1件の S2C を読み、ログし、必要なら保持ダケンを更新する。GameOver で true。
func handle(log *logger, who string, env proto.Envelope, self *proto.PlayerId, started *bool,
	push func(proto.DakenInstance), remove func(proto.DakenId)) bool {
	switch env.Type {
	case proto.TypeWelcome:
		var m proto.Welcome
		_ = json.Unmarshal(env.Payload, &m)
		*self = m.PlayerId
		log.log(who, "Welcome self=%s", m.PlayerId)
	case proto.TypeMatchmakingStatus:
		var m proto.MatchmakingStatus
		_ = json.Unmarshal(env.Payload, &m)
		log.log(who, "MatchmakingStatus 待機=%d/min=%d", m.WaitingCount, m.MinPlayers)
	case proto.TypeMatchStart:
		var m proto.MatchStart
		_ = json.Unmarshal(env.Payload, &m)
		*started = true
		names := make([]string, 0, len(m.Players))
		for _, p := range m.Players {
			names = append(names, p.DisplayName)
		}
		log.log(who, "MatchStart self=%s players=%v 初期お題=%s", m.SelfPlayerId, names, m.InitialDaken.DakenId)
		push(m.InitialDaken)
	case proto.TypeDakenIssued:
		var m proto.DakenIssued
		_ = json.Unmarshal(env.Payload, &m)
		idx := "末尾"
		if m.InsertIndex != nil {
			idx = fmt.Sprintf("index=%d", *m.InsertIndex)
		}
		types := make([]string, 0, len(m.Daken))
		for _, d := range m.Daken {
			types = append(types, string(d.Type))
			if !ownedBy(d.DakenId, *self) { // 本人宛検証（#80/権威ルーティング）
				log.routeViolation()
				log.log(who, "⚠️ROUTING 他人のお題を受信: %s (self=%s)", d.DakenId, *self)
			}
			push(d)
		}
		log.log(who, "DakenIssued x%d %v (%s)", len(m.Daken), types, idx)
	case proto.TypeDakenExpired:
		var m proto.DakenExpired
		_ = json.Unmarshal(env.Payload, &m)
		if !ownedBy(m.DakenId, *self) {
			log.routeViolation()
			log.log(who, "⚠️ROUTING 他人の時間切れを受信: %s (self=%s)", m.DakenId, *self)
		}
		remove(m.DakenId)
		log.log(who, "DakenExpired %s（時間切れ）", m.DakenId)
	case proto.TypeComboUpdated:
		var m proto.ComboUpdated
		_ = json.Unmarshal(env.Payload, &m)
		log.log(who, "ComboUpdated combo=%d (%+d, %s)", m.ComboValue, m.Delta, m.Reason)
	case proto.TypeDifficultyUpdated:
		var m proto.DifficultyUpdated
		_ = json.Unmarshal(env.Payload, &m)
		log.log(who, "DifficultyUpdated 全体=%d 個人=%d 実効=%d", m.GlobalLevel, m.PersonalLevel, m.EffectiveLevel)
	case proto.TypeAttackIncoming:
		var m proto.AttackIncoming
		_ = json.Unmarshal(env.Payload, &m)
		log.log(who, "◀ AttackIncoming 攻撃者=%s 威力=%d grace=%dms", m.AttackerId, m.Power, m.GraceMs)
	case proto.TypeDakenStackUpdated:
		var m proto.DakenStackUpdated
		_ = json.Unmarshal(env.Payload, &m)
		log.log(who, "DakenStackUpdated stack=%d/%d trap=%v", m.Count, m.Limit, m.TrapPending)
	case proto.TypeKoNotified:
		var m proto.KoNotified
		_ = json.Unmarshal(env.Payload, &m)
		att := "自滅"
		if m.AttackerId != nil {
			att = string(*m.AttackerId)
		}
		log.log(who, "☠ KoNotified 脱落=%s KO実行=%s バッジ移動=%d", m.VictimId, att, m.BadgesTransferred)
	case proto.TypePlayerListUpdated:
		var m proto.PlayerListUpdated
		_ = json.Unmarshal(env.Payload, &m)
		parts := make([]string, 0, len(m.Players))
		for _, p := range m.Players {
			parts = append(parts, fmt.Sprintf("%s(rank%d,%s)", p.DisplayName, p.Rank, aliveMark(p.Alive)))
		}
		log.log(who, "PlayerListUpdated 生存=%d %s", m.AliveCount, strings.Join(parts, " "))
	case proto.TypeGameOver:
		var m proto.GameOver
		_ = json.Unmarshal(env.Payload, &m)
		result := fmt.Sprintf("rank=%d ko=%d badges=%d", m.Rank, m.KoCount, m.FinalBadgeCount)
		if m.Rank == 1 {
			result = "🏆 優勝 " + result
		}
		log.log(who, "GameOver %s / stats: clear=%d miss=%d maxCombo=%d elapsed=%dms",
			result, m.TypingStats.TotalDakenCleared, m.TypingStats.TotalMiss, m.TypingStats.MaxCombo, m.TypingStats.ElapsedMs)
		return true
	default:
		log.log(who, "未知メッセージ type=%s", env.Type)
	}
	return false
}

// ownedBy は dakenId が self のお題か（サーバーは "<playerId>-<seq>" で発行する）。
func ownedBy(id proto.DakenId, self proto.PlayerId) bool {
	return self != "" && strings.HasPrefix(string(id), string(self)+"-")
}

func aliveMark(alive bool) string {
	if alive {
		return "生"
	}
	return "死"
}

// summary は proto の各段が観測されたかのチェックリストを出す。
func (l *logger) summary() {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Println("\n========== 観測サマリ（proto 準拠チェック） ==========")
	want := []struct{ typ, label string }{
		{proto.TypeWelcome, "接続確立(Welcome)"},
		{proto.TypeMatchStart, "ゲーム開始(MatchStart)"},
		{proto.TypeDakenIssued, "お題発行/被弾(DakenIssued)"},
		{proto.TypeComboUpdated, "コンボ(ComboUpdated)"},
		{proto.TypeAttackIncoming, "攻撃予告=誰が誰に(AttackIncoming)"},
		{proto.TypeDakenStackUpdated, "スタック増減(DakenStackUpdated)"},
		{proto.TypeKoNotified, "KO/脱落(KoNotified)"},
		{proto.TypePlayerListUpdated, "盤面+順位(PlayerListUpdated)"},
		{proto.TypeGameOver, "結果/優勝(GameOver)"},
	}
	for _, w := range want {
		n := l.seen[w.typ]
		mark := "✅"
		if n == 0 {
			mark = "❌"
		}
		fmt.Printf("  %s %-40s ×%d\n", mark, w.label, n)
	}
	// 参考: 任意イベント
	for _, opt := range []struct{ typ, label string }{
		{proto.TypeDakenExpired, "時間切れ(DakenExpired)"},
		{proto.TypeDifficultyUpdated, "難易度上昇(DifficultyUpdated)"},
		{proto.TypeMatchmakingStatus, "待機(MatchmakingStatus)"},
	} {
		fmt.Printf("  ・ %-40s ×%d\n", opt.label, l.seen[opt.typ])
	}
	routeMark := "✅"
	if l.routeErr > 0 {
		routeMark = "❌"
	}
	fmt.Printf("  %s %-40s 違反=%d（お題は必ず本人宛か）\n", routeMark, "per-player ルーティング正当性", l.routeErr)
}
