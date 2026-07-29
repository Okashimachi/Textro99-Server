package proto

import (
	"encoding/json"
	"testing"
)

// TestWireGolden は、契約メッセージが canonical proto の JSON 形（camelCase フィールド名・
// enum 文字列・nullable / omitempty の挙動）どおりにワイヤーへ出ることを固定する回帰ガード。
//
// internal/proto は canonical(Textro99-Proto) の type alias 再輸出なので、これは
// 「サーバーが実際に送出する JSON」の形を検証する。canonical 側でフィールド名・型・enum が
// 変わればこのテストが落ち、契約変更の見落としを CI で検知できる。
func TestWireGolden(t *testing.T) {
	self := PlayerId("p1")

	cases := []struct {
		name string
		v    any
		want string
	}{
		// ── S2C ──
		{"MatchStart",
			MatchStart{
				MatchId: "m-1", SelfPlayerId: "p1",
				Players:      []PlayerSummary{{PlayerId: "p1", DisplayName: "p1", ComboValue: 0, DakenStackCount: 0, DakenStackLimit: 20, BadgeCount: 0, Alive: true}},
				InitialDaken: DakenInstance{DakenId: "p1-1", Type: DakenNormal, Text: "ねこ", DifficultyLevel: 0, TimeLimitMs: 5000, IssuedAtServerTimeMs: 0},
				Parameters:   GameParametersPublicSubset{StackLimit: 20, TrapTriggerInterval: 5, PersonalDifficultyStep: 20, DifficultyMaxLevel: 10},
			},
			`{"matchId":"m-1","selfPlayerId":"p1","players":[{"playerId":"p1","displayName":"p1","comboValue":0,"dakenStackCount":0,"dakenStackLimit":20,"badgeCount":0,"alive":true,"rank":0}],"initialDaken":{"dakenId":"p1-1","type":"Normal","text":"ねこ","difficultyLevel":0,"timeLimitMs":5000,"issuedAtServerTimeMs":0},"parameters":{"stackLimit":20,"trapTriggerInterval":5,"personalDifficultyStep":20,"difficultyMaxLevel":10}}`},
		{"Welcome", Welcome{PlayerId: "p1"}, `{"playerId":"p1"}`},
		{"ComboUpdated", ComboUpdated{ComboValue: 28, Delta: 14, Reason: ComboClear}, `{"comboValue":28,"delta":14,"reason":"Clear"}`},
		{"DakenIssued",
			DakenIssued{Daken: []DakenInstance{{DakenId: "p1-2", Type: DakenEnemySent, Text: "x"}}},
			`{"daken":[{"dakenId":"p1-2","type":"EnemySent","text":"x","difficultyLevel":0,"timeLimitMs":0,"issuedAtServerTimeMs":0}]}`},
		{"DakenExpired", DakenExpired{DakenId: "p1-1"}, `{"dakenId":"p1-1"}`},
		{"DifficultyUpdated", DifficultyUpdated{GlobalLevel: 1, PersonalLevel: 1, EffectiveLevel: 2}, `{"globalLevel":1,"personalLevel":1,"effectiveLevel":2}`},
		{"AttackIncoming", AttackIncoming{WarningId: "w-1", AttackerId: "p2", Power: 28, GraceMs: 1500}, `{"warningId":"w-1","attackerId":"p2","power":28,"graceMs":1500}`},
		{"DakenStackUpdated", DakenStackUpdated{Count: 5, Limit: 20, TrapPending: true}, `{"count":5,"limit":20,"trapPending":true}`},
		// KoNotified: 自滅時は attackerId を「キー省略でなく明示 null」で送る（決定D）。
		{"KoNotified/自滅", KoNotified{AttackerId: nil, VictimId: "p3", BadgesTransferred: 0}, `{"attackerId":null,"victimId":"p3","badgesTransferred":0}`},
		{"KoNotified/通常", KoNotified{AttackerId: &self, VictimId: "p3", BadgesTransferred: 2}, `{"attackerId":"p1","victimId":"p3","badgesTransferred":2}`},
		{"PlayerListUpdated",
			PlayerListUpdated{Players: []PlayerSummary{{PlayerId: "p1", DisplayName: "p1", DakenStackLimit: 20, Alive: true}}, AliveCount: 1},
			`{"players":[{"playerId":"p1","displayName":"p1","comboValue":0,"dakenStackCount":0,"dakenStackLimit":20,"badgeCount":0,"alive":true,"rank":0}],"aliveCount":1}`},
		{"GameOver",
			GameOver{Rank: 1, KoCount: 1, FinalBadgeCount: 2, TypingStats: TypingStats{TotalDakenCleared: 30, TotalMiss: 2, MaxCombo: 88, ElapsedMs: 123456}},
			`{"rank":1,"koCount":1,"finalBadgeCount":2,"typingStats":{"totalDakenCleared":30,"totalMiss":2,"maxCombo":88,"elapsedMs":123456}}`},
		{"MatchmakingStatus/待機", MatchmakingStatus{WaitingCount: 3, MinPlayers: 20}, `{"waitingCount":3,"minPlayers":20}`},

		// ── C2S（サーバーが受ける形も固定）──
		{"DakenClearReport", DakenClearReport{DakenId: "p1-1", IsMiss: true, MissCount: 2, ElapsedMs: 1200}, `{"dakenId":"p1-1","isMiss":true,"missCount":2,"elapsedMs":1200}`},
		{"StrategySelect", StrategySelect{StrategyId: 3}, `{"strategyId":3}`},
	}

	for _, c := range cases {
		got, err := json.Marshal(c.v)
		if err != nil {
			t.Errorf("%s: marshal error: %v", c.name, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("%s: ワイヤー形式が契約とズレている\n  got  %s\n  want %s", c.name, got, c.want)
		}
	}
}
