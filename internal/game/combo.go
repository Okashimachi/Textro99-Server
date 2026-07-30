package game

// ComboReason は1回のコンボ変化の理由。境界で proto.ComboReason へ写像する。
type ComboReason int

const (
	ReasonClear    ComboReason = iota // ノーミスクリアによる加算
	ReasonMiss                        // ミスタイプによる減衰
	ReasonConsumed                    // 攻撃発動による全消費
)

// ComboOutcome は1回のコンボ操作の確定結果（判定式の出力）。
type ComboOutcome struct {
	Value  int
	Delta  int
	Reason ComboReason
}

// Player は1人分のルール状態。per-player に属する状態はここへ集約する。
// 今後スタック・個人難易度などのフィールドが同居する。
type Player struct {
	combo int
}

// Combo は現在のコンボ値を返す。
func (p *Player) Combo() int { return p.combo }

// ApplyDakenClear は判定済みの DakenClearReport 相当（missCount）を受けてコンボを確定する。
// keystrokeCount はそのダケンのローマ字打鍵数（サーバーがダケン発行時に算出した正準値・決定C）。
//
//   - missCount == 0（ノーミスクリア）: combo += base + perChar×keystrokeCount（連続で伸びる）
//   - missCount > 0（ミスあり）        : combo を 0 にリセット（#77・連続を断つ。減衰はやめた）
//
// 打鍵の正誤判定はクライアント責務。ここは判定済み結果からコンボを算出するだけ。
func (p *Player) ApplyDakenClear(missCount, keystrokeCount int, gp GameParameters) ComboOutcome {
	if missCount <= 0 {
		gain := gp.Combo.NoMissBaseGain + gp.Combo.NoMissPerCharGain*keystrokeCount
		p.combo += gain
		return ComboOutcome{Value: p.combo, Delta: gain, Reason: ReasonClear}
	}
	return p.breakCombo() // ミスは連続を断つ（#77）
}

// ResetCombo は時間切れ等で連続を断ち切る（#77）。リセット前のコンボ量を Delta の絶対値で返す。
func (p *Player) ResetCombo() ComboOutcome { return p.breakCombo() }

// breakCombo はコンボを 0 にし、Reason=Miss の ComboOutcome を返す（ミス／時間切れ共通）。
func (p *Player) breakCombo() ComboOutcome {
	delta := -p.combo
	p.combo = 0
	return ComboOutcome{Value: 0, Delta: delta, Reason: ReasonMiss}
}
