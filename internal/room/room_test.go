package room

import (
	"context"
	"encoding/json"
	"math/rand"
	"testing"
	"time"

	"textro99/internal/game"
	"textro99/internal/proto"
	"textro99/internal/transport"
)

// ── テスト用スタブ・時計 ──

type stubWords struct{}

func (stubWords) Next(int, *rand.Rand) game.Word { return game.Word{Text: "ねこ", KeystrokeCount: 4} }
func (stubWords) NextTrap(*rand.Rand) game.Word  { return game.Word{Text: "trap", KeystrokeCount: 10} }

type stubStrategy struct{}

func (stubStrategy) Id() int { return 4 }
func (stubStrategy) SelectTargets(c game.TargetingContext) []game.PlayerId {
	if o := c.Others(); len(o) > 0 {
		return []game.PlayerId{o[0].PlayerId}
	}
	return nil
}

// nopClock は tick を発火しない（コアループの入出力だけを試すため）。
type nopClock struct{}

func (nopClock) NewTicker(time.Duration) Ticker { return nopTicker{} }

type nopTicker struct{}

func (nopTicker) C() <-chan time.Time { return nil } // nil チャネルは select で永久に発火しない
func (nopTicker) Stop()               {}

// manualClock/manualTicker はテストから任意のタイミングで1 tick を発火させる。
type manualTicker struct{ c chan time.Time }

func (m manualTicker) C() <-chan time.Time { return m.c }
func (m manualTicker) Stop()               {}

type manualClock struct{ ticker manualTicker }

func (m manualClock) NewTicker(time.Duration) Ticker { return m.ticker }

func recvEnv(t *testing.T, c transport.Connection) proto.Envelope {
	t.Helper()
	select {
	case env, ok := <-c.Receive():
		if !ok {
			t.Fatal("接続が閉じた")
		}
		return env
	case <-time.After(2 * time.Second):
		t.Fatal("受信タイムアウト")
		return proto.Envelope{}
	}
}

// Room が session を回し、Connection 経由で MatchStart 配信・お題クリア往復ができる。
func TestRoom_CoreLoopThroughConnection(t *testing.T) {
	sess := game.NewSession("m1", game.DefaultParameters(),
		map[int]game.TargetingStrategy{4: stubStrategy{}}, stubWords{},
		rand.New(rand.NewSource(1)),
		[]game.PlayerInit{{Id: "a", DisplayName: "a"}, {Id: "b", DisplayName: "b"}})

	sa, ca := transport.Pipe() // a: server端 / client端
	sb, _ := transport.Pipe()
	conns := map[game.PlayerId]transport.Connection{"a": sa, "b": sb}

	rm := New(sess, conns, 150, nopClock{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rm.Run(ctx)

	// Start で a に MatchStart（初期お題つき）が届く。
	env := recvEnv(t, ca)
	if env.Type != proto.TypeMatchStart {
		t.Fatalf("最初は MatchStart のはず: got %s", env.Type)
	}
	var ms proto.MatchStart
	if err := json.Unmarshal(env.Payload, &ms); err != nil || ms.InitialDaken.DakenId == "" {
		t.Fatalf("MatchStart 復号失敗: %v %+v", err, ms)
	}

	// a がお題クリアを送る → ComboUpdated と 次のお題(DakenIssued) が返る。
	report, _ := json.Marshal(proto.DakenClearReport{DakenId: ms.InitialDaken.DakenId})
	if err := ca.Send(proto.Envelope{Type: proto.TypeDakenClearReport, Payload: report}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	gotCombo, gotIssued := false, false
	for i := 0; i < 2; i++ {
		e := recvEnv(t, ca)
		switch e.Type {
		case proto.TypeComboUpdated:
			var cu proto.ComboUpdated
			_ = json.Unmarshal(e.Payload, &cu)
			if cu.Delta == 14 && cu.Reason == proto.ComboClear {
				gotCombo = true
			}
		case proto.TypeDakenIssued:
			gotIssued = true
		}
	}
	if !gotCombo || !gotIssued {
		t.Fatalf("お題クリア往復不備: combo=%v issued=%v", gotCombo, gotIssued)
	}
}

// 試合が Finished になったら Room は全接続を閉じる。クライアントがリザルト画面を開いたまま
// 放置しても、サーバー側に無駄な接続が残らないことを確認する回帰テスト。
func TestRoom_ClosesConnectionsOnFinish(t *testing.T) {
	// 単独プレイヤー（aliveCount=1）で構成し、1 tick で即 Finished になる最小構成。
	sess := game.NewSession("m1", game.DefaultParameters(),
		map[int]game.TargetingStrategy{4: stubStrategy{}}, stubWords{},
		rand.New(rand.NewSource(1)),
		[]game.PlayerInit{{Id: "a", DisplayName: "a"}})

	sa, ca := transport.Pipe()
	conns := map[game.PlayerId]transport.Connection{"a": sa}

	tickCh := make(chan time.Time, 1)
	rm := New(sess, conns, 150, manualClock{ticker: manualTicker{c: tickCh}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { rm.Run(ctx); close(done) }()

	// Start で MatchStart が届く。
	if env := recvEnv(t, ca); env.Type != proto.TypeMatchStart {
		t.Fatalf("最初は MatchStart のはず: got %s", env.Type)
	}

	// 1 tick 発火 → aliveCount=1 で即 Finished。
	tickCh <- time.Now()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run が Finished で終了しない")
	}

	// GameOver（Finished直前の最終配信）を読み飛ばし、その後 Receive() が閉じることを確認する。
	drained := false
	for i := 0; i < 4 && !drained; i++ {
		select {
		case _, ok := <-ca.Receive():
			if !ok {
				drained = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("接続が閉じない（closeConns が効いていない）")
		}
	}
	if !drained {
		t.Fatal("Run 終了後も接続が閉じていない")
	}
}
