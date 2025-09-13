package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Discord Discord `toml:"discord"`
	// チャンネルごとの実行設定
	Channels []Channel `toml:"channels"`
	Codex    Codex     `toml:"codex"`
}

type Discord struct {
	BotToken string `toml:"bot_token"`
	GuildID  string `toml:"guild_id"`
	// 詳細エラーログなどを投稿するチャンネル（任意）
	LogChannelID string `toml:"log_channel_id"`
}

type Channel struct {
	ChannelID string `toml:"channel_id"`
	// このチャンネルで実行するコマンドを上書き（未指定なら [codex].command、さらに未指定なら内蔵デフォルト）
	Command string `toml:"command,omitempty"`
	// 作業ディレクトリ（ローカル）。指定すると "cd <dir> && <command>" で実行
	Workdir string `toml:"workdir,omitempty"`
	// 実行時に設定する環境変数。例: env = { OPENAI_API_KEY = "..." }
	Env map[string]string `toml:"env,omitempty"`
}

type Codex struct {
	// インタラクティブ起動に使うコマンド（空なら既定: codex -a never --sandbox workspace-write --color never）
	Command string `toml:"command"`
	// interactiveモード用: セッションJSONLのルートディレクトリを上書き（未指定なら $HOME/.codex/sessions）
	SessionRoot string `toml:"session_root"`
	// 1リクエストのタイムアウト（秒）
	TimeoutSeconds int `toml:"timeout_seconds"`
	// 追加デバッグログ（環境変数 DISCODEX_DEBUG=1 でも有効）
	Debug bool `toml:"debug"`
	// アイドルでMCPを自動終了するまでの秒数（0以下で無効）
	IdleSeconds int `toml:"idle_seconds"`
	// 新規会話の先頭に付加する指示文（任意）
	Preamble string `toml:"preamble"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if _, err := toml.Decode(string(data), &c); err != nil {
		return nil, err
	}
	// 簡易バリデーション
	for i, ch := range c.Channels {
		if ch.ChannelID == "" {
			return nil, fmt.Errorf("channels[%d].channel_id is empty", i)
		}
	}
	return &c, nil
}

func LoadDefault() (*Config, error) {
	path := os.Getenv("DISCODEX_CONFIG")
	if path == "" {
		path = "discodex.toml"
	}
	c, err := Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return c, err
}
