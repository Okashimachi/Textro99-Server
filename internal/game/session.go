package game

import (
	"fmt"
	"math/rand"
	"sort"

	"textro99/internal/proto"
)

// session.go は【層1コア】1試合の状態機械。純粋・Tick(dt)駆動（時計は持たず room が dt を渡す）。
// 戦闘ロジックは attack.go(#21a 威力) / offset.go(#77 クリア攻撃・予告) / stack.go(#21c) /
// difficulty.go(#21d) に分割。

// SessionState は試合の状態。
type SessionState int

const (
	WaitingStart SessionState = iota
	Running
	Finished
)

// Recipient は Outbound の宛先。Broadcast=true で全員。
type Recipient struct {
	PlayerId  PlayerId
	Broadcast bool
}

// Outbound は session が返す「宛先つきメッセージ」。Msg は proto.<Message> の値で、
// room が Envelope に包んで実際の接続へ送る（game は通信を知らない）。
type Outbound struct {
	To  Recipient
	Msg any
}

func to(pid PlayerId, msg any) Outbound { return Outbound{To: Recipient{PlayerId: pid}, Msg: msg} }
func broadcastMsg(msg any) Outbound     { return Outbound{To: Recipient{Broadcast: true}, Msg: msg} }

// issuedDaken はサーバーが発行済みのダケン（整合検証・打鍵数引き・時間切れ監視の元）。
// ps.issued は発行順を保った slice（#81）。クライアントのお題キューと同じ並びを権威的に持ち、
// 被弾ダケンの割り込み位置を算出できるようにする。
type issuedDaken struct {
	id          proto.DakenId
	keystrokes  int
	typ         proto.DakenType
	timeLimitMs int
	issuedAtMs  int64
}

// warning は予告(AttackWarning・#77)。power は着弾威力、issuedAt+grace で expire→着弾する。
type warning struct {
	id         proto.WarningId
	attacker   PlayerId
	victim     PlayerId
	power      int
	issuedAtMs int64
	graceMs    int
}

// impactAtMs は着弾予定時刻。
func (w *warning) impactAtMs() int64 { return w.issuedAtMs + int64(w.graceMs) }

// playerState は1人分の横断状態。per-player に属する状態はここに集約する。
type playerState struct {
	id            PlayerId
	name          string
	p             Player // コンボ（判定式は combo.go）
	stack         int    // ダケンスタック（受領ダケン数。上限で脱落）
	trapMilestone int    // トラップ誘発のハイウォーターマーク（#21c）
	badges        int
	koCount       int
	strategy      int // 現在の作戦（既定4）
	alive         bool
	rank          int // 脱落時に確定した最終順位（#80。生存中は0＝summariesで動的算出）

	// リザルト用タイプ統計（GameOver.TypingStats へ集計。#51）。
	maxCombo          int // 到達した最大コンボ（終了時点ではなく高水位）
	totalDakenCleared int // クリアしたダケン総数
	totalMiss         int // 総ミス打鍵数（各報告の missCount の合計）

	issued           []*issuedDaken    // 発行順のお題キュー（#81・クライアント表示順と一致）
	pendingAgainstMe []proto.WarningId // 自分宛の予告（カウンター/巻き添え）
	lastImpactor     *PlayerId         // 直近着弾者（リベンジ／KO帰属）
	dakenSeq         int

	// 脱落は即時確定せず保留する（#77 タイブレーク）。1tick内で複数が上限到達しても、
	// resolveEliminations でまとめて「弱い順」に確定し、最強者を最後に脱落＝最上位にする。
	pendingKO     bool
	pendingKiller *PlayerId
}

// PlayerInit は NewSession に渡す初期プレイヤー情報。
type PlayerInit struct {
	Id          PlayerId
	DisplayName string
}

// Session は1試合。strategies/words はDIPで注入される部品実装。
type Session struct {
	id         proto.MatchId
	params     GameParameters
	strategies map[int]TargetingStrategy
	words      WordSource
	rng        *rand.Rand

	players     map[PlayerId]*playerState
	order       []PlayerId // 安定順（隣狙いの基準は targeting 側でIDソート）
	warnings    map[proto.WarningId]*warning
	state       SessionState
	elapsedMs   int64
	aliveCount  int
	warnSeq     int
	globalLevel int // 全体難易度段階（#21d）
}

// NewSession は WaitingStart 状態の試合を作る。まだお題は配らない（Start で配る）。
func NewSession(id proto.MatchId, params GameParameters, strategies map[int]TargetingStrategy,
	words WordSource, rng *rand.Rand, inits []PlayerInit) *Session {
	s := &Session{
		id: id, params: params, strategies: strategies, words: words, rng: rng,
		players:  make(map[PlayerId]*playerState, len(inits)),
		warnings: make(map[proto.WarningId]*warning),
		state:    WaitingStart,
	}
	for _, in := range inits {
		s.players[in.Id] = &playerState{
			id: in.Id, name: in.DisplayName, strategy: 4, alive: true,
		}
		s.order = append(s.order, in.Id)
	}
	s.aliveCount = len(inits)
	return s
}

// State は現在の状態を返す。
func (s *Session) State() SessionState { return s.state }

// Snapshot は現在の全プレイヤー要約と生存人数を返す（room/StatePublisher が盤面の定期配信に使う）。
func (s *Session) Snapshot() ([]proto.PlayerSummary, int) {
	return s.summaries(), s.aliveCount
}

// Start は WaitingStart→Running へ遷移し、各プレイヤーへ MatchStart（初期お題つき）を配る。
func (s *Session) Start() []Outbound {
	if s.state != WaitingStart {
		return nil
	}
	s.state = Running
	summaries := s.summaries()
	out := make([]Outbound, 0, len(s.order))
	for _, pid := range s.order {
		ps := s.players[pid]
		d := s.issueDaken(ps, proto.DakenNormal)
		out = append(out, to(pid, proto.MatchStart{
			MatchId:      s.id,
			SelfPlayerId: pid,
			Players:      summaries,
			InitialDaken: d,
			Parameters:   s.publicParams(),
		}))
		// 先読みストック: 残り N-1 件を末尾に積んで NEXT を初手から満たす（#81）。
		if rest := s.refillNormalStock(ps); len(rest) > 0 {
			out = append(out, to(pid, proto.DakenIssued{Daken: rest}))
		}
	}
	return out
}

// refillNormalStock は通常ダケンの先読み在庫を LookaheadCount まで補充し、新規発行分を返す（#81）。
// クリア/時間切れで1件減るたびに1件補充して常時 N を保つ。返り値は末尾追加ぶんの DakenInstance。
func (s *Session) refillNormalStock(ps *playerState) []proto.DakenInstance {
	want := s.params.Odai.LookaheadCount
	if want < 1 {
		want = 1
	}
	have := 0
	for _, d := range ps.issued {
		if d.typ == proto.DakenNormal {
			have++
		}
	}
	var out []proto.DakenInstance
	for ; have < want; have++ {
		out = append(out, s.issueDaken(ps, proto.DakenNormal))
	}
	return out
}

// ApplyDakenClear は判定済みのお題クリア報告を受け、コンボ確定＋種別ごとの後処理を返す。
func (s *Session) ApplyDakenClear(from PlayerId, r proto.DakenClearReport) []Outbound {
	ps := s.players[from]
	if ps == nil || !ps.alive || s.state != Running {
		return nil
	}
	d := ps.findIssued(r.DakenId)
	if d == nil {
		return nil // dakenId 整合検証: 発行中でない報告は無視（プロトコル仕様7章）
	}
	pos := ps.removeIssued(r.DakenId)
	if pos == 0 {
		ps.refreshHead(s.elapsedMs)
	}

	prevPersonal := s.personalLevel(ps)
	outcome := ps.p.ApplyDakenClear(r.MissCount, d.keystrokes, s.params)

	// リザルト統計を集計（#51）。1報告=1ダケンクリア。総ミス打鍵数は missCount を積算、コンボは高水位を維持。
	ps.totalDakenCleared++
	ps.totalMiss += r.MissCount
	if outcome.Value > ps.maxCombo {
		ps.maxCombo = outcome.Value
	}

	out := []Outbound{to(from, proto.ComboUpdated{ComboValue: outcome.Value, Delta: outcome.Delta, Reason: comboReasonToProto(outcome.Reason)})}

	// 攻撃はダケンクリア起点（#77）。ノーミスクリアなら現在コンボを威力に、作戦の対象へ予告を出す。
	// コンボは消費しないため、連続クリアで火力が伸びる。ミス時は combo=0 で発火しない。
	if r.MissCount == 0 {
		out = append(out, s.fireClearAttack(ps)...)
	}

	switch d.typ {
	case proto.DakenNormal:
		if rest := s.refillNormalStock(ps); len(rest) > 0 { // 先読み在庫を N に戻す（#81）
			out = append(out, to(from, proto.DakenIssued{Daken: rest}))
		}
	case proto.DakenEnemySent:
		if ps.stack > 0 {
			ps.stack--
		}
		out = append(out, s.dakenStackUpdated(ps, false))
	case proto.DakenTrap:
		if r.MissCount > 0 { // トラップミス→ペナルティ加算（#21c）
			out = append(out, s.addStack(ps, s.params.Stack.TrapMissPenalty)...)
		}
	}

	if ps.alive && s.personalLevel(ps) != prevPersonal { // 個人難易度が変わったら通知（#21d）
		out = append(out, s.difficultyUpdatedFor(ps))
	}
	out = append(out, s.resolveEliminations()...) // トラップミスで上限到達した場合の確定（#77）
	out = append(out, s.checkFinished()...)
	return out
}

// ApplyStrategy は現在の作戦を差し替える（次の攻撃で参照）。
func (s *Session) ApplyStrategy(from PlayerId, r proto.StrategySelect) []Outbound {
	ps := s.players[from]
	if ps == nil {
		return nil
	}
	if r.StrategyId >= 0 && r.StrategyId <= 9 {
		ps.strategy = r.StrategyId
	}
	return nil
}

// Tick は時間を dt 進め、全体難易度上昇・時間切れ積み残し・予告着弾・終了を処理する。
func (s *Session) Tick(dtMs int) []Outbound {
	if s.state != Running {
		return nil
	}
	s.elapsedMs += int64(dtMs)
	var out []Outbound
	out = append(out, s.advanceGlobalDifficulty()...) // #21d
	out = append(out, s.expireTimeouts()...)          // #21d 時間切れ→積み残し
	out = append(out, s.expireWarnings()...)          // #77 予告grace超過→着弾
	out = append(out, s.resolveEliminations()...)     // #77 同時脱落をタイブレークで確定
	out = append(out, s.checkFinished()...)
	return out
}

// checkFinished は生存1人以下で Finished へ遷移し、優勝者へ GameOver を返す。
func (s *Session) checkFinished() []Outbound {
	if s.state != Running || s.aliveCount > 1 {
		return nil
	}
	s.state = Finished
	var out []Outbound
	for _, pid := range s.order {
		ps := s.players[pid]
		if ps.alive {
			out = append(out, to(pid, proto.GameOver{
				Rank: 1, KoCount: ps.koCount, FinalBadgeCount: ps.badges,
				TypingStats: s.typingStats(ps),
			}))
		}
	}
	return out
}

// ── 共通ヘルパ ────────────────────────────────────────────

// typingStats は GameOver 用のリザルト統計を組み立てる（#51）。checkFinished /
// eliminateWithKO の両方の GameOver 生成箇所で共有する。ElapsedMs は試合経過時間。
func (s *Session) typingStats(ps *playerState) proto.TypingStats {
	return proto.TypingStats{
		TotalDakenCleared: ps.totalDakenCleared,
		TotalMiss:         ps.totalMiss,
		MaxCombo:          ps.maxCombo,
		ElapsedMs:         int(s.elapsedMs),
	}
}

// issueDaken は次のお題を発行し、キュー末尾に積んで DakenInstance を返す（通常の発行経路）。
func (s *Session) issueDaken(ps *playerState, typ proto.DakenType) proto.DakenInstance {
	d, inst := s.newDaken(ps, typ)
	ps.issued = append(ps.issued, d)
	return inst
}

// newDaken はお題を1件生成する（キューへは積まない）。割り込み発行（被弾）で使うため分離した（#81）。
func (s *Session) newDaken(ps *playerState, typ proto.DakenType) (*issuedDaken, proto.DakenInstance) {
	lvl := s.effectiveLevel(ps)
	var w Word
	if typ == proto.DakenTrap {
		w = s.words.NextTrap(s.rng)
	} else {
		w = s.words.Next(lvl, s.rng)
	}
	ps.dakenSeq++
	id := proto.DakenId(fmt.Sprintf("%s-%d", ps.id, ps.dakenSeq))
	tl := s.timeLimitFor(lvl)
	d := &issuedDaken{id: id, keystrokes: w.KeystrokeCount, typ: typ, timeLimitMs: tl, issuedAtMs: s.elapsedMs}
	return d, proto.DakenInstance{
		DakenId: id, Type: typ, Text: w.Text, DifficultyLevel: lvl,
		TimeLimitMs: tl, IssuedAtServerTimeMs: s.elapsedMs,
	}
}

// findIssued はキューから id のお題を探す（発行中でなければ nil）。
func (ps *playerState) findIssued(id proto.DakenId) *issuedDaken {
	for _, d := range ps.issued {
		if d.id == id {
			return d
		}
	}
	return nil
}

// removeIssued はキューから id のお題を除去する（順序保持）。除去した位置を返す（-1=不在）。
func (ps *playerState) removeIssued(id proto.DakenId) int {
	for i, d := range ps.issued {
		if d.id == id {
			ps.issued = append(ps.issued[:i], ps.issued[i+1:]...)
			return i
		}
	}
	return -1
}

// refreshHead は先頭ダケンの issuedAtMs を nowMs にリセットする（#86）。
// 先読みキューから昇格した新ヘッドに完全な制限時間を与えるために使う。
func (ps *playerState) refreshHead(nowMs int64) {
	if len(ps.issued) > 0 && ps.issued[0].issuedAtMs < nowMs {
		ps.issued[0].issuedAtMs = nowMs
	}
}

// insertIssuedAt は ds をキューの位置 at へ挿入する（at はクランプ済み前提）。
func (ps *playerState) insertIssuedAt(at int, ds []*issuedDaken) {
	ps.issued = append(ps.issued[:at], append(append([]*issuedDaken{}, ds...), ps.issued[at:]...)...)
}

// timeLimitFor は難易度段階のダケン制限時間。
func (s *Session) timeLimitFor(level int) int {
	o := s.params.Odai
	tl := o.BaseTimeLimitMs - o.PerLevelReductionMs*level
	if tl < o.MinTimeLimitMs {
		tl = o.MinTimeLimitMs
	}
	return tl
}

func (s *Session) dakenStackUpdated(ps *playerState, trapPending bool) Outbound {
	return to(ps.id, proto.DakenStackUpdated{Count: ps.stack, Limit: s.params.Stack.Limit, TrapPending: trapPending})
}

// resolveTargets は作戦idで対象を解決する。未登録は作戦4へフォールバック。
func (s *Session) resolveTargets(id int, ctx TargetingContext) []PlayerId {
	st := s.strategies[id]
	if st == nil {
		st = s.strategies[4]
	}
	if st == nil {
		return nil
	}
	return st.SelectTargets(ctx)
}

// targetingContext は現在の生存状況から作戦解決用スナップショットを組み立てる。
func (s *Session) targetingContext(ps *playerState) TargetingContext {
	alive := make([]PlayerView, 0, s.aliveCount)
	for _, pid := range s.order {
		q := s.players[pid]
		if !q.alive {
			continue
		}
		alive = append(alive, PlayerView{
			PlayerId: q.id, ComboValue: q.p.Combo(), DakenStackCount: q.stack,
			DakenStackLimit: s.params.Stack.Limit, BadgeCount: q.badges,
			IncomingWarnings: s.incomingCount(q.id),
		})
	}
	var pending []PlayerId // 新しい順（作戦1カウンター）
	for i := len(ps.pendingAgainstMe) - 1; i >= 0; i-- {
		if w := s.warnings[ps.pendingAgainstMe[i]]; w != nil {
			pending = append(pending, w.attacker)
		}
	}
	return TargetingContext{SelfId: ps.id, Alive: alive, PendingAttackers: pending, LastImpactorId: ps.lastImpactor, Rng: s.rng}
}

// incomingCount は pid を対象に現在 Pending 中の予告数。
func (s *Session) incomingCount(pid PlayerId) int {
	n := 0
	for _, w := range s.warnings {
		if w.victim == pid {
			n++
		}
	}
	return n
}

func (s *Session) summaries() []proto.PlayerSummary {
	ranks := s.currentRanks()
	out := make([]proto.PlayerSummary, 0, len(s.order))
	for _, pid := range s.order {
		p := s.players[pid]
		out = append(out, proto.PlayerSummary{
			PlayerId: p.id, DisplayName: p.name, ComboValue: p.p.Combo(),
			DakenStackCount: p.stack, DakenStackLimit: s.params.Stack.Limit,
			BadgeCount: p.badges, Alive: p.alive, Rank: ranks[pid],
		})
	}
	return out
}

// currentRanks は試合中のサーバー確定順位を全員ぶん返す（#80）。
// 生存者は強さ順（weaker の逆）に 1..aliveCount。脱落者は脱落時に確定した rank を使う
// （脱落rank は常に aliveCount より大きいので生存者の順位と重複しない）。強さ基準は同時脱落の
// タイブレーク [[resolveEliminations]] と同一（キル数>バッジ>コンボ>スタック少）＝盤面と結末が一貫する。
func (s *Session) currentRanks() map[PlayerId]int {
	live := make([]*playerState, 0, s.aliveCount)
	for _, pid := range s.order {
		if p := s.players[pid]; p.alive {
			live = append(live, p)
		}
	}
	sort.SliceStable(live, func(i, j int) bool { return s.weaker(live[j], live[i]) }) // 強い順
	ranks := make(map[PlayerId]int, len(s.order))
	for i, p := range live {
		ranks[p.id] = i + 1
	}
	for _, pid := range s.order {
		if p := s.players[pid]; !p.alive {
			ranks[pid] = p.rank
		}
	}
	return ranks
}

func (s *Session) publicParams() proto.GameParametersPublicSubset {
	return proto.GameParametersPublicSubset{
		StackLimit:             s.params.Stack.Limit,
		TrapTriggerInterval:    s.params.Stack.TrapTriggerInterval,
		PersonalDifficultyStep: s.params.Combo.PersonalDifficultyStep,
		DifficultyMaxLevel:     s.params.Difficulty.MaxLevel,
	}
}

// comboReasonToProto は内部の理由列挙を proto の契約値へ写像する（境界での変換）。
func comboReasonToProto(r ComboReason) proto.ComboReason {
	switch r {
	case ReasonClear:
		return proto.ComboClear
	case ReasonMiss:
		return proto.ComboMiss
	case ReasonConsumed:
		return proto.ComboConsumed
	default:
		return proto.ComboClear
	}
}
