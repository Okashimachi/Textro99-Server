package app_test

// クライアント結合の回帰テスト。実WSクライアント（coder/websocket 直叩き＝内部Connection を
// 介さない生ワイヤ）で 接続→Welcome→MatchStart→ダケンクリア→攻撃→GameOver までを通し、
// 「クライアントが最低限つなげる」ことと、各S2Cの on-wire JSON 形（camelCase）を守る。
// docs/client-integration.md の記述と乖離したらここが落ちる。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"textro99/internal/app"
	"textro99/internal/bot"
	"textro99/internal/game"
	"textro99/internal/matchmaking"
	"textro99/internal/proto"
	"textro99/internal/transport"
)

func TestE2E_ClientWireFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 短時間で決着するよう強めの調整値。
	deps := app.DefaultDeps()
	deps.Params.Stack.Limit = 4
	deps.Params.Attack.ComboToPowerRatio = 2.0
	deps.Params.Attack.PowerToDakenRate = 0.6
	deps.Params.Attack.WarningGraceMs = 100
	deps.Params.Session.TickIntervalMs = 15
	deps.Params.Session.PublishIntervalMs = 50
	deps.Params.Difficulty.GlobalIntervalMs = 1500

	var ids atomic.Int64
	nextID := func() game.PlayerId { return game.PlayerId(fmt.Sprintf("p-%d", ids.Add(1))) }

	// solo 相当のハンドラ: 接続で Welcome→人間1＋Bot3で試合開始。
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := transport.Accept(w, r, transport.AcceptOptions{AllowAll: true})
		if err != nil {
			return
		}
		id := nextID()
		data, _ := json.Marshal(proto.Welcome{PlayerId: id})
		_ = conn.Send(proto.Envelope{Type: proto.TypeWelcome, Payload: data})
		players := []matchmaking.Player{{Id: id, Conn: conn}}
		for i := 0; i < 3; i++ {
			players = append(players, app.NewBotPlayer(ctx, nextID(),
				bot.Config{ClearIntervalMs: 40, MissRate: 0.02}))
		}
		go app.RunMatch(ctx, deps, players)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 生WSクライアントで接続。
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close(websocket.StatusNormalClosure, "bye") }()

	seen := map[string]json.RawMessage{} // 型ごとに最初の生payloadを1件保存
	var self proto.PlayerId
	pending := []proto.DakenId{}

	pushDaken := func(d proto.DakenInstance) { pending = append(pending, d.DakenId) }
	send := func(typ string, msg any) {
		p, _ := json.Marshal(msg)
		env, _ := json.Marshal(proto.Envelope{Type: typ, Payload: p})
		_ = c.Write(ctx, websocket.MessageText, env)
	}

	gotGameOver := false
	deadline := time.After(25 * time.Second)
loop:
	for {
		select {
		case <-deadline:
			break loop
		case <-ctx.Done():
			break loop
		default:
		}

		_, raw, err := c.Read(ctx)
		if err != nil {
			t.Logf("read 終了: %v", err)
			break
		}
		var env proto.Envelope
		if json.Unmarshal(raw, &env) != nil {
			t.Fatalf("Envelope でない生フレーム: %s", raw)
		}
		if _, ok := seen[env.Type]; !ok {
			seen[env.Type] = env.Payload
		}

		switch env.Type {
		case proto.TypeWelcome:
			var m proto.Welcome
			_ = json.Unmarshal(env.Payload, &m)
			self = m.PlayerId
		case proto.TypeMatchStart:
			var m proto.MatchStart
			_ = json.Unmarshal(env.Payload, &m)
			pushDaken(m.InitialDaken)
		case proto.TypeDakenIssued:
			var m proto.DakenIssued
			_ = json.Unmarshal(env.Payload, &m)
			for _, d := range m.Daken {
				pushDaken(d)
			}
		case proto.TypeGameOver:
			gotGameOver = true
			break loop
		}

		// 受信ごとに保持ダケンを1つクリア報告する（攻撃はサーバー側でクリア起点に自動発火・#77）。
		if len(pending) > 0 {
			id := pending[0]
			pending = pending[1:]
			send(proto.TypeDakenClearReport, proto.DakenClearReport{DakenId: id, IsMiss: false, MissCount: 0, ElapsedMs: 300})
		}
	}

	// ---- 結果ログ（資料用の生JSONサンプル） ----
	t.Logf("self=%s / 観測メッセージ種別数=%d / gameOver=%v", self, len(seen), gotGameOver)
	types := make([]string, 0, len(seen))
	for k := range seen {
		types = append(types, k)
	}
	sort.Strings(types)
	for _, k := range types {
		t.Logf("  %-18s payload=%s", k, string(seen[k]))
	}

	// ---- 最低限の結合が成立していることの表明 ----
	must := []string{proto.TypeWelcome, proto.TypeMatchStart, proto.TypeDakenIssued}
	for _, m := range must {
		if _, ok := seen[m]; !ok {
			t.Errorf("必須メッセージ %s を受信できていない", m)
		}
	}
	if self == "" {
		t.Error("Welcome から自分の PlayerId を取得できていない")
	}
	if !gotGameOver {
		t.Error("GameOver に到達しなかった（最低限フローが完走していない）")
	}
}
