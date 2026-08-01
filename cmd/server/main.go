// Command server は textro99 ゲームサーバーの合成ルート。全部品を配線し WebSocket で待ち受ける。
//
//	go run ./cmd/server --mode match   # マッチングプール起動（人数下限＋カウントダウン＋Bot補完）
//	go run ./cmd/server --mode solo    # 接続クライアント＋Botで即試合（ローカル確認用）
//
// 数値は GameParameters 経由（config）。作戦/お題/Bot は差し替え可能な部品を注入する。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"textro99/internal/app"
	"textro99/internal/bot"
	"textro99/internal/config"
	"textro99/internal/configapi"
	"textro99/internal/db"
	"textro99/internal/game"
	"textro99/internal/matchmaking"
	"textro99/internal/proto"
	"textro99/internal/transport"
)

func main() {
	mode := flag.String("mode", "match", "solo | match")
	addr := flag.String("addr", listenAddr(), "listen address（既定は $PORT があればそれ）")
	bots := flag.Int("bots", 3, "solo=補完Bot数 / match=Bot補完してこの人数まで埋める")
	configURL := flag.String("config-url", "", "GameParameters を JSON で返す HTTPエンドポイント（空ならデフォルト値で起動）")
	flag.Parse()

	ctx := context.Background()

	provider := chooseProvider(ctx, *configURL)

	// マッチング用パラメータ（minPlayers/countdown 等）は以前は起動時スナップショットだったが、
	// 現在は matchmaker 側で動的リロード（当日 minPlayers や countdown の変更）に対応している。
	_, err := provider.Load(ctx)
	if err != nil {
		log.Printf("config: 起動時取得失敗のためデフォルトで継続: %v", err)
	}
	baseDeps := app.DefaultDeps()

	// loadDeps はマッチ生成のたびに最新 config を読み、その試合用の Deps を作る。
	// ＝ config-front での編集が「次の試合から」再起動なしで反映される（進行中の試合は固定）。
	// provider.Load は失敗時も有効なデフォルトを返すので p は常に使用可能。
	loadDeps := func() app.Deps {
		d := baseDeps
		p, err := provider.Load(ctx)
		if err != nil {
			log.Printf("config: マッチ用取得失敗のためデフォルトで継続: %v", err)
		}
		d.Params = p
		return d
	}

	// botConfig は Bot 生成のたびに最新 config の Bot 強さを読む（config-front の編集が次の試合の
	// Bot から反映）。provider.Load は失敗時も有効なデフォルトを返す。game は Bot を知らないので
	// 合成ルートのここで game.BotParams → bot.Config へ写す。
	botConfig := func() bot.Config {
		p, _ := provider.Load(ctx)
		return bot.Config{
			ClearIntervalMs: p.Bot.ClearIntervalMs,
			MissRate:        p.Bot.MissRate,
		}
	}

	var ids atomic.Int64
	nextID := func() game.PlayerId { return game.PlayerId(idString(ids.Add(1))) }

	// /ws の許可オリジン。ブラウザは Origin を必ず送り、coder/websocket は既定で同一オリジン以外を
	// 拒否するため、フロント（localhost:5173 等）を ALLOWED_ORIGINS（カンマ区切り）で許可する。
	// 未設定なら全許可（結合をブロックしない。/ws は Cookie 認証等を持たないため実害小）。
	wsAccept := wsAcceptOptions(parseCSV(os.Getenv("ALLOWED_ORIGINS")))

	botsExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "bots" {
			botsExplicit = true
		}
	})

	switch *mode {
	case "solo":
		http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
			conn, err := transport.Accept(w, r, wsAccept)
			if err != nil {
				return
			}
			id := nextID()
			welcome(conn, id)
			name := awaitJoinName(conn, joinNameTimeout)
			players := []matchmaking.Player{{Id: id, Conn: conn, Name: name}}
			for i := 0; i < *bots; i++ {
				players = append(players, app.NewBotPlayer(ctx, nextID(), botConfig()))
			}
			log.Printf("solo: 試合開始 human=%s bots=%d", id, *bots)
			go app.RunMatch(ctx, loadDeps(), players)
		})

	default: // match
		mm := matchmaking.New(matchmaking.Config{
			GetParams: func() game.MatchingParams {
				p, _ := provider.Load(ctx)
				m := p.Matching
				if botsExplicit {
					m.MinFill = *bots
				}
				if m.MinFill == 0 {
					m.MinFill = game.DefaultParameters().Matching.MinFill
				}
				return m
			},
			Start: func(players []matchmaking.Player) {
				log.Printf("match: 試合開始 players=%d", len(players))
				go app.RunMatch(ctx, loadDeps(), players)
			},
			NewBot:  func() matchmaking.Player { return app.NewBotPlayer(ctx, nextID(), botConfig()) },
		})
		go mm.Run(ctx)
		http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
			conn, err := transport.Accept(w, r, wsAccept)
			if err != nil {
				return
			}
			id := nextID()
			welcome(conn, id)
			name := awaitJoinName(conn, joinNameTimeout)
			log.Printf("match: 参加 %s name=%q", id, name)
			mm.Join(matchmaking.Player{Id: id, Conn: conn, Name: name})
		})
	}

	// config 管理API（/api/params）。config-front(#51) が編集に使う。
	// 永続化(Postgres)がある時だけ意味を持つので、provider が db.ConfigStore の時に本物の Store を渡す。
	// DBが無ければ nil を渡し、ハンドラは 503 を返す（config-front に「DB未設定」を明示）。
	var cfgStore configapi.Store
	if cs, ok := provider.(*db.ConfigStore); ok {
		cfgStore = cs
	}
	// CONFIG_FRONT_ORIGIN はカンマ区切りで複数可（config-front を複数環境で使う場合）。空なら "*"。
	http.Handle("/api/params", configapi.NewHandler(cfgStore, os.Getenv("CONFIG_ADMIN_TOKEN"), parseCSV(os.Getenv("CONFIG_FRONT_ORIGIN"))))

	// ヘルスチェック（Render 等の稼働監視・疎通確認用）。
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("textro99 server: mode=%s addr=%s", *mode, *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// chooseProvider は環境に応じて ConfigProvider を選ぶ。
// 優先順位: DATABASE_URL(Postgres) > --config-url / CONFIG_URL(HTTP) > 内蔵デフォルト。
// DB接続・マイグレーション失敗でも必ず有効な ConfigProvider を返す（起動を止めない・04仕様7章）。
func chooseProvider(ctx context.Context, configURL string) game.ConfigProvider {
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		pool, err := db.NewPool(ctx, dsn)
		if err != nil {
			log.Printf("config: DB接続失敗のため設定は内蔵デフォルト: %v", err)
			return config.DefaultLoader{}
		}
		cs := db.NewConfigStore(pool)
		if err := cs.Migrate(ctx); err != nil {
			log.Printf("config: DBマイグレーション失敗のため設定は内蔵デフォルト: %v", err)
			return config.DefaultLoader{}
		}
		log.Printf("config: Postgres から取得（DATABASE_URL）")
		return cs
	}
	url := configURL
	if url == "" {
		url = os.Getenv("CONFIG_URL") // Render 等では env で渡すのが自然
	}
	if url != "" {
		log.Printf("config: HTTP から取得（%s）", url)
		return config.NewRemoteLoader(url)
	}
	log.Printf("config: 内蔵デフォルトで起動")
	return config.DefaultLoader{}
}

// parseCSV はカンマ区切り文字列を trim して非空要素のリストにする。
func parseCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// wsAcceptOptions は許可オリジン（フルURL or host[:port]）から /ws の受け入れ設定を作る。
// 空 or "*" を含む → 全許可（結合をブロックしない既定）。それ以外 → host パターンの許可リスト。
func wsAcceptOptions(origins []string) transport.AcceptOptions {
	if len(origins) == 0 {
		return transport.AcceptOptions{AllowAll: true}
	}
	hosts := make([]string, 0, len(origins))
	for _, o := range origins {
		if o == "*" {
			return transport.AcceptOptions{AllowAll: true}
		}
		hosts = append(hosts, originHost(o))
	}
	return transport.AcceptOptions{AllowedOriginHosts: hosts}
}

// originHost は "http://localhost:5173/" → "localhost:5173"。scheme 無し（既に host パターン）ならそのまま。
func originHost(o string) string {
	o = strings.TrimRight(strings.TrimSpace(o), "/")
	if u, err := url.Parse(o); err == nil && u.Host != "" {
		return u.Host
	}
	return o
}

// listenAddr は addr フラグの既定値。Render 等は $PORT を渡すのでそれを優先する。
func listenAddr() string {
	if p := os.Getenv("PORT"); p != "" {
		return ":" + p
	}
	return ":8080"
}

// joinNameTimeout は Welcome 後に MatchmakingJoin（表示名つき）を待つ上限。
// 期限内に来なければ空名で続行し、試合構築側が接続IDへフォールバックする（#79）。
const joinNameTimeout = 3 * time.Second

// awaitJoinName は接続の最初の MatchmakingJoin を読み、サニタイズ済み表示名を返す。
// タイムアウト・切断・別種メッセージ・不正JSON はいずれも空名を返す（フォールバック運用）。
// ここで消費するのは参加メッセージ1通のみで、以降のゲーム内メッセージは room が読む。
func awaitJoinName(conn transport.Connection, timeout time.Duration) string {
	select {
	case env, ok := <-conn.Receive():
		if !ok || env.Type != proto.TypeMatchmakingJoin {
			return ""
		}
		var m proto.MatchmakingJoin
		if json.Unmarshal(env.Payload, &m) != nil {
			return ""
		}
		return matchmaking.SanitizeDisplayName(m.DisplayName)
	case <-time.After(timeout):
		return ""
	}
}

func welcome(conn transport.Connection, id game.PlayerId) {
	data, err := json.Marshal(proto.Welcome{PlayerId: id})
	if err != nil {
		return
	}
	_ = conn.Send(proto.Envelope{Type: proto.TypeWelcome, Payload: data})
}

func idString(n int64) string {
	return "p-" + strconv.FormatInt(n, 10)
}
