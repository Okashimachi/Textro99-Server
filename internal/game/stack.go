package game

import (
	"sort"

	"textro99/internal/proto"
)

// stack.go は【#21c】ダケンスタックの増減・トラップ誘発（ハイウォーターマーク）・脱落確定。

// landReceived は攻撃の着弾を処理する（相殺しきれなかった分・時間切れ着弾）。
// count 個の EnemySent ダケンを送り、スタックへ加算し、直近着弾者を記録する。
func (s *Session) landReceived(ps *playerState, count int, attacker PlayerId) []Outbound {
	if count <= 0 {
		return nil // 威力小で0個なら着弾なし
	}
	a := attacker
	ps.lastImpactor = &a
	insts := make([]proto.DakenInstance, 0, count)
	ds := make([]*issuedDaken, 0, count)
	for i := 0; i < count; i++ {
		d, inst := s.newDaken(ps, proto.DakenEnemySent)
		ds = append(ds, d)
		insts = append(insts, inst)
	}
	// 被弾は「現在＋次数手」を崩さないよう offset 手先へ割り込ませる（#81）。
	at := s.damageInsertPos(ps)
	ps.insertIssuedAt(at, ds)
	out := []Outbound{to(ps.id, proto.DakenIssued{Daken: insts, InsertIndex: &at})}
	out = append(out, s.addStack(ps, count)...)
	return out
}

// damageInsertPos は被弾ダケンの割り込み位置＝min(DamageInsertOffset, キュー長)。短ければ末尾。
func (s *Session) damageInsertPos(ps *playerState) int {
	at := s.params.Odai.DamageInsertOffset
	if at < 0 {
		at = 0
	}
	if at > len(ps.issued) {
		at = len(ps.issued)
	}
	return at
}

// addStack はスタックを n 増やし、トラップ誘発・脱落を処理する。
//
// トラップ誘発はハイウォーターマーク方式（trapMilestone 整数1個）。往復で連発しない。
// ⚠ トラップダケン自体は権威スタックには計上しない（誘発の連鎖を避ける。02_詳細企画書 1.3 の例に一致。
// この割り切りはプロトタイプ用でマネージャー再検討可）。脱落が先に成立する場合はトラップを出さない。
func (s *Session) addStack(ps *playerState, n int) []Outbound {
	if n <= 0 {
		return nil
	}
	ps.stack += n
	if ps.stack >= s.params.Stack.Limit { // 上限到達は脱落を保留（確定は resolveEliminations・#77）
		if !ps.pendingKO {
			ps.pendingKO = true
			ps.pendingKiller = ps.lastImpactor // 着弾時点の下手人を記録
		}
		return []Outbound{s.dakenStackUpdated(ps, false)}
	}

	var out []Outbound
	trapPending := false
	if iv := s.params.Stack.TrapTriggerInterval; iv > 0 {
		milestone := ps.stack / iv
		if milestone > ps.trapMilestone {
			num := milestone - ps.trapMilestone
			ps.trapMilestone = milestone
			trapPending = true
			daken := make([]proto.DakenInstance, 0, num)
			for i := 0; i < num; i++ {
				daken = append(daken, s.issueDaken(ps, proto.DakenTrap))
			}
			out = append(out, to(ps.id, proto.DakenIssued{Daken: daken}))
		}
	}
	out = append(out, s.dakenStackUpdated(ps, trapPending))
	return out
}

// eliminateWithKO は脱落を確定し、KO実行者へバッジ総取りを適用する。
// killer が nil / 生存していない場合は自滅（KO実行者なし・決定D。attackerId=nil で送出）。
func (s *Session) eliminateWithKO(ps *playerState, killer *PlayerId) []Outbound {
	if !ps.alive {
		return nil
	}
	ps.alive = false
	s.aliveCount--
	rank := s.aliveCount + 1 // 後に脱落するほど上位。最後の1人が1位
	ps.rank = rank           // 確定順位を保持（試合中の順位配信で使う・#80）

	var attackerId *PlayerId
	transferred := 0
	if killer != nil && *killer != ps.id {
		if kp := s.players[*killer]; kp != nil && kp.alive {
			transferred = ps.badges
			kp.badges += ps.badges
			kp.koCount++
			ps.badges = 0
			k := *killer
			attackerId = &k
		}
	}

	return []Outbound{
		broadcastMsg(proto.KoNotified{AttackerId: attackerId, VictimId: ps.id, BadgesTransferred: transferred}),
		to(ps.id, proto.GameOver{
			Rank: rank, KoCount: ps.koCount, FinalBadgeCount: ps.badges,
			TypingStats: s.typingStats(ps),
		}),
		broadcastMsg(proto.PlayerListUpdated{Players: s.summaries(), AliveCount: s.aliveCount}),
	}
}

// resolveEliminations は保留中(pendingKO)の脱落をまとめて確定する（#77）。
// 同一tickで複数が上限到達した場合、弱い順に脱落させることで、最強者が最後に脱落＝最上位になる。
// ＝全員が同時に倒れても「勝者なし」にはならず、強さ（キル数優先）で1位が決まる。
func (s *Session) resolveEliminations() []Outbound {
	var pend []*playerState
	for _, pid := range s.order {
		if ps := s.players[pid]; ps.alive && ps.pendingKO {
			pend = append(pend, ps)
		}
	}
	if len(pend) == 0 {
		return nil
	}
	// 弱い順（先に脱落＝下位）に並べる。強さの比較は koCount→badges→combo→stack少→id。
	sort.SliceStable(pend, func(i, j int) bool { return s.weaker(pend[i], pend[j]) })

	var out []Outbound
	for _, ps := range pend {
		ps.pendingKO = false
		out = append(out, s.eliminateWithKO(ps, ps.pendingKiller)...)
	}
	return out
}

// weaker は a が b より弱い（先に脱落させるべき）なら true。勝者確定のタイブレーク基準（#77）。
// 強さ = キル数 > バッジ数 > コンボ > スタックの少なさ。完全同値は id で決定的に割る。
func (s *Session) weaker(a, b *playerState) bool {
	if a.koCount != b.koCount {
		return a.koCount < b.koCount
	}
	if a.badges != b.badges {
		return a.badges < b.badges
	}
	if ca, cb := a.p.Combo(), b.p.Combo(); ca != cb {
		return ca < cb
	}
	if a.stack != b.stack {
		return a.stack > b.stack // スタックが多いほうが弱い
	}
	return a.id > b.id
}
