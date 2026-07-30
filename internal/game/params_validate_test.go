package game

import "testing"

func TestGameParameters_Validate(t *testing.T) {
	t.Run("デフォルトは妥当", func(t *testing.T) {
		if err := DefaultParameters().Validate(); err != nil {
			t.Fatalf("DefaultParameters は妥当なはず: %v", err)
		}
	})

	t.Run("stack.limit<=0 を弾く", func(t *testing.T) {
		gp := DefaultParameters()
		gp.Stack.Limit = 0
		if err := gp.Validate(); err == nil {
			t.Fatal("stack.limit=0 はエラーになるべき")
		}
	})

	t.Run("difficulty.maxLevel<=0 を弾く", func(t *testing.T) {
		gp := DefaultParameters()
		gp.Difficulty.MaxLevel = -1
		if err := gp.Validate(); err == nil {
			t.Fatal("difficulty.maxLevel=-1 はエラーになるべき")
		}
	})

	t.Run("bot.clearIntervalMs<=0 を弾く", func(t *testing.T) {
		gp := DefaultParameters()
		gp.Bot.ClearIntervalMs = 0
		if err := gp.Validate(); err == nil {
			t.Fatal("bot.clearIntervalMs=0 はエラーになるべき")
		}
	})

	t.Run("bot.missRate が範囲外を弾く", func(t *testing.T) {
		gp := DefaultParameters()
		gp.Bot.MissRate = 1.5
		if err := gp.Validate(); err == nil {
			t.Fatal("bot.missRate=1.5 はエラーになるべき")
		}
	})
}

// TestBackfillLegacyDefaults は bot 追加前に保存された設定（JSON に bot キーが無くゼロ値になる）が
// 補完されて Validate を通ることを確認する回帰テスト（本番incident: db#Load が500を返した原因）。
func TestBackfillLegacyDefaults(t *testing.T) {
	t.Run("Bot が丸ごとゼロ値なら既定値で補完される", func(t *testing.T) {
		gp := DefaultParameters()
		gp.Bot = BotParams{} // bot 追加前の保存データを模す（Unmarshal時に発生する状態）
		got := gp.BackfillLegacyDefaults()
		if got.Bot != DefaultParameters().Bot {
			t.Fatalf("Bot が既定値で補完されていない: %+v", got.Bot)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("補完後は Validate を通るはず: %v", err)
		}
	})

	t.Run("有効な Bot 設定は上書きされない", func(t *testing.T) {
		gp := DefaultParameters()
		gp.Bot = BotParams{ClearIntervalMs: 900, MissRate: 0.2} // 保存済みの正当な調整値
		got := gp.BackfillLegacyDefaults()
		if got.Bot != gp.Bot {
			t.Fatalf("有効な Bot 設定が上書きされた: got=%+v want=%+v", got.Bot, gp.Bot)
		}
	})
}
