// Command balancesim は多数の試合を headless（WS無し・game.Session を直接駆動）で回し、
// #29 のバランス調整に使う指標を出す。人間ペースの打鍵を模した Bot で、試合時間・最大コンボ・
// KO 分布・決着率を集計する。パラメータを振って「面白い攻防・妥当な試合時間」に寄せるための土台。
//
//	go run ./cmd/balancesim                 # 現行 DefaultParameters を計測
//	go run ./cmd/balancesim -preset tuned   # 調整案を計測
//	go run ./cmd/balancesim -players 30 -matches 50 -minCombo 30 -powerRate 0.08
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"sort"

	"textro99/internal/app"
	"textro99/internal/game"
	"textro99/internal/odai"
	"textro99/internal/proto"
)

// simPlayer は1プレイヤーのシミュ状態（お題キュー・打鍵ペース・成績）。
type simPlayer struct {
	pending  []proto.DakenId
	nextMs   int
	alive    bool
	maxCombo int
	koCount  int
	clears   int
	rank     int
}

func (pp *simPlayer) removeID(id proto.DakenId) {
	for i, x := range pp.pending {
		if x == id {
			pp.pending = append(pp.pending[:i], pp.pending[i+1:]...)
			return
		}
	}
}

func main() {
	preset := flag.String("preset", "default", "default=現行DefaultParameters / tuned=スノーボール抑制の調整案")
	players := flag.Int("players", 20, "1試合の人数")
	matches := flag.Int("matches", 30, "試合数")
	intervalMs := flag.Int("interval", 1500, "打鍵ペース平均ms(人間想定)")
	missRate := flag.Float64("miss", 0.08, "ミス率")
	minCombo := flag.Int("minCombo", -1, "attack.minComboToAttack 上書き(-1でpreset値)")
	powerRate := flag.Float64("powerRate", -1, "attack.powerToDakenRate 上書き(-1でpreset値)")
	stackLimit := flag.Int("stackLimit", -1, "stack.limit 上書き(-1でpreset値)")
	grace := flag.Int("grace", -1, "attack.warningGraceMs 上書き(-1でpreset値)")
	dumpJSON := flag.Bool("dumpjson", false, "解決した GameParameters を JSON で出して終了（config-front 投入用）")
	flag.Parse()

	params := buildParams(*preset)
	if *minCombo >= 0 {
		params.Attack.MinComboToAttack = *minCombo
	}
	if *powerRate >= 0 {
		params.Attack.PowerToDakenRate = *powerRate
	}
	if *stackLimit >= 0 {
		params.Stack.Limit = *stackLimit
	}
	if *grace >= 0 {
		params.Attack.WarningGraceMs = *grace
	}

	if *dumpJSON {
		b, _ := json.MarshalIndent(params, "", "  ")
		fmt.Println(string(b))
		return
	}

	fmt.Printf("========== balancesim preset=%s players=%d matches=%d interval=%dms miss=%.2f ==========\n",
		*preset, *players, *matches, *intervalMs, *missRate)
	fmt.Printf("  attack: comboToPower=%.2f powerToDaken=%.2f minCombo=%d grace=%dms\n",
		params.Attack.ComboToPowerRatio, params.Attack.PowerToDakenRate, params.Attack.MinComboToAttack, params.Attack.WarningGraceMs)
	fmt.Printf("  combo: base=%d perChar=%d / stack.limit=%d trapEvery=%d / odai.lookahead=%d\n\n",
		params.Combo.NoMissBaseGain, params.Combo.NoMissPerCharGain, params.Stack.Limit,
		params.Stack.TrapTriggerInterval, params.Odai.LookaheadCount)

	var (
		durations   []float64
		winnerCombo []float64
		winnerKOs   []float64
		snowball    []float64
		decided     int
	)
	for i := 0; i < *matches; i++ {
		r := runMatch(params, *players, *intervalMs, *missRate, rand.New(rand.NewSource(int64(i)+1)))
		durations = append(durations, float64(r.elapsedMs)/1000)
		winnerCombo = append(winnerCombo, float64(r.winnerMaxCombo))
		winnerKOs = append(winnerKOs, float64(r.winnerKOs))
		snowball = append(snowball, r.snowball)
		if r.decided {
			decided++
		}
	}

	fmt.Printf("決着率: %d/%d (%.0f%%)\n", decided, *matches, 100*float64(decided)/float64(*matches))
	report("試合時間(秒)", durations)
	report("優勝者 最大コンボ", winnerCombo)
	report("優勝者 KO数", winnerKOs)
	report("スノーボール度(勝者clear/中央clear)", snowball)
	fmt.Println("\n※Bot は固定ペース。実人間とは違うので絶対値でなく preset 間の相対比較で見る。")
}

func buildParams(preset string) game.GameParameters {
	p := game.DefaultParameters()
	if preset == "tuned" {
		// スノーボール抑制の推奨開始値（#29・balancesim で snowball~1.1 を確認した設定B）。
		// 実機プレイテストの出発点。config-front から上書きして微調整する前提。
		p.Attack.MinComboToAttack = 50   // ある程度コンボを貯めないと攻撃が出ない（毎クリア発火をやめる）
		p.Attack.PowerToDakenRate = 0.02 // 1発の被害を大幅緩和
		p.Attack.BadgePowerBonusCap = 0.5
		p.Attack.WarningGraceMs = 2500 // 予告→着弾に間を持たせる
		p.Stack.Limit = 60             // 上限を上げて即死を防ぐ
	}
	return p
}

type matchResult struct {
	elapsedMs      int
	winnerMaxCombo int
	winnerKOs      int
	snowball       float64
	decided        bool
}

func runMatch(params game.GameParameters, n, intervalMs int, missRate float64, rng *rand.Rand) matchResult {
	inits := make([]game.PlayerInit, n)
	for i := range inits {
		id := game.PlayerId(fmt.Sprintf("p-%d", i+1))
		inits[i] = game.PlayerInit{Id: id, DisplayName: string(id)}
	}
	s := game.NewSession("m", params, app.DefaultStrategies(), odai.NewStaticPool(), rng, inits)

	players := make(map[game.PlayerId]*simPlayer, n)
	for _, in := range inits {
		players[in.Id] = &simPlayer{alive: true, nextMs: rng.Intn(intervalMs)}
	}

	consume := func(out []game.Outbound) {
		for _, o := range out {
			pp := players[o.To.PlayerId]
			switch m := o.Msg.(type) {
			case proto.MatchStart:
				pp.pending = append(pp.pending, m.InitialDaken.DakenId)
			case proto.DakenIssued:
				for _, d := range m.Daken {
					pp.pending = append(pp.pending, d.DakenId)
				}
			case proto.DakenExpired:
				pp.removeID(m.DakenId)
			case proto.ComboUpdated:
				if m.ComboValue > pp.maxCombo {
					pp.maxCombo = m.ComboValue
				}
			case proto.GameOver:
				pp.alive = false
				pp.rank = m.Rank
				pp.koCount = m.KoCount
			}
		}
	}

	const tickMs = 150
	const capMs = 20 * 60 * 1000 // 20分で打ち切り（決着しない設定の検出用）
	consume(s.Start())
	simMs := 0
	for s.State() != game.Finished && simMs < capMs {
		for id, pp := range players {
			if !pp.alive || len(pp.pending) == 0 || simMs < pp.nextMs {
				continue
			}
			did := pp.pending[0]
			pp.pending = pp.pending[1:]
			miss := 0
			if rng.Float64() < missRate {
				miss = 1
			}
			pp.clears++
			consume(s.ApplyDakenClear(id, proto.DakenClearReport{DakenId: did, MissCount: miss}))
			jitter := 0.6 + 0.8*rng.Float64() // ±40%
			pp.nextMs = simMs + int(float64(intervalMs)*jitter)
		}
		consume(s.Tick(tickMs))
		simMs += tickMs
	}

	res := matchResult{elapsedMs: simMs}
	clears := make([]float64, 0, n)
	var winner *simPlayer
	winners := 0
	for _, pp := range players {
		clears = append(clears, float64(pp.clears))
		if pp.rank == 1 {
			winners++
			winner = pp
		}
	}
	res.decided = winners == 1
	if winner != nil {
		res.winnerMaxCombo = winner.maxCombo
		res.winnerKOs = winner.koCount
		res.snowball = float64(winner.clears) / math.Max(1, median(clears))
	}
	return res
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	return c[len(c)/2]
}

func report(label string, xs []float64) {
	if len(xs) == 0 {
		fmt.Printf("  %-32s (データなし)\n", label)
		return
	}
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	p := func(q float64) float64 { return c[int(q*float64(len(c)-1))] }
	fmt.Printf("  %-32s 中央=%.1f  min=%.1f  p90=%.1f  max=%.1f\n", label, p(0.5), c[0], p(0.9), c[len(c)-1])
}
