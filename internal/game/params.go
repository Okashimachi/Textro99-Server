// Package game は【層1・コア】戦闘の権威。純粋な計算のみで、ネットワーク・時間・I/O を持たない。
//
// per-player の判定式（combo/attack/offset/stack/difficulty）と、全体を調停する session
// 状態機械（Tick(dt) で進む）を内包する。game は他の internal 部品/スパインを import しない
// （proto は契約として参照可）。継ぎ目は ports.go（DIP）。すべての調整値は GameParameters 経由。
package game

import "fmt"

// GameParameters は数値バランスの全項目。正典は
// Textro99-Docs/03_サーバー仕様/04_パラメータ仕様.md。
// サーバーが起動時に config(ConfigProvider) 経由で外部取得し、失敗時は DefaultParameters()。
// クライアントへは MatchStart で公開サブセット（proto側）に絞って配信する。
type GameParameters struct {
	Combo      ComboParams      `json:"combo"`
	Attack     AttackParams     `json:"attack"`
	Stack      StackParams      `json:"stack"`
	Difficulty DifficultyParams `json:"difficulty"`
	Odai       OdaiParams       `json:"odai"`
	Matching   MatchingParams   `json:"matching"`
	Session    SessionParams    `json:"session"`
	Bot        BotParams        `json:"bot"`
}

// ComboParams: コンボの蓄積・減衰・個人難易度連動。
type ComboParams struct {
	NoMissBaseGain             int `json:"noMissBaseGain"`
	NoMissPerCharGain          int `json:"noMissPerCharGain"`
	MissDecay                  int `json:"missDecay"`
	PersonalDifficultyStep     int `json:"personalDifficultyStep"`
	PersonalDifficultyMaxLevel int `json:"personalDifficultyMaxLevel"`
}

// AttackParams: ダケンクリア攻撃の威力（#77）。
type AttackParams struct {
	ComboToPowerRatio       float64 `json:"comboToPowerRatio"`
	PowerToDakenRate        float64 `json:"powerToDakenRate"`
	BadgePowerBonusPerBadge float64 `json:"badgePowerBonusPerBadge"`
	BadgePowerBonusCap      float64 `json:"badgePowerBonusCap"`
	WarningGraceMs          int     `json:"warningGraceMs"`
	MinComboToAttack        int     `json:"minComboToAttack"` // このコンボ未満のクリアでは攻撃を出さない（#77）
	MaxReboundChain         int     `json:"maxReboundChain"`  // 予約（現状の撃ち返し連鎖なしでは未使用）
}

// StackParams: ダケンスタックとトラップ誘発。
type StackParams struct {
	Limit               int `json:"limit"`
	TrapTriggerInterval int `json:"trapTriggerInterval"`
	TrapMissPenalty     int `json:"trapMissPenalty"`
}

// DifficultyParams: 全体難易度。
type DifficultyParams struct {
	GlobalIntervalMs int `json:"globalIntervalMs"`
	MaxLevel         int `json:"maxLevel"`
}

// OdaiParams: ダケン個別制限時間と先読みストック（#81）。
type OdaiParams struct {
	BaseTimeLimitMs     int `json:"baseTimeLimitMs"`
	PerLevelReductionMs int `json:"perLevelReductionMs"`
	MinTimeLimitMs      int `json:"minTimeLimitMs"`
	LookaheadCount      int `json:"lookaheadCount"`     // 常に先読みで保つ通常ダケン数（NEXTストック・#81）
	DamageInsertOffset  int `json:"damageInsertOffset"` // 被弾ダケンをキューの何手先へ割り込ませるか（#81）
}

// MatchingParams: マッチング（試合前）。minPlayers は当日運用で下げられるよう可変性が重要。
type MatchingParams struct {
	MinPlayers       int `json:"minPlayers"`
	MaxPlayers       int `json:"maxPlayers"`
	StartCountdownMs int `json:"startCountdownMs"`
}

// SessionParams: 試合ループの調整値。tick 周期・状態配信間隔もハードコードせずここで持つ（決定4）。
type SessionParams struct {
	TickIntervalMs    int `json:"tickIntervalMs"`
	PublishIntervalMs int `json:"publishIntervalMs"` // 99人ミニ盤面の配信間隔（tickより低頻度で帯域を抑える）
}

// BotParams: 人数補完・デモ用の CPU（Bot）の強さ。合成ルート(main)が bot.Config へ写して各Botに渡す。
// 値は次の試合から反映（マッチ生成時に config を読み直すため）。game 本体は Bot を知らない。
type BotParams struct {
	ClearIntervalMs int     `json:"clearIntervalMs"` // 1お題を打ち切るのにかける間隔ms。上げるほど遅い＝弱い
	MissRate        float64 `json:"missRate"`        // 1お題ごとのミス確率(0..1)。上げるほどミスが増える＝弱い
}

// Validate は破綻値を弾く最小限の検証。config 取得（RemoteLoader / DB / config-front POST）で
// 共通に使う。コア game が GameParameters の不変条件を所有する（検証ロジックの単一ソース）。
func (gp GameParameters) Validate() error {
	if gp.Stack.Limit <= 0 {
		return fmt.Errorf("stack.limit は正である必要 (got %d)", gp.Stack.Limit)
	}
	if gp.Difficulty.MaxLevel <= 0 {
		return fmt.Errorf("difficulty.maxLevel は正である必要 (got %d)", gp.Difficulty.MaxLevel)
	}
	if gp.Bot.ClearIntervalMs <= 0 {
		return fmt.Errorf("bot.clearIntervalMs は正である必要 (got %d)", gp.Bot.ClearIntervalMs)
	}
	if gp.Bot.MissRate < 0 || gp.Bot.MissRate > 1 {
		return fmt.Errorf("bot.missRate は 0..1 である必要 (got %v)", gp.Bot.MissRate)
	}
	return nil
}

// DefaultParameters はリモートコンフィグ取得失敗時のフォールバック内蔵デフォルト。
// 値は 04_パラメータ仕様.md の初期仮値（すべて実測調整前のサンプル）。
func DefaultParameters() GameParameters {
	return GameParameters{
		Combo: ComboParams{
			NoMissBaseGain:             10,
			NoMissPerCharGain:          1,
			MissDecay:                  3,
			PersonalDifficultyStep:     20,
			PersonalDifficultyMaxLevel: 5,
		},
		Attack: AttackParams{
			ComboToPowerRatio:       1.0,
			PowerToDakenRate:        0.1,
			BadgePowerBonusPerBadge: 0.1,
			BadgePowerBonusCap:      1.0,
			WarningGraceMs:          1500,
			MinComboToAttack:        0, // 0=コンボがあれば毎クリア発火（#29で調整）
			MaxReboundChain:         3,
		},
		Stack: StackParams{
			Limit:               20,
			TrapTriggerInterval: 5,
			TrapMissPenalty:     3,
		},
		Difficulty: DifficultyParams{
			GlobalIntervalMs: 30000,
			MaxLevel:         10,
		},
		Odai: OdaiParams{
			BaseTimeLimitMs:     5000,
			PerLevelReductionMs: 300,
			MinTimeLimitMs:      2000,
			LookaheadCount:      5, // NEXT ストックを常時5件（#81・初期仮値4〜6）
			DamageInsertOffset:  3, // 被弾は3手先へ割り込み（現在＋次2件は不変）
		},
		Matching: MatchingParams{
			MinPlayers:       20,
			MaxPlayers:       99,
			StartCountdownMs: 15000,
		},
		Session: SessionParams{
			TickIntervalMs:    150,
			PublishIntervalMs: 250, // 約4Hz。KO等の即時イベントとは別に、盤面は低頻度スナップ
		},
		Bot: BotParams{
			ClearIntervalMs: 500,  // 従来のハードコード値（bot.DefaultConfig 相当）
			MissRate:        0.05, // 5%
		},
	}
}
