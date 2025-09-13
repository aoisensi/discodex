# CONFIG

`discodex.toml` の設定項目。

## 例
```toml
[discord]
bot_token = "YOUR_BOT_TOKEN"
guild_id  = ""            # 任意。指定すると開発用にそのギルドにのみ登録
# log_channel_id = "..."   # 任意。詳細エラー等の出力先チャンネル

[[channels]]
channel_id = "123456789012345678"
# command = "codex mcp"         # MCP起動コマンド。空で既定
# workdir = "/home/aoi/work"    # 実行カレントディレクトリ
# env = { OPENAI_API_KEY = "..." }

[codex]
command = ""              # 空で既定（codex mcp）
timeout_seconds = 120      # 1リクエストの待ち時間
# debug = true             # 追加デバッグログ（env DISCODEX_DEBUG=1 でも可）
# idle_seconds = 600       # 一定時間チャットが無ければMCPを自動終了。0以下で無効。
# preamble = "..."         # 新規会話の先頭に付加する指示文
```

## 詳細
- `[discord]`
  - `bot_token`: Discord Bot Token（必須）
  - `guild_id`: 開発時に限定登録したいギルドID（任意）
- `[[channels]]`
  - `channel_id`: 紐付けるDiscordチャンネルID
  - `command`: チャンネル固有でCodex起動コマンドを上書き
  - `workdir`: Codexプロセスのカレントディレクトリ
  - `env`: Codex実行時に追加する環境変数
- `[codex]`
  - `command`: 既定は `codex mcp`
  - `timeout_seconds`: MCPリクエストのタイムアウト
  - `debug`: 追加デバッグログ（`DISCODEX_DEBUG=1` と同等）
  - `idle_seconds`: 最終アクティビティからのアイドル秒数。経過するとMCPを終了
  - `preamble`: 新規会話の最初に付ける指示

## 環境変数
- `DISCODEX_CONFIG`: TOMLのパス（未設定なら `discodex.toml`）
- `DISCODEX_DEBUG`: 追加デバッグログ（`1`, `true` 等で有効）
