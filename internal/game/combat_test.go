package game

import (
	"testing"

	"textro99/internal/proto"
)

// #21a 威力算出・個数変換（純関数）
func TestAttackPower(t *testing.T) {
	p := DefaultParameters().Attack // ratio1.0, perBadge0.1, cap1.0
	cases := []struct {
		combo, badge, want int
	}{
		{100, 0, 100},
		{100, 5, 150},  // ×1.5
		{100, 20, 200}, // cap +100%
		{45, 0, 45},
	}
	for _, c := range cases {
		if got := attackPower(c.combo, c.badge, p); got != c.want {
			t.Fatalf("attackPower(%d,%d)=%d, want %d", c.combo, c.badge, got, c.want)
		}
	}
}

func TestPowerToDakenCount(t *testing.T) {
	p := DefaultParameters().Attack // rate 0.1
	for _, c := range []struct{ power, want int }{{45, 4}, {150, 15}, {80, 8}, {5, 0}, {0, 0}} {
		if got := powerToDakenCount(c.power, p); got != c.want {
			t.Fatalf("powerToDakenCount(%d)=%d, want %d", c.power, got, c.want)
		}
	}
}

// #77 予告は相殺されず grace 経過で着弾する（相殺撤去後の基本挙動）。
func TestWarning_LandsWithoutOffset(t *testing.T) {
	s := newTestSession(t, "a", "b")
	s.Start()
	s.newWarning("a", "b", 50) // b に威力50の予告（floor(50*0.1)=5個着弾予定）
	res := s.Tick(2000)        // grace 1500 超過

	if len(s.warnings) != 0 {
		t.Fatalf("着弾後は予告が消える: %d 残", len(s.warnings))
	}
	if s.players["b"].stack != 5 {
		t.Fatalf("残威力5個が着弾すべき: stack=%d", s.players["b"].stack)
	}
	if di, _, ok := find[proto.DakenIssued](res); !ok || di.Daken[0].Type != proto.DakenEnemySent {
		t.Fatalf("EnemySent 着弾が来ていない: %+v ok=%v", di, ok)
	}
	if lp := s.players["b"].lastImpactor; lp == nil || *lp != "a" {
		t.Fatalf("直近着弾者が a になるべき: %v", lp)
	}
}

// #21c トラップ誘発（ハイウォーターマーク）
func TestStack_TrapMilestone(t *testing.T) {
	s := newTestSession(t, "a", "b")
	s.Start()
	a := s.players["a"]

	res := s.addStack(a, 6) // 6/5=1 > 0 → トラップ1個
	di, _, ok := find[proto.DakenIssued](res)
	if !ok || di.Daken[0].Type != proto.DakenTrap {
		t.Fatalf("トラップが発生すべき: %+v ok=%v", di, ok)
	}
	if su, _, ok := find[proto.DakenStackUpdated](res); !ok || su.Count != 6 || !su.TrapPending {
		t.Fatalf("DakenStackUpdated 不備: %+v ok=%v", su, ok)
	}
	// 同マイルストーン内（7）では再誘発しない。
	res2 := s.addStack(a, 1)
	if _, _, ok := find[proto.DakenIssued](res2); ok {
		t.Fatalf("同マイルストーンでトラップ再発してはいけない")
	}
}

// #21c 脱落＋バッジ総取り
func TestStack_EliminationTransfersBadges(t *testing.T) {
	s := newTestSession(t, "a", "b")
	s.Start()
	s.players["b"].badges = 2

	// a が b へ着弾させ続け、上限到達で脱落保留 → resolveEliminations で確定。KO実行者は a。
	res := s.landReceived(s.players["b"], s.params.Stack.Limit, "a")
	if s.players["b"].alive != true || !s.players["b"].pendingKO {
		t.Fatalf("着弾直後は脱落保留（未確定）であるべき: alive=%v pendingKO=%v", s.players["b"].alive, s.players["b"].pendingKO)
	}
	res = append(res, s.resolveEliminations()...)

	ko, _, ok := find[proto.KoNotified](res)
	if !ok || ko.VictimId != "b" || ko.AttackerId == nil || *ko.AttackerId != "a" || ko.BadgesTransferred != 2 {
		t.Fatalf("KoNotified 不備: %+v ok=%v", ko, ok)
	}
	if s.players["a"].badges != 2 {
		t.Fatalf("バッジ総取り: a.badges=%d, want 2", s.players["a"].badges)
	}
	if s.players["b"].alive {
		t.Fatalf("b は脱落しているべき")
	}
}

// #77 同時脱落のタイブレーク: 同一tickで2人が上限到達しても、キル数の多い方が最上位(rank1)。
func TestResolveEliminations_TieBreakByKo(t *testing.T) {
	s := newTestSession(t, "a", "b")
	s.Start()
	s.players["a"].koCount = 2 // a のほうがキル数多い＝強い
	s.players["b"].koCount = 0

	// 両者を同一tick内で上限到達（保留）にする。
	s.addStack(s.players["a"], s.params.Stack.Limit)
	s.addStack(s.players["b"], s.params.Stack.Limit)
	if !s.players["a"].pendingKO || !s.players["b"].pendingKO {
		t.Fatalf("両者とも脱落保留のはず: a=%v b=%v", s.players["a"].pendingKO, s.players["b"].pendingKO)
	}

	res := s.resolveEliminations()

	// 弱い b が先に脱落(rank2)、強い a が最後に脱落(rank1)。
	ranks := map[PlayerId]int{}
	for _, o := range res {
		if go1, ok := o.Msg.(proto.GameOver); ok {
			ranks[o.To.PlayerId] = go1.Rank
		}
	}
	if ranks["a"] != 1 || ranks["b"] != 2 {
		t.Fatalf("キル数優先のタイブレーク不備: a=%d(want1) b=%d(want2)", ranks["a"], ranks["b"])
	}
}

// #21d 時間切れ→積み残し（先読み在庫1件で単一タイムアウトを分離検証）
func TestDifficulty_TimeoutCarryover(t *testing.T) {
	s := newTestSession(t, "a", "b")
	s.params.Odai.LookaheadCount = 1 // #81 の複数先読みを切って +1 挙動を素で見る
	s.Start()
	res := s.Tick(6000) // base制限時間5000超過

	if _, _, ok := find[proto.DakenExpired](res); !ok {
		t.Fatalf("DakenExpired が来ていない")
	}
	if s.players["a"].stack != 1 {
		t.Fatalf("積み残し+1すべき: a.stack=%d", s.players["a"].stack)
	}
}

// #81 先読みストック: 開始時に N 件の通常ダケンがキューに積まれ、クリアで N を維持する。
func TestLookaheadStock_MaintainsN(t *testing.T) {
	s := newTestSession(t, "a", "b")
	want := s.params.Odai.LookaheadCount // 既定5
	da := startAndDaken(t, s, "a")
	a := s.players["a"]

	if got := len(a.issued); got != want {
		t.Fatalf("開始時の先読み在庫=%d, want %d", got, want)
	}
	// 1件クリア → 補充されて N を維持。
	s.ApplyDakenClear("a", proto.DakenClearReport{DakenId: da})
	normals := 0
	for _, d := range a.issued {
		if d.typ == proto.DakenNormal {
			normals++
		}
	}
	if normals != want {
		t.Fatalf("クリア後の通常ストック=%d, want %d", normals, want)
	}
}

// #81 被弾ダケンは現在＋次数手を崩さず offset 位置へ割り込む（InsertIndex つきで配信）。
func TestDamageDaken_InsertsAtOffset(t *testing.T) {
	s := newTestSession(t, "a", "b")
	s.Start() // a のキューは通常 N=5 件
	a := s.players["a"]
	offset := s.params.Odai.DamageInsertOffset // 既定3

	res := s.landReceived(a, 2, "b") // 被弾2件
	di, _, ok := find[proto.DakenIssued](res)
	if !ok || di.InsertIndex == nil || *di.InsertIndex != offset {
		t.Fatalf("被弾は InsertIndex=%d で配信すべき: %+v", offset, di)
	}
	// サーバー側キューでも offset 位置に EnemySent が並ぶ（現在＋次2件は通常のまま）。
	for i := 0; i < offset; i++ {
		if a.issued[i].typ != proto.DakenNormal {
			t.Fatalf("先頭%d件は通常のはず: index %d が %s", offset, i, a.issued[i].typ)
		}
	}
	if a.issued[offset].typ != proto.DakenEnemySent {
		t.Fatalf("offset位置は被弾のはず: %s", a.issued[offset].typ)
	}
}

// #86 被弾ダケンの時間切れ: 先頭1件のみ expire し、stack を1減算する。
func TestDifficulty_EnemySentExpires(t *testing.T) {
	s := newTestSession(t, "a", "b")
	s.Start()
	a := s.players["a"]
	clearIssued(a)
	clearIssued(s.players["b"])
	a.stack = 3
	for i := 0; i < 3; i++ {
		s.issueDaken(a, proto.DakenEnemySent) // t=0, tl=base(5000)
	}

	// tick 1: 先頭だけ expire → stack 3→2, 残り2件の先頭は issuedAtMs=6000 にリセット
	res := s.Tick(6000)
	if n := countExpired(res); n != 1 {
		t.Fatalf("先頭1件だけ expire すべき: got %d", n)
	}
	if a.stack != 2 {
		t.Fatalf("被弾 expire で stack-1 すべき: got %d, want 2", a.stack)
	}
	if len(a.issued) != 2 {
		t.Fatalf("残り2件あるべき: issued=%d", len(a.issued))
	}

	// tick 2: 新ヘッドの issuedAtMs=6000+5000=11000 まで進める → expire, stack 2→1
	s.Tick(5150) // elapsedMs = 11150
	if a.stack != 1 {
		t.Fatalf("2件目 expire で stack=1 すべき: got %d", a.stack)
	}

	// tick 3: 最後の1件 expire → stack 1→0
	s.Tick(5150) // elapsedMs = 16300, head.issuedAtMs=11150+5000=16150 < 16300
	if a.stack != 0 {
		t.Fatalf("3件目 expire で stack=0 すべき: got %d", a.stack)
	}
	if len(a.issued) != 0 {
		t.Fatalf("全件 expire 後はキュー空: issued=%d", len(a.issued))
	}
}

// #86 先読みダケンはヘッドより先に期限切れしない。
func TestLookahead_DoesNotExpireBeforeHead(t *testing.T) {
	s := newTestSession(t, "a", "b")
	s.Start()
	a := s.players["a"]
	initialCount := len(a.issued) // 5件（LookaheadCount=5）
	clearIssued(s.players["b"])   // b を対象外にする

	// 全ダケンの issuedAtMs=0, timeLimitMs=5000。旧ロジックなら5件一斉 expire。
	res := s.Tick(6000)
	expired := countExpired(res)
	if expired != 1 {
		t.Fatalf("先頭1件だけ expire すべき: got %d (旧ロジックなら %d 件一斉 expire)", expired, initialCount)
	}
}

// #86 ヘッド除去後に新ヘッドの issuedAtMs がリセットされる。
func TestRefreshHead_ResetsIssuedAt(t *testing.T) {
	s := newTestSession(t, "a", "b")
	out := s.Start()
	a := s.players["a"]
	var da proto.DakenId
	for _, o := range out {
		if ms, ok := o.Msg.(proto.MatchStart); ok && o.To.PlayerId == "a" {
			da = ms.InitialDaken.DakenId
		}
	}

	// ヘッドをクリア → 新ヘッドの issuedAtMs が現在時刻(0)にリセットされる。
	s.ApplyDakenClear("a", proto.DakenClearReport{DakenId: da})
	if len(a.issued) > 0 && a.issued[0].issuedAtMs != 0 {
		t.Fatalf("新ヘッドの issuedAtMs がリセットされるべき: got %d, want 0", a.issued[0].issuedAtMs)
	}

	clearIssued(s.players["b"]) // b を対象外にする
	// 時間を進めてからヘッドを expire → 新ヘッドは進めた時刻にリセット。
	s.Tick(6000)
	if len(a.issued) > 0 && a.issued[0].issuedAtMs != 6000 {
		t.Fatalf("expire 後の新ヘッド issuedAtMs=%d, want 6000", a.issued[0].issuedAtMs)
	}
}

// #78 トラップダケンの時間切れ: 失敗扱いで TrapMissPenalty を加算し、次のお題は出さない。
func TestDifficulty_TrapExpiresWithPenalty(t *testing.T) {
	s := newTestSession(t, "a", "b")
	s.Start()
	a := s.players["a"]
	clearIssued(a)
	s.issueDaken(a, proto.DakenTrap) // t=0, tl=base
	res := s.Tick(6000)

	if _, _, ok := find[proto.DakenExpired](res); !ok {
		t.Fatalf("トラップ DakenExpired が来ていない")
	}
	if a.stack != s.params.Stack.TrapMissPenalty {
		t.Fatalf("トラップ時間切れはペナルティ加算すべき: stack=%d want %d", a.stack, s.params.Stack.TrapMissPenalty)
	}
	if len(a.issued) != 0 {
		t.Fatalf("トラップ expire は次のお題を出さない: issued=%d", len(a.issued))
	}
}

// #21d 全体難易度上昇
func TestDifficulty_GlobalRise(t *testing.T) {
	s := newTestSession(t, "a", "b")
	s.Start()
	res := s.Tick(30000) // globalIntervalMs 到達
	if s.globalLevel != 1 {
		t.Fatalf("全体難易度=%d, want 1", s.globalLevel)
	}
	if _, _, ok := find[proto.DifficultyUpdated](res); !ok {
		t.Fatalf("DifficultyUpdated が来ていない")
	}
}

// #21b 予告が猶予超過で着弾する
func TestExpireWarnings_Lands(t *testing.T) {
	s := newTestSession(t, "a", "b")
	s.Start()
	s.newWarning("a", "b", 14) // floor(14*0.1)=1 個着弾予定
	res := s.Tick(2000)        // grace 1500 超過

	if s.players["b"].stack != 1 {
		t.Fatalf("着弾で stack+1 すべき: %d", s.players["b"].stack)
	}
	if di, _, ok := find[proto.DakenIssued](res); !ok || di.Daken[0].Type != proto.DakenEnemySent {
		t.Fatalf("EnemySent 着弾が来ていない: %+v ok=%v", di, ok)
	}
	if len(s.warnings) != 0 {
		t.Fatalf("着弾後は予告が消える: %d 残", len(s.warnings))
	}
}

// clearIssued はテスト対象を絞るため発行済みキューを空にする（初期の通常お題を除外）。
func clearIssued(ps *playerState) { ps.issued = nil }

// countExpired は出力中の DakenExpired の件数を数える。
func countExpired(out []Outbound) int {
	n := 0
	for _, o := range out {
		if _, ok := o.Msg.(proto.DakenExpired); ok {
			n++
		}
	}
	return n
}
