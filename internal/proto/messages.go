// Package proto は canonical な契約リポジトリ github.com/Okashimachi/Textro99-Proto を
// server 内から参照するための薄いラッパ（AGENTS §2）。
//
// 【なぜラッパか】
// 契約の単一ソースは Textro99-Proto（Go/TS/C# を対で保持、v0.1.0〜）。ここでは canonical の
// 型・定数を type alias / const で再輸出するだけにし、server 側の import パスは
// 従来どおり "textro99/internal/proto" に固定する。これにより:
//   - server の全パッケージ・.golangci.yml の depguard 設定を一切変えずに canonical へ移行できる
//   - canonical のモジュールパス/バージョン変更の影響をこの1ファイルに閉じ込められる
//   - 型の実体は canonical 1本になり、server 独自コピーとのドリフトが構造的に起きない
//
// 【追従ルール】canonical に新メッセージ/型が増えたら、この再輸出リストにも1行追加する
// （未追加の型を使うと "undefined" の明示的なコンパイルエラーになるので、黙って古い形にはならない）。
// 型の追加・変更・削除そのものは canonical 側で人間（りーせ）承認を得てから行う（AGENTS 1.2）。
//
// ローカルは go.work（リポジトリ親の go.work）で隣の Textro99-Proto を直参照して解決する。
package proto

import canon "github.com/Okashimachi/Textro99-Proto/proto"

// ── 共通型 ────────────────────────────────────────────────
type (
	PlayerId  = canon.PlayerId
	MatchId   = canon.MatchId
	DakenId   = canon.DakenId
	WarningId = canon.WarningId

	DakenType                  = canon.DakenType
	DakenInstance              = canon.DakenInstance
	PlayerSummary              = canon.PlayerSummary
	GameParametersPublicSubset = canon.GameParametersPublicSubset
	Envelope                   = canon.Envelope
)

const (
	DakenNormal    = canon.DakenNormal
	DakenEnemySent = canon.DakenEnemySent
	DakenTrap      = canon.DakenTrap
)

// ── メッセージ種別タグ（Envelope.Type） ──────────────────────
const (
	TypeDakenClearReport = canon.TypeDakenClearReport
	TypeStrategySelect   = canon.TypeStrategySelect
	TypeMatchmakingJoin  = canon.TypeMatchmakingJoin
	TypeMatchmakingLeave = canon.TypeMatchmakingLeave

	TypeWelcome           = canon.TypeWelcome
	TypeMatchStart        = canon.TypeMatchStart
	TypeDakenIssued       = canon.TypeDakenIssued
	TypeDakenExpired      = canon.TypeDakenExpired
	TypeComboUpdated      = canon.TypeComboUpdated
	TypeDifficultyUpdated = canon.TypeDifficultyUpdated
	TypeAttackIncoming    = canon.TypeAttackIncoming
	TypeDakenStackUpdated = canon.TypeDakenStackUpdated
	TypeKoNotified        = canon.TypeKoNotified
	TypePlayerListUpdated = canon.TypePlayerListUpdated
	TypePlayerListDelta   = canon.TypePlayerListDelta
	TypeGameOver          = canon.TypeGameOver
	TypeMatchmakingStatus = canon.TypeMatchmakingStatus
)

// ── C2S ───────────────────────────────────────────────────
type (
	DakenClearReport = canon.DakenClearReport
	StrategySelect   = canon.StrategySelect
	MatchmakingJoin  = canon.MatchmakingJoin
	MatchmakingLeave = canon.MatchmakingLeave
)

// ── S2C ───────────────────────────────────────────────────
type (
	Welcome           = canon.Welcome
	MatchStart        = canon.MatchStart
	DakenIssued       = canon.DakenIssued
	DakenExpired      = canon.DakenExpired
	ComboReason       = canon.ComboReason
	ComboUpdated      = canon.ComboUpdated
	DifficultyUpdated = canon.DifficultyUpdated
	AttackIncoming    = canon.AttackIncoming
	DakenStackUpdated = canon.DakenStackUpdated
	KoNotified        = canon.KoNotified
	PlayerListUpdated = canon.PlayerListUpdated
	PlayerDelta       = canon.PlayerDelta
	PlayerListDelta   = canon.PlayerListDelta
	TypingStats       = canon.TypingStats
	GameOver          = canon.GameOver
	WaitingPlayer     = canon.WaitingPlayer
	MatchmakingStatus = canon.MatchmakingStatus
)

const (
	ComboClear    = canon.ComboClear
	ComboMiss     = canon.ComboMiss
	ComboConsumed = canon.ComboConsumed
)
