# クライアント結合ガイド（Web / Unity 向け）

テキストロ99 サーバーへ**クライアント（Web=TS / Unity=C#）をつなぐための参考資料**。
プロトコルの正典は [Textro99-Proto `proto/messages.go`](https://github.com/Okashimachi/Textro99-Proto) と
`Textro99-Docs/02_共通仕様/01_プロトコル仕様.md`。本書はそれを結合作業目線でまとめたもの。

> このドキュメントの JSON サンプルは、実サーバーに生 WebSocket で接続して取得した実物
> （`internal/app/e2e_wire_test.go` で自動検証）。乖離したらそのテストが落ちる。

---

## ⚠️ 2026-07-30 更新（#77/#79/#80/#81。以下が正典。矛盾する下記記述は本節優先）

DemoStage(8/2) 向けの戦闘刷新で契約が変わった（proto `textro-main` v0.1.1）:

- **#77 Enter攻撃・相殺・撃ち返しは廃止**。攻撃は**ダケンをノーミスクリアした瞬間サーバーが自動発火**する（クライアントは何も送らない）。コンボは消費されず伸び続け、**ミス／時間切れで0にリセット**。
  → C2S から **`AttackRequest` 撤去**。S2C から **`OffsetResolved` / `AttackFailed` 撤去**。予告 `AttackIncoming`→grace→着弾は従来どおり。
- **#79 表示名**: 接続後に **`MatchmakingJoin{ "displayName": "..." }`** を1通送る。未送信/空はサーバーが接続IDでフォールバック。
- **#80 順位**: `PlayerSummary` に **`rank`**（試合中の確定順位。1=首位、0=未確定）を追加。クライアントは自前推定せずこれを表示。
- **#81 先読み/割り込み**: `DakenIssued` に **`insertIndex`**（お題キューへの挿入位置。省略=末尾、被弾は途中に割り込む）を追加。開始時に複数の通常お題が届く（NEXT ストック）。

以降の本文で `AttackRequest` / `OffsetResolved` / `AttackFailed` / 相殺・撃ち返し・Enter攻撃、および「`MatchmakingJoin` は未処理」と書いてある箇所は**すべて上記で置き換わっている**。

---

## 0. 大原則（これだけは外さない）

- **サーバーが戦闘の唯一の権威**。コンボ・威力・被弾・KO・脱落・難易度・**順位**は**全部サーバーが確定**する。
- **クライアントの責務は2つだけ**: ①打鍵のローカル判定（合ってる/ミス）②表示。
- したがってクライアントが送るのは実質 **`DakenClearReport` / `StrategySelect` / `MatchmakingJoin`(表示名)** だけ。
  攻撃はサーバーがクリア起点に自動発火するので **`AttackRequest` は送らない（廃止）**。
  **時間切れ報告・脱落報告も送らない**（サーバーが自律検知して `DakenExpired` / `KoNotified` を配る）。
- 全メッセージは **`{"type": "...", "payload": {...}}` の封筒**。JSON、フィールドは **camelCase**。WS フレームは **text**。

---

## 1. 接続

| 項目 | 値 |
|---|---|
| エンドポイント | `wss://textro99-server.onrender.com/ws` |
| ヘルスチェック | `GET https://textro99-server.onrender.com/healthz` → `200 ok` |
| サブプロトコル | なし（素の WebSocket） |
| フレーム | text（UTF-8 JSON） |

### 起動モード（重要）
サーバーには2モードあり、**接続後の挙動が変わる**:

- **solo**（結合テスト用, #56）: `/ws` に**1接続しただけで即試合開始**（人間1＋Bot）。単独で結合テストできる。
- **match**（本番, #57）: 接続すると待機プールに入り、`MatchmakingStatus` が届く。
  規定人数＋カウントダウン成立で `MatchStart`。**単独接続では試合が始まらない**（Bot だけでは埋まらない）。

> **今どちらで動いているかは接続して確認**（§8）：`Welcome` の直後に `MatchmakingStatus` が来たら match、いきなり `MatchStart` なら solo。
> 結合開発は solo で回すのが楽（server 窓口＝りーせに solo 化を依頼できる）。本番は match なので、クライアントは**両方のシーケンスに対応**しておくこと（§3）。

---

## 2. メッセージ一覧

### 2.1 クライアント→サーバー（C2S）— 送るのはこれだけ

| type | いつ送る | payload |
|---|---|---|
| `DakenClearReport` | 1ダケンの打鍵を**完了した瞬間**（ローカル判定） | `{ "dakenId": "...", "isMiss": false, "missCount": 0, "elapsedMs": 300 }` |
| `StrategySelect` | 作戦選択（0〜9キー / 試合前初期選択） | `{ "strategyId": 4 }` |
| `MatchmakingJoin` | 接続後に表示名を送る（#79） | `{ "displayName": "りーせ" }` |
| `MatchmakingLeave` | （予約）キュー離脱 | `{}` |

補足:
- `DakenClearReport.missCount` が**正典**（`isMiss` は `missCount>0` の冗長フラグ）。ミスしても**そのダケンは完了扱い**で報告する（ミス回数を添えて送る）。**ミスするとコンボは0にリセット**され、そのクリアでは攻撃が出ない（#77）。
- 🚫 **`AttackRequest` は廃止（#77）**。Enter 攻撃はもう無い。攻撃は**ノーミスクリアのたびサーバーが自動発火**する。クライアントは打鍵報告だけ送ればよい。
- `StrategySelect.strategyId` は 0〜9。**未選択の既定は 4（ランダム）**。作戦IDの割り当ては §5。
- `MatchmakingJoin.displayName`（#79）は接続後に1通送る。最大24ルーン・制御文字は除去・空/未送信は接続IDでフォールバック。Bot は `"BOT p-x"`。
- ⚠️ dakenId は**サーバー発行の実在IDのみ**報告可（チート検証と衝突する。`MatchStart.initialDaken` と `DakenIssued` で受け取ったIDを使う）。

### 2.2 サーバー→クライアント（S2C）

| type | 意味 | 配信先 |
|---|---|---|
| `Welcome` | 接続直後、自分の `playerId` 通知 | 本人 |
| `MatchStart` | 試合開始・初期状態 | 全員 |
| `DakenIssued` | 次ダケン提示（1件）/ 被弾・トラップ（複数件） | 本人 |
| `DakenExpired` | サーバーが時間切れ検知 | 本人 |
| `ComboUpdated` | コンボ変化（加算/減衰/消費） | 本人 |
| `DifficultyUpdated` | 難易度変化（全体/個人/実効） | 本人 |
| `AttackIncoming` | 被弾予告（クリア起点攻撃の対象に確定・#77） | 被弾者 |
| `DakenStackUpdated` | スタック増減 | 本人 |
| `KoNotified` | 脱落確定 | 全員 |
| `PlayerListUpdated` | 全員ぶんのフルスナップ（低頻度） | 全員 |
| `PlayerListDelta` | 変化分のみ（帯域対策） | 全員 |
| `GameOver` | 自分の脱落 or 優勝（`rank==1` で優勝） | 本人 |
| `MatchmakingStatus` | 待機人数/カウントダウン | 待機者 |

---

## 3. 標準シーケンス

### solo（今の結合テスト環境）
```
C → connect /ws
S → Welcome            {"playerId":"p-1"}
C → MatchmakingJoin    {"displayName":"りーせ"}   ← 接続後に表示名(#79)
S → MatchStart         {... initialDaken ... 先読み複数(#81) ...}
   （ループ）
S → DakenIssued / ComboUpdated / DakenStackUpdated / AttackIncoming / KoNotified / PlayerListUpdated ...
C → DakenClearReport   （打鍵完了ごと。ノーミスクリアでサーバーが自動攻撃・#77）
   （…）
S → GameOver           {"rank":1, ...}
```

### match（本番）
```
C → connect /ws
S → Welcome
S → MatchmakingStatus  {"waitingCount":1,"minPlayers":20}    ← 人が集まるまで繰り返し
S → MatchmakingStatus  {"waitingCount":20,"minPlayers":20,"countdownMs":15000}  ← 成立→カウントダウン
S → MatchStart
   （以降 solo と同じ）
```
> `Welcome` は `MatchStart` より前に必ず来る。待機中から `StrategySelect` を送れるよう自分IDを先に確定させるため。

---

## 4. メッセージ別・実サンプルとフィールド

以下 payload は**実接続で取得した実物**（`(実測)`）または契約からの構成（`(契約)`）。
**着目するのはフィールド名と型（形）**。数値そのものは検証用テスト試合の値で、`stackLimit` 等の実値は
config 由来で変わる（既定の `stackLimit` は 20、サンプルはテストで 4 に絞ったもの）。

### Welcome `(実測)`
```json
{"playerId":"p-1"}
```

### MatchStart `(実測)`
```json
{
  "matchId":"m-1",
  "selfPlayerId":"p-1",
  "players":[
    {"playerId":"p-1","displayName":"p-1","comboValue":0,"dakenStackCount":0,"dakenStackLimit":4,"badgeCount":0,"alive":true}
  ],
  "initialDaken":{"dakenId":"p-1-1","type":"Normal","text":"そら","difficultyLevel":0,"timeLimitMs":5000,"issuedAtServerTimeMs":0},
  "parameters":{"stackLimit":4,"trapTriggerInterval":5,"personalDifficultyStep":20,"difficultyMaxLevel":10}
}
```
- `players[]` は `PlayerSummary`（99人ミニ盤面の初期値）。`selfPlayerId` で自分を特定。
- `initialDaken` は最初の1問。以降は `DakenIssued` で届く。
- `parameters` は**公開サブセットのみ**（表示に必要な値だけ。威力係数等の内部通貨は非公開）。

### DakenIssued `(実測)`
```json
{"daken":[{"dakenId":"p-1-2","type":"Normal","text":"ねこ","difficultyLevel":0,"timeLimitMs":5000,"issuedAtServerTimeMs":0}]}
```
- 通常は1件。**被弾・トラップ時は複数件**まとめて届く（配列）。
- `type`: `Normal`（自然出題）/ `EnemySent`（被弾）/ `Trap`（煽り長文ペナルティ）。
- **打鍵対象は `text`**。コンボ加算は打鍵数（ローマ字キーストローク数）依存でサーバーが計算する。

### ComboUpdated `(実測)`
```json
{"comboValue":14,"delta":14,"reason":"Clear"}
```
- `reason`: `Clear`（ノーミス加算）/ `Miss`（減衰）/ `Consumed`（攻撃で消費）。

### DifficultyUpdated `(実測)`
```json
{"globalLevel":0,"personalLevel":1,"effectiveLevel":1}
```
- `effectiveLevel = min(globalLevel + personalLevel, difficultyMaxLevel)`。出題難易度はこれで決まる。

### KoNotified `(実測)`
```json
{"attackerId":"p-1","victimId":"p-2","badgesTransferred":0}
```
- ⚠️ **`attackerId` は null になり得る**（自滅＝時間切れ積み残しやトラップミスのみで上限到達）。
  その場合バッジは移動せず消滅（`badgesTransferred":0`）。**null を必ずハンドルすること**。

### PlayerListUpdated `(実測)`
```json
{"players":[{"playerId":"p-1","displayName":"p-1","comboValue":15,"dakenStackCount":0,"dakenStackLimit":4,"badgeCount":0,"alive":true}],"aliveCount":4}
```
- フルスナップ（低頻度・約4Hz既定）。99人だと重いので、高頻度更新は `PlayerListDelta` を使う。

### GameOver `(実測)`
```json
{"rank":1,"koCount":3,"finalBadgeCount":0,"typingStats":{"totalDakenCleared":1196,"totalMiss":0,"maxCombo":46,"elapsedMs":75}}
```
- `rank==1` で優勝、それ以外は脱落。`typingStats` はリザルト表示用。

### DakenExpired `(契約)`
```json
{"dakenId":"p-1-7"}
```
サーバーが時間切れを検知したら届く（クライアントは報告不要）。該当ダケンの表示を消す。

### AttackIncoming `(契約)`
```json
{"warningId":"w-1","attackerId":"p-3","power":24,"graceMs":1500}
```
被弾予告。`graceMs` の猶予内に相殺（攻撃で撃ち返し）できる。`power` は内部通貨（着弾個数への変換は着弾時）。

### AttackFailed `(契約)`
```json
{"reason":"NoTarget"}
```
自分の `AttackRequest` が不成立だった時のみ本人へ。`reason`: `NoTarget`（生存者が自分だけ等）/ `NoCombo`（コンボ0）。**コンボは消費されない**。

### OffsetResolved `(契約)`
```json
{"warningId":"w-1","offsetAmount":10,"remainderDakenCount":2,"counterAttackWarningId":"w-2"}
```
相殺の確定。余剰の撃ち返しが成立した時のみ `counterAttackWarningId` が入る（**不成立なら余剰は消失し、このフィールドは省略/ null**）。

### DakenStackUpdated `(契約)`
```json
{"count":3,"limit":4,"trapPending":true}
```
未処理ダケンのスタック。`count==limit` で脱落方向。`trapPending` はトラップ誘発待ち。

### PlayerListDelta `(契約)`
```json
{"changed":[{"playerId":"p-2","stackRatio":2,"badgeCount":1,"alive":false}],"aliveCount":3}
```
変化プレイヤーのみ。**null（省略）のフィールドは変化なし**。`displayName` は `MatchStart` で配布済みなので差分に含まれない。

### MatchmakingStatus `(実測: match時)`
```json
{"waitingCount":1,"minPlayers":20}
```
カウントダウン中のみ `countdownMs` が入る（待機中は省略）：`{"waitingCount":20,"minPlayers":20,"countdownMs":15000}`。

---

## 5. 作戦ID（StrategySelect.strategyId）

| id | 名称 | 対象の選び方（サーバー実装準拠） | 対象なし時 |
|---|---|---|---|
| 0 | SplitAttack（全体割り） | 自分以外の**生存者全員**へ威力を均等分配 | 不発 |
| 1 | Counter（カウンター） | 自分に予告中の相手のうち**最新の予告主**（生存） | 4へ |
| 2 | Finisher（とどめ） | スタック比率 `count/limit` が**最大**＝脱落に近い相手 | 不発 |
| 3 | BadgeHunter（バッジ狙い） | `badgeCount` が**最大**の相手 | 不発 |
| 4 | Random（ランダム・**既定**） | 生存者から一様ランダムに1名 | 不発 |
| 5 | Revenge（リベンジ） | **直近で自分に着弾させた相手**（生存時） | 4へ |
| 6 | TallPoppy（出る杭） | `comboValue` が**最大**＝大技を溜めてる相手 | 不発 |
| 7 | Neighbor（隣狙い） | PlayerId 昇順で**自分の次**の相手（末尾→先頭ラップ） | 不発 |
| 8 | PileOn（巻き添え） | 今**いちばん狙われている**相手（予告受信数が最多） | 4へ |
| 9 | PacifistHunter（平和主義狩り） | **誰からも狙われていない**相手からランダム | 4へ |

> 同値は基本ランダムでタイブレーク。「4へ」＝対象不成立時に **4:ランダムへフォールバック**。
> 具体ロジックはサーバー内部で、クライアントは id を送るだけ。

---

## 6. 最小クライアント実装（擬似コード）

```ts
const ws = new WebSocket("wss://textro99-server.onrender.com/ws");
let selfId = "";
const pending: string[] = [];        // 保持中の dakenId（表示・打鍵対象）

ws.onmessage = (ev) => {
  const { type, payload } = JSON.parse(ev.data);
  switch (type) {
    case "Welcome":     selfId = payload.playerId; break;
    case "MatchStart":  pending.push(payload.initialDaken.dakenId); render(payload); break;
    case "DakenIssued": for (const d of payload.daken) pending.push(d.dakenId); break;
    case "DakenExpired":remove(pending, payload.dakenId); break;
    case "ComboUpdated":       /* コンボ表示更新 */ break;
    case "DakenStackUpdated":  /* スタックゲージ更新 */ break;
    case "AttackIncoming":     /* 被弾予告演出（graceMs 猶予） */ break;
    case "KoNotified":         /* payload.attackerId は null あり！ */ break;
    case "PlayerListUpdated":
    case "PlayerListDelta":    /* 99人ミニ盤面更新 */ break;
    case "GameOver":           showResult(payload); break;
  }
};

// 打鍵完了（ローカル判定）→ 報告
function onDakenTyped(dakenId: string, missCount: number, elapsedMs: number) {
  send("DakenClearReport", { dakenId, isMiss: missCount > 0, missCount, elapsedMs });
}
// Enter → 攻撃
function onEnter() { send("AttackRequest", { consumedCombo: 0 }); }
// 0-9 → 作戦
function onStrategyKey(id: number) { send("StrategySelect", { strategyId: id }); }

function send(type: string, payload: object) {
  ws.send(JSON.stringify({ type, payload }));
}
```

---

## 7. つまずきどころチェックリスト

- [ ] 封筒は `{type, payload}`。payload を**入れ子の文字列**にしない（オブジェクトのまま）。
- [ ] `KoNotified.attackerId` / `OffsetResolved.counterAttackWarningId` は **null（省略）あり**。
- [ ] 時間切れ・脱落は**送らない**（サーバー権威）。送るのは 3種のみ。
- [ ] `DakenClearReport` の dakenId は**サーバー発行の実在ID**のみ。
- [ ] match モードでは `MatchStart` 前に `MatchmakingStatus` が来る。solo では即 `MatchStart`。
- [ ] `elapsedMs` は表示・統計用。整合の権威はサーバー（発行→受信の実時間）。
- [ ] フィールド名は **camelCase**。数値は数値型（文字列にしない）。
- [ ] **ブラウザから `/ws` に繋がらない（connecting→reconnecting を繰り返す）**時は、サーバー側の**許可オリジン設定**を確認。サーバーは WS upgrade 時に `Origin` を検証しており、許可外のオリジン（例 `http://localhost:5173`）は拒否される（websocat/Node は `Origin` を送らないので通る＝クライアント起因ではない）。サーバーの `ALLOWED_ORIGINS`（カンマ区切り）に自分のオリジンを追加してもらう。**未設定時は全許可**なので、通常はデプロイ済みなら繋がる。クライアント側で `Origin` は変えられない。
- [ ] ID類（`playerId` / `matchId` / `dakenId` / `warningId`）は**サーバー割当ての不透明文字列**。**パースや自前生成をしない**（`dakenId` の "p-1-1" 等の形は実装詳細で、依存しない）。§4 の `(契約)` サンプル中のID値は説明用の例。

---

## 8. 動作確認の手っ取り早い方法

```bash
# ヘルスチェック
curl https://textro99-server.onrender.com/healthz    # => ok

# WS 疎通（websocat 等）: solo なら接続だけで Welcome→MatchStart が流れてくる
websocat wss://textro99-server.onrender.com/ws
```

疑問・不整合があれば server 側の窓口（りーせ）へ。契約変更は Proto リポジトリで人間承認が要る（勝手に形を変えない）。
