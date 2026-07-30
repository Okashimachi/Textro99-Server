package config

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"textro99/internal/game"
)

// RemoteLoader はリモートの設定エンドポイント（HTTP で GameParameters の JSON を返すもの）から取得する。
// 取得元の方式は問わない：config-front(Webアプリ)/GAS/静的ファイル等、HTTP で JSON を返せばよい。
type RemoteLoader struct {
	// URL は GameParameters を JSON で返すエンドポイント。
	URL string
	// Client はHTTPクライアント。未指定なら DefaultHTTPClient。テストで差し替え可能にする。
	Client *http.Client
}

// DefaultHTTPClient はタイムアウト付きの既定クライアント。
var DefaultHTTPClient = &http.Client{Timeout: 5 * time.Second}

// NewRemoteLoader は URL を指定して RemoteLoader を作る。
func NewRemoteLoader(url string) *RemoteLoader {
	return &RemoteLoader{URL: url, Client: DefaultHTTPClient}
}

// Load はリモートから GameParameters を取得する。
// 取得・パース・検証のいずれかに失敗しても内蔵デフォルト＋理由err を返す（起動を止めない・04仕様7章）。
// ＝ 第1返り値は常に有効な GameParameters。
func (l *RemoteLoader) Load(ctx context.Context) (game.GameParameters, error) {
	def := game.DefaultParameters()

	client := l.Client
	if client == nil {
		client = DefaultHTTPClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.URL, nil)
	if err != nil {
		return def, fmt.Errorf("config: リクエスト生成: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return def, fmt.Errorf("config: GET %s: %w", l.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return def, fmt.Errorf("config: 予期しないステータス %d (%s)", resp.StatusCode, l.URL)
	}

	var gp game.GameParameters
	if err := json.NewDecoder(resp.Body).Decode(&gp); err != nil {
		return def, fmt.Errorf("config: JSONデコード: %w", err)
	}
	// セクション追加前に保存された応答の後方互換（追加分はゼロ値→既定値で補う）。
	gp = gp.BackfillLegacyDefaults()
	if err := gp.Validate(); err != nil {
		return def, fmt.Errorf("config: 値の検証: %w", err)
	}
	return gp, nil
}

var _ game.ConfigProvider = (*RemoteLoader)(nil)
