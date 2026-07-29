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

// expireTimeouts は制限時間を超えたお題を種別問わず打ち切る（#78）。
// 種別で後処理が違う:
//   - 通常     : 積み残し(+1)＋先読み在庫を N に補充（#81）。
//   - 被弾      : 画面から除去するだけ。着弾時に stack へ加算済みのため増減せず、次のお題も出さない
//     （時間内にクリアできなかった＝負担が残る。要マネージャー再検討）。
//   - トラップ  : 未処理トラップは失敗扱いとし TrapMissPenalty を加算（クリア時ミスと同待遇。要再検討）。
//
// いずれも DakenExpired を送ってクライアント側の凍結（同じお題が消えず次へ進まない）を解消する。
func (s *Session) expireTimeouts() []Outbound {
	var out []Outbound
	for _, pid := range s.order {
		ps := s.players[pid]
		if !ps.alive {
			continue
		}
		// キュー順（＝発行順・決定的）に時間切れを集める。
		var expired []*issuedDaken
		for _, d := range ps.issued {
			if s.elapsedMs >= d.issuedAtMs+int64(d.timeLimitMs) {
				expired = append(expired, d)
			}
		}
		// 時間切れ（クリア未達）は連続を断つ（#77）。1tickで複数切れても1回だけリセット・通知する。
		if len(expired) > 0 && ps.p.Combo() > 0 {
			oc := ps.p.ResetCombo()
			out = append(out, to(pid, proto.ComboUpdated{ComboValue: 0, Delta: oc.Delta, Reason: proto.ComboMiss}))
		}
		for _, d := range expired {
			ps.removeIssued(d.id)
			out = append(out, to(pid, proto.DakenExpired{DakenId: d.id}))
			switch d.typ {
			case proto.DakenNormal:
				out = append(out, s.addStack(ps, 1)...)             // 積み残し（通常ダケン1個分）
				if rest := s.refillNormalStock(ps); len(rest) > 0 { // 先読み在庫を N に戻す（#81）
					out = append(out, to(pid, proto.DakenIssued{Daken: rest}))
				}
			case proto.DakenTrap:
				out = append(out, s.addStack(ps, s.params.Stack.TrapMissPenalty)...)
			case proto.DakenEnemySent:
				// 除去のみ。stack は着弾時に加算済みのため触らない。
			}
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
