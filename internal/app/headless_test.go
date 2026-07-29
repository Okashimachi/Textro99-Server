package app_test

import (
	"math/rand"
	"testing"

	"textro99/internal/app"
	"textro99/internal/game"
	"textro99/internal/odai"
	"textro99/internal/proto"
)

// #50a: Bot 数体の試合を決定的に最後まで走らせ、全イベントをログに出して
// 「開始→攻防→相殺→KO→順位確定」が仕様どおりかを検証する。
//
// room/transport/bot の実ゴルーチンは使わず、game.Session を直接 tick 駆動して観測性と
// 決定性を確保する（戦闘の"仕様"は session/combat 側にある。組み立ては app_test.go で別途実証済み）。
// `go test -v -run HeadlessMatch ./internal/app/` でログが読める。
func TestHeadlessMatch_PlaysToWinnerWithRanks(t *testing.T) {
	// 収束を早めるための検証用パラメータ（短時間で決着＋相殺も見える程度）。
	params := game.DefaultParameters()
	params.Stack.Limit = 8
	params.Attack.PowerToDakenRate = 0.2
	params.Attack.WarningGraceMs = 1000

	ids := []game.PlayerId{"p1", "p2", "p3", "p4"}
	inits := make([]game.PlayerInit, len(ids))
	for i, id := range ids {
		inits[i] = game.PlayerInit{Id: id, DisplayName: string(id)}
	}
	s := game.NewSession("verify", params, app.DefaultStrategies(), odai.NewStaticPool(), rand.New(rand.NewSource(7)), inits)

	pending := map[game.PlayerId][]proto.DakenId{}
	gameOverRank := map[int]game.PlayerId{}
	kos := 0

	consume := func(out []game.Outbound) {
		for _, o := range out {
			switch m := o.Msg.(type) {
			case proto.MatchStart:
				pending[o.To.PlayerId] = append(pending[o.To.PlayerId], m.InitialDaken.DakenId)
				t.Logf("[%s] MatchStart players=%d initialDaken=%s", o.To.PlayerId, len(m.Players), m.InitialDaken.DakenId)
			case proto.DakenIssued:
				for _, dk := range m.Daken {
					pending[o.To.PlayerId] = append(pending[o.To.PlayerId], dk.DakenId)
				}
				t.Logf("[%s] DakenIssued x%d (type=%s)", o.To.PlayerId, len(m.Daken), m.Daken[0].Type)
			case proto.ComboUpdated:
				t.Logf("[%s] ComboUpdated combo=%d delta=%+d (%s)", o.To.PlayerId, m.ComboValue, m.Delta, m.Reason)
			case proto.AttackIncoming:
				t.Logf("   ↳ [%s] AttackIncoming from=%s power=%d grace=%dms", o.To.PlayerId, m.AttackerId, m.Power, m.GraceMs)
			case proto.DakenStackUpdated:
				t.Logf("[%s] DakenStackUpdated stack=%d/%d trapPending=%v", o.To.PlayerId, m.Count, m.Limit, m.TrapPending)
			case proto.DifficultyUpdated:
				t.Logf("[%s] DifficultyUpdated global=%d personal=%d effective=%d", o.To.PlayerId, m.GlobalLevel, m.PersonalLevel, m.EffectiveLevel)
			case proto.DakenExpired:
				t.Logf("[%s] DakenExpired %s (積み残し)", o.To.PlayerId, m.DakenId)
			case proto.KoNotified:
				kos++
				att := "なし(自滅)"
				if m.AttackerId != nil {
					att = string(*m.AttackerId)
				}
				t.Logf("*** KO  victim=%s  attacker=%s  badges+%d ***", m.VictimId, att, m.BadgesTransferred)
			case proto.GameOver:
				gameOverRank[m.Rank] = o.To.PlayerId
				t.Logf("=== GameOver [%s] rank=%d ko=%d maxCombo=%d ===", o.To.PlayerId, m.Rank, m.KoCount, m.TypingStats.MaxCombo)
			}
		}
	}

	t.Log("========== 試合開始 ==========")
	consume(s.Start())

	const tickMs = 1500 // grace(1000) を超えるので、そのラウンドの予告は Tick で着弾/expire する
	rounds := 0
	for ; rounds < 500 && s.State() != game.Finished; rounds++ {
		for _, id := range ids {
			q := pending[id]
			if len(q) == 0 {
				continue
			}
			did := q[0]
			pending[id] = q[1:]
			// クリア報告のみ。攻撃はサーバー側でクリア起点に自動発火する（#77）。
			consume(s.ApplyDakenClear(id, proto.DakenClearReport{DakenId: did}))
		}
		consume(s.Tick(tickMs))
	}

	t.Logf("========== 試合終了: %d ラウンド / KO=%d ==========", rounds, kos)

	// 検証: 最後まで走って順位が確定していること。
	if s.State() != game.Finished {
		t.Fatalf("試合が収束しなかった（%d ラウンドで未終了）", rounds)
	}
	for r := 1; r <= len(ids); r++ {
		if gameOverRank[r] == "" {
			t.Fatalf("順位 %d の確定(GameOver)が無い: %+v", r, gameOverRank)
		}
	}
	if kos == 0 {
		t.Fatal("KO が1件も発生していない（攻防が成立していない疑い）")
	}
	t.Logf("優勝(rank1)=%s / 順位: 1:%s 2:%s 3:%s 4:%s",
		gameOverRank[1], gameOverRank[1], gameOverRank[2], gameOverRank[3], gameOverRank[4])
}
