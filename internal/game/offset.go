package game

import (
	"fmt"

	"textro99/internal/proto"
)

// offset.go は【#77】ダケンクリア起点の攻撃発火と、予告(AttackWarning)のライフサイクル。
// 旧 Enter=全消費／相殺／撃ち返し（#21b）は撤去した。コンボは消費せず、ノーミスクリアの
// たびに現在コンボを威力へ変換して対象へ予告を出す。着弾は expireWarnings(#21d) が処理する。

// fireClearAttack はノーミスのダケンクリア時に呼ばれ、現在コンボ×バッジ倍率の威力で
// 作戦の対象へ予告を出す（コンボは消費しない・#77）。威力0・対象不成立・閾値未満は何もしない。
func (s *Session) fireClearAttack(ps *playerState) []Outbound {
	if !ps.alive {
		return nil
	}
	combo := ps.p.Combo()
	if combo < s.params.Attack.MinComboToAttack {
		return nil
	}
	power := attackPower(combo, ps.badges, s.params.Attack)
	if power <= 0 {
		return nil
	}
	targets := s.resolveTargets(ps.strategy, s.targetingContext(ps))
	if len(targets) == 0 {
		return nil
	}
	return s.emitWarnings(ps.id, targets, power)
}

// emitWarnings は対象集合へ予告を発行する。複数対象（作戦0）は威力を均等分配（端数消滅）。
func (s *Session) emitWarnings(from PlayerId, targets []PlayerId, power int) []Outbound {
	per := power
	if len(targets) > 1 {
		per = power / len(targets)
	}
	if per <= 0 {
		return nil
	}
	out := make([]Outbound, 0, len(targets))
	for _, tid := range targets {
		w := s.newWarning(from, tid, per)
		out = append(out, to(tid, proto.AttackIncoming{
			WarningId: w.id, AttackerId: from, Power: per, GraceMs: s.params.Attack.WarningGraceMs,
		}))
	}
	return out
}

// newWarning は予告を発行し、被害者の pending に積む。
func (s *Session) newWarning(attacker, victim PlayerId, power int) *warning {
	s.warnSeq++
	w := &warning{
		id: fmt.Sprintf("w-%d", s.warnSeq), attacker: attacker, victim: victim,
		power: power, issuedAtMs: s.elapsedMs, graceMs: s.params.Attack.WarningGraceMs,
	}
	s.warnings[w.id] = w
	if vp := s.players[victim]; vp != nil {
		vp.pendingAgainstMe = append(vp.pendingAgainstMe, w.id)
	}
	return w
}

// removeWarning は予告を除去する（map と被害者の pending から）。
func (s *Session) removeWarning(id proto.WarningId) {
	w := s.warnings[id]
	if w == nil {
		return
	}
	delete(s.warnings, id)
	if vp := s.players[w.victim]; vp != nil {
		for i, x := range vp.pendingAgainstMe {
			if x == id {
				vp.pendingAgainstMe = append(vp.pendingAgainstMe[:i], vp.pendingAgainstMe[i+1:]...)
				break
			}
		}
	}
}
