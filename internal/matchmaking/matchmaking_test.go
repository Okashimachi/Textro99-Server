package matchmaking

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"textro99/internal/game"
	"textro99/internal/proto"
	"textro99/internal/transport"
)

func recvStatus(t *testing.T, c transport.Connection) proto.MatchmakingStatus {
	t.Helper()
	select {
	case env, ok := <-c.Receive():
		if !ok {
			t.Fatal("接続が閉じた")
		}
		var s proto.MatchmakingStatus
		_ = json.Unmarshal(env.Payload, &s)
		return s
	case <-time.After(2 * time.Second):
		t.Fatal("MatchmakingStatus 受信タイムアウト")
		return proto.MatchmakingStatus{}
	}
}

// min到達でカウントダウン→完了で開始。定員不足は Bot で補完される。
func TestMatchmaker_CountdownAndBotFill(t *testing.T) {
	afterCh := make(chan time.Time, 1)
	started := make(chan []Player, 1)
	botN := 0
	m := New(Config{
		Params: game.MatchingParams{MinPlayers: 2, MaxPlayers: 99, StartCountdownMs: 15000},
		After:  func(time.Duration) <-chan time.Time { return afterCh },
		Start:  func(p []Player) { started <- p },
		NewBot: func() Player {
			botN++
			s, _ := transport.Pipe()
			return Player{Id: game.PlayerId(fmt.Sprintf("bot%d", botN)), Conn: s}
		},
		MinFill: 4, // 人間2 + Bot2 = 4
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	sa, ca := transport.Pipe()
	sb, cb := transport.Pipe()

	m.Join(Player{Id: "a", Conn: sa})
	if st := recvStatus(t, ca); st.WaitingCount != 1 || st.CountdownMs != nil {
		t.Fatalf("1人目はカウントダウンなし: %+v", st)
	}

	m.Join(Player{Id: "b", Conn: sb})
	if st := recvStatus(t, cb); st.WaitingCount != 2 || st.CountdownMs == nil {
		t.Fatalf("min到達でカウントダウン開始: %+v", st)
	}

	afterCh <- time.Now() // カウントダウン完了
	select {
	case p := <-started:
		if len(p) != 4 {
			t.Fatalf("人間2+Bot2=4で開始すべき: got %d", len(p))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start が呼ばれない")
	}
}

// maxPlayers 到達で即開始。
func TestMatchmaker_MaxPlayersImmediateStart(t *testing.T) {
	started := make(chan []Player, 1)
	m := New(Config{
		Params: game.MatchingParams{MinPlayers: 2, MaxPlayers: 2, StartCountdownMs: 15000},
		Start:  func(p []Player) { started <- p },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	sa, _ := transport.Pipe()
	sb, _ := transport.Pipe()
	m.Join(Player{Id: "a", Conn: sa})
	m.Join(Player{Id: "b", Conn: sb}) // max=2 到達

	select {
	case p := <-started:
		if len(p) != 2 {
			t.Fatalf("max到達で2人開始すべき: got %d", len(p))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("max到達で即開始すべき")
	}
}

// カウントダウン中に min を割り込むとリセットされ、開始しない。
func TestMatchmaker_LeaveBelowMinCancelsCountdown(t *testing.T) {
	afterCh := make(chan time.Time, 1)
	started := make(chan []Player, 1)
	m := New(Config{
		Params: game.MatchingParams{MinPlayers: 2, MaxPlayers: 99, StartCountdownMs: 15000},
		After:  func(time.Duration) <-chan time.Time { return afterCh },
		Start:  func(p []Player) { started <- p },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	sa, ca := transport.Pipe()
	sb, _ := transport.Pipe()
	m.Join(Player{Id: "a", Conn: sa})
	m.Join(Player{Id: "b", Conn: sb}) // カウントダウン開始
	m.Leave("b")                      // min割り込み→リセット

	// a は join-a / join-b / leave-b の3件の status を受け取る。3件目まで読めば
	// Leave が処理済み＝countdown が nil にリセット済みと確定できる（決定的に同期）。
	recvStatus(t, ca) // after join-a (waiting1)
	recvStatus(t, ca) // after join-b (waiting2, countdown)
	if st := recvStatus(t, ca); st.WaitingCount != 1 || st.CountdownMs != nil {
		t.Fatalf("leave後はwaiting1・カウントダウン解除: %+v", st)
	}

	afterCh <- time.Now() // ここで撃っても Run側 countdown は nil なので無視される
	select {
	case <-started:
		t.Fatal("カウントダウンはリセットされ開始しないはず")
	case <-time.After(300 * time.Millisecond):
		// 開始しない＝正しい
	}
}

// #79 表示名サニタイズ: trim / 制御文字除去 / ルーン単位切り詰め / 空。
func TestSanitizeDisplayName(t *testing.T) {
	long := ""
	for i := 0; i < 40; i++ {
		long += "あ"
	}
	cases := []struct{ in, want string }{
		{"りーせ", "りーせ"},
		{"  spaced  ", "spaced"},
		{"a\tb\nc", "abc"},       // 制御文字は除去
		{"\x00\x07evil", "evil"}, // NUL/ベル除去
		{"", ""},                 // 空
		{"   ", ""},              // 空白のみ→空
		{long, string([]rune(long)[:MaxDisplayNameLen])}, // 24ルーンへ切り詰め
	}
	for _, c := range cases {
		if got := SanitizeDisplayName(c.in); got != c.want {
			t.Fatalf("SanitizeDisplayName(%q)=%q, want %q", c.in, got, c.want)
		}
	}
	if n := len([]rune(SanitizeDisplayName(long))); n != MaxDisplayNameLen {
		t.Fatalf("切り詰め後のルーン数=%d, want %d", n, MaxDisplayNameLen)
	}
}
