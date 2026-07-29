package game

import (
	"math/rand"
	"testing"

	"textro99/internal/proto"
)

// ── テスト用スタブ（game は部品を import できないので、interface をテスト内で実装）──

type stubWords struct{}

func (stubWords) Next(int, *rand.Rand) Word { return Word{Text: "ねこ", KeystrokeCount: 4} }
func (stubWords) NextTrap(*rand.Rand) Word  { return Word{Text: "trap", KeystrokeCount: 10} }

// stubStrategy: Id と、対象を返す関数を差し替えられるテスト用作戦。
type stubStrategy struct {
	id int
	fn func(TargetingContext) []PlayerId
}

func (s stubStrategy) Id() int                                     { return s.id }
func (s stubStrategy) SelectTargets(c TargetingContext) []PlayerId { return s.fn(c) }

func newTestSession(t *testing.T, ids ...string) *Session {
	t.Helper()
	inits := make([]PlayerInit, len(ids))
	for i, id := range ids {
		inits[i] = PlayerInit{Id: id, DisplayName: id}
	}
	// 作戦4=先頭のOtherを狙う決定的スタブ。
	reg := map[int]TargetingStrategy{
		4: stubStrategy{id: 4, fn: func(c TargetingContext) []PlayerId {
			o := c.Others()
			if len(o) == 0 {
				return nil
			}
			return []PlayerId{o[0].PlayerId}
		}},
	}
	return NewSession("m1", DefaultParameters(), reg, stubWords{}, rand.New(rand.NewSource(1)), inits)
}

// Msg を型で探すヘルパ。
func find[T any](out []Outbound) (T, PlayerId, bool) {
	for _, o := range out {
		if m, ok := o.Msg.(T); ok {
			return m, o.To.PlayerId, true
		}
	}
	var zero T
	return zero, "", false
}

func TestSession_StartEmitsMatchStart(t *testing.T) {
	s := newTestSession(t, "a", "b")
	if s.State() != WaitingStart {
		t.Fatalf("初期状態=%v, want WaitingStart", s.State())
	}
	out := s.Start()
	if s.State() != Running {
		t.Fatalf("Start後=%v, want Running", s.State())
	}
	// 各プレイヤーに MatchStart（初期お題つき）。
	count := 0
	for _, o := range out {
		if ms, ok := o.Msg.(proto.MatchStart); ok {
			count++
			if ms.SelfPlayerId != o.To.PlayerId || ms.InitialDaken.DakenId == "" || len(ms.Players) != 2 {
				t.Fatalf("MatchStart 不備: %+v", ms)
			}
		}
	}
	if count != 2 {
		t.Fatalf("MatchStart 数=%d, want 2", count)
	}
}

func TestSession_ApplyDakenClear_ComboAndNextDaken(t *testing.T) {
	s := newTestSession(t, "a", "b")
	out := s.Start()
	// a の初期お題IDを取得。
	var dakenId proto.DakenId
	for _, o := range out {
		if ms, ok := o.Msg.(proto.MatchStart); ok && o.To.PlayerId == "a" {
			dakenId = ms.InitialDaken.DakenId
		}
	}

	res := s.ApplyDakenClear("a", proto.DakenClearReport{DakenId: dakenId, MissCount: 0})
	cu, _, ok := find[proto.ComboUpdated](res)
	if !ok || cu.Reason != proto.ComboClear || cu.Delta != 14 { // base10 + 4打鍵
		t.Fatalf("ComboUpdated=%+v ok=%v, want delta=14 Clear", cu, ok)
	}
	di, _, ok := find[proto.DakenIssued](res)
	if !ok || len(di.Daken) != 1 || di.Daken[0].DakenId == dakenId {
		t.Fatalf("次のお題が来ていない: %+v", di)
	}
}

func TestSession_ApplyDakenClear_UnknownDakenIdIgnored(t *testing.T) {
	s := newTestSession(t, "a", "b")
	s.Start()
	if res := s.ApplyDakenClear("a", proto.DakenClearReport{DakenId: "bogus"}); res != nil {
		t.Fatalf("未発行dakenIdは無視されるべき: %+v", res)
	}
}

// #77 ノーミスクリアで攻撃発火。コンボは消費されず（伸び続ける）、対象へ予告が飛ぶ。
func TestSession_ClearFiresAttack(t *testing.T) {
	s := newTestSession(t, "a", "b")
	da := startAndDaken(t, s, "a")

	res := s.ApplyDakenClear("a", proto.DakenClearReport{DakenId: da}) // combo=14

	// コンボは消費されない（Reason=Clear で加算、値が残る）。
	if cu, _, ok := find[proto.ComboUpdated](res); !ok || cu.Reason != proto.ComboClear || cu.ComboValue != 14 {
		t.Fatalf("クリアで加算 ComboUpdated 不備: %+v ok=%v", cu, ok)
	}
	if s.players["a"].p.Combo() != 14 {
		t.Fatalf("攻撃してもコンボは消費されない: got %d, want 14", s.players["a"].p.Combo())
	}
	// b へ AttackIncoming（威力=attackPower(14,0)=14）。
	ai, toPid, ok := find[proto.AttackIncoming](res)
	if !ok || toPid != "b" || ai.AttackerId != "a" || ai.Power != 14 || ai.GraceMs != DefaultParameters().Attack.WarningGraceMs {
		t.Fatalf("AttackIncoming 不備: %+v to=%s ok=%v", ai, toPid, ok)
	}
}

// #77 ミスクリアはコンボを0にリセットし、攻撃を出さない。
func TestSession_MissResetsComboNoAttack(t *testing.T) {
	s := newTestSession(t, "a", "b")
	da := startAndDaken(t, s, "a")
	r1 := s.ApplyDakenClear("a", proto.DakenClearReport{DakenId: da}) // combo=14, 次お題発行
	di, _, ok := find[proto.DakenIssued](r1)
	if !ok || len(di.Daken) == 0 {
		t.Fatalf("クリアで次のお題が発行されるべき")
	}
	// 次のお題をミスクリア。
	res := s.ApplyDakenClear("a", proto.DakenClearReport{DakenId: di.Daken[0].DakenId, IsMiss: true, MissCount: 1})

	if s.players["a"].p.Combo() != 0 {
		t.Fatalf("ミスでコンボ0リセットすべき: got %d", s.players["a"].p.Combo())
	}
	if cu, _, ok := find[proto.ComboUpdated](res); !ok || cu.Reason != proto.ComboMiss || cu.ComboValue != 0 {
		t.Fatalf("ミス ComboUpdated 不備: %+v ok=%v", cu, ok)
	}
	if _, _, ok := find[proto.AttackIncoming](res); ok {
		t.Fatalf("ミスクリアでは攻撃を出さない")
	}
}

// #77 対象不成立（生存が自分だけ）なら攻撃は出ず、コンボは維持される。
func TestSession_ClearNoTargetNoAttack(t *testing.T) {
	s := newTestSession(t, "a", "b")
	da := startAndDaken(t, s, "a")
	s.eliminateWithKO(s.players["b"], nil) // b を退場させ生存を a だけに

	res := s.ApplyDakenClear("a", proto.DakenClearReport{DakenId: da}) // combo=14
	if _, _, ok := find[proto.AttackIncoming](res); ok {
		t.Fatalf("対象不成立では攻撃を出さない")
	}
	if s.players["a"].p.Combo() != 14 {
		t.Fatalf("攻撃不発でもコンボは維持: got %d, want 14", s.players["a"].p.Combo())
	}
}

// #80 試合中の順位: 生存者は強さ順(キル数優先)で1..、脱落者は脱落時の確定順位。
func TestCurrentRanks_LiveByStrengthDeadByDeathRank(t *testing.T) {
	s := newTestSession(t, "a", "b", "c")
	s.Start()
	s.eliminateWithKO(s.players["c"], nil) // c 脱落 → rank=3（生存2人の下）
	s.players["a"].koCount = 1             // a を b より強く

	sum, _ := s.Snapshot()
	rank := map[proto.PlayerId]int{}
	for _, p := range sum {
		rank[p.PlayerId] = p.Rank
	}
	if rank["a"] != 1 || rank["b"] != 2 || rank["c"] != 3 {
		t.Fatalf("順位不備: a=%d b=%d c=%d, want 1,2,3", rank["a"], rank["b"], rank["c"])
	}
}

// startAndDaken は Start して pid の初期お題IDを返すヘルパ。
func startAndDaken(t *testing.T, s *Session, pid PlayerId) proto.DakenId {
	t.Helper()
	out := s.Start()
	for _, o := range out {
		if ms, ok := o.Msg.(proto.MatchStart); ok && o.To.PlayerId == pid {
			return ms.InitialDaken.DakenId
		}
	}
	t.Fatalf("%s の MatchStart が無い", pid)
	return ""
}

// clearOnce は pid の dakenId を missCount 付きでクリアし、次に発行された Normal お題IDを返す。
func clearOnce(t *testing.T, s *Session, pid PlayerId, dakenId proto.DakenId, missCount int) proto.DakenId {
	t.Helper()
	res := s.ApplyDakenClear(pid, proto.DakenClearReport{DakenId: dakenId, MissCount: missCount})
	di, _, ok := find[proto.DakenIssued](res)
	if !ok || len(di.Daken) == 0 {
		t.Fatalf("クリア後に次のお題が発行されていない: %+v", res)
	}
	return di.Daken[0].DakenId
}

// GameOver.TypingStats に クリア回数・ミス数・最大コンボ・経過時間が反映される（#51）。
// 特に MaxCombo は「終了時点のコンボ」ではなく高水位であること（ミスで減衰しても下がらない）を検証。
func TestSession_GameOverTypingStats_Winner(t *testing.T) {
	s := newTestSession(t, "a", "b")
	d := startAndDaken(t, s, "a")
	d = clearOnce(t, s, "a", d, 0) // combo 14, cleared 1, maxCombo 14
	d = clearOnce(t, s, "a", d, 0) // combo 28, cleared 2, maxCombo 28
	_ = clearOnce(t, s, "a", d, 2) // ミスで combo 0 リセット, cleared 3, totalMiss +2, maxCombo は 28 のまま

	if s.players["a"].p.Combo() != 0 {
		t.Fatalf("前提: ミスでコンボ0リセットのはず, got %d", s.players["a"].p.Combo())
	}

	s.eliminateWithKO(s.players["b"], nil)
	res := s.Tick(150) // 生存1人で Finished → a へ GameOver

	go1, toPid, ok := find[proto.GameOver](res)
	if !ok || toPid != "a" || go1.Rank != 1 {
		t.Fatalf("優勝者へ GameOver(rank1): %+v to=%s ok=%v", go1, toPid, ok)
	}
	want := proto.TypingStats{TotalDakenCleared: 3, TotalMiss: 2, MaxCombo: 28, ElapsedMs: 150}
	if go1.TypingStats != want {
		t.Fatalf("勝者 TypingStats=%+v, want %+v", go1.TypingStats, want)
	}
}

// 脱落プレイヤーの GameOver（eliminateWithKO 経由）にも統計が反映される（#51）。
func TestSession_GameOverTypingStats_Eliminated(t *testing.T) {
	s := newTestSession(t, "a", "b")
	d := startAndDaken(t, s, "b")
	s.Tick(200)                    // 経過時間を進める（2人生存中は終了しない）
	d = clearOnce(t, s, "b", d, 0) // cleared 1, combo 14, maxCombo 14
	_ = clearOnce(t, s, "b", d, 1) // cleared 2, totalMiss 1, ミスで combo 0, maxCombo は 14 のまま

	out := s.eliminateWithKO(s.players["b"], nil)
	go1, toPid, ok := find[proto.GameOver](out)
	if !ok || toPid != "b" {
		t.Fatalf("脱落者 b へ GameOver: %+v to=%s ok=%v", go1, toPid, ok)
	}
	want := proto.TypingStats{TotalDakenCleared: 2, TotalMiss: 1, MaxCombo: 14, ElapsedMs: 200}
	if go1.TypingStats != want {
		t.Fatalf("脱落者 TypingStats=%+v, want %+v", go1.TypingStats, want)
	}
}

func TestSession_TickFinishesWhenOneLeft(t *testing.T) {
	s := newTestSession(t, "a", "b")
	s.Start()
	if res := s.Tick(150); res != nil { // まだ2人
		t.Fatalf("2人生存中は終了しない: %+v", res)
	}
	s.eliminateWithKO(s.players["b"], nil)
	res := s.Tick(150)
	if s.State() != Finished {
		t.Fatalf("生存1人で Finished になるべき: %v", s.State())
	}
	go1, toPid, ok := find[proto.GameOver](res)
	if !ok || toPid != "a" || go1.Rank != 1 {
		t.Fatalf("優勝者へ GameOver(rank1): %+v to=%s ok=%v", go1, toPid, ok)
	}
}
