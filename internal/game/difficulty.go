package game

import (
	"sort"

	"textro99/internal/proto"
)

// difficulty.go は【#21d】難易度（全体＝経過時間 / 個人＝コンボ連動）と、
// 時間切れ（積み残し）・予告着弾の時系列処理。

// personalLevel は個人コンボ連動の難易度段階。
func (s *Session) personalLevel(ps *playerState) int {
	step := s.params.Combo.PersonalDifficultyStep
	if step <= 0 {
		return 0
	}
	l := ps.p.Combo() / step
	if m := s.params.Combo.PersonalDifficultyMaxLevel; l > m {
		l = m
	}
	return l
}

// effectiveLevel は実効難易度＝min(全体+個人, maxLevel)。
func (s *Session) effectiveLevel(ps *playerState) int {
	l := s.globalLevel + s.personalLevel(ps)
	if m := s.params.Difficulty.MaxLevel; l > m {
		l = m
	}
	return l
}

func (s *Session) difficultyUpdatedFor(ps *playerState) Outbound {
	return to(ps.id, proto.DifficultyUpdated{
		GlobalLevel: s.globalLevel, PersonalLevel: s.personalLevel(ps), EffectiveLevel: s.effectiveLevel(ps),
	})
}

// advanceGlobalDifficulty は経過時間に応じて全体難易度を上げ、生存者へ通知する。
func (s *Session) advanceGlobalDifficulty() []Outbound {
	iv := s.params.Difficulty.GlobalIntervalMs
	if iv <= 0 {
		return nil
	}
	want := int(s.elapsedMs / int64(iv))
	if m := s.params.Difficulty.MaxLevel; want > m {
		want = m
	}
	if want <= s.globalLevel {
		return nil
	}
	s.globalLevel = want
	var out []Outbound
	for _, pid := range s.order {
		if ps := s.players[pid]; ps.alive {
			out = append(out, s.difficultyUpdatedFor(ps))
		}
	}
	return out
}

// expireTimeouts はキュー先頭（プレイヤーが作業中のお題）だけを時間切れ判定する（#86）。
// 先読みストック（キュー2番目以降）はまだ到達していないため期限切れにしない。
// 先頭が除去されると新先頭の issuedAtMs を現在時刻にリセットし、完全な制限時間を与える。
//
// 種別で後処理が違う:
//   - 通常    : 積み残し(+1)＋先読み在庫を N に補充（#81）。
//   - 被弾    : stack を1減算（着弾時に加算した分を戻す。#86）。
//   - トラップ: 未処理トラップは失敗扱いとし TrapMissPenalty を加算。
func (s *Session) expireTimeouts() []Outbound {
	var out []Outbound
	for _, pid := range s.order {
		ps := s.players[pid]
		if !ps.alive || len(ps.issued) == 0 {
			continue
		}
		d := ps.issued[0]
		if s.elapsedMs < d.issuedAtMs+int64(d.timeLimitMs) {
			continue
		}
		if ps.p.Combo() > 0 {
			oc := ps.p.ResetCombo()
			out = append(out, to(pid, proto.ComboUpdated{ComboValue: 0, Delta: oc.Delta, Reason: proto.ComboMiss}))
		}
		ps.removeIssued(d.id)
		ps.refreshHead(s.elapsedMs)
		out = append(out, to(pid, proto.DakenExpired{DakenId: d.id}))
		switch d.typ {
		case proto.DakenNormal:
			out = append(out, s.addStack(ps, 1)...)
			if rest := s.refillNormalStock(ps); len(rest) > 0 {
				out = append(out, to(pid, proto.DakenIssued{Daken: rest}))
			}
		case proto.DakenTrap:
			out = append(out, s.addStack(ps, s.params.Stack.TrapMissPenalty)...)
		case proto.DakenEnemySent:
			if ps.stack > 0 {
				ps.stack--
			}
			out = append(out, s.dakenStackUpdated(ps, false))
		}
	}
	return out
}

// expireWarnings は猶予を過ぎた予告を着弾させる（相殺されなかった攻撃）。
func (s *Session) expireWarnings() []Outbound {
	var expired []proto.WarningId
	for id, w := range s.warnings {
		if s.elapsedMs >= w.impactAtMs() {
			expired = append(expired, id)
		}
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i] < expired[j] })

	var out []Outbound
	for _, id := range expired {
		w := s.warnings[id]
		if w == nil {
			continue
		}
		s.removeWarning(id)
		vp := s.players[w.victim]
		if vp == nil || !vp.alive {
			continue
		}
		cnt := powerToDakenCount(w.power, s.params.Attack)
		out = append(out, s.landReceived(vp, cnt, w.attacker)...)
	}
	return out
}
