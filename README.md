# discodex

Codex CLI を常駐起動し、Discord と橋渡しするボット。

## 概要
- Discordメッセージを Codex の MCP（STDIO/JSON‑RPC）へ流し、応答をDiscordへ返す。
- 紐付けチャンネルではメンション不要。未紐付けはメンションで単発実行。
- 応答はストリーミング（deltaをメッセ編集で逐次反映、確定で固定）。
- 推論中テキスト（agent_reasoning）を Discord のプレゼンスに反映。
- 終了時は各チャンネルへ終了通知し、Presence をオフライン化。

## できること
- ストリーミング返信（`agent_message_delta` → 編集、`agent_message` → 確定）
- プレゼンス更新（`agent_reasoning(_delta)`）
- 会話継続（`conversationId` を保持）
- デバッグログ（`DISCODEX_DEBUG=1` または TOML の `debug=true`）
- リセット（本文に `/reset` と送ると会話をクリア）

## 動かし方
1) 前提
- Codex CLI がローカルで動く（`codex mcp` が起動できる）
- Discord Bot Token を用意し、Message Content Intent を有効化

2) 設定
- `discodex.example.toml` を `discodex.toml` にコピーして編集
- `bot_token` と `channels[].channel_id` を設定

3) 実行
```bash
go run ./cmd/discodex
# デバッグ有効:
DISCODEX_DEBUG=1 go run ./cmd/discodex
```

## 設定（TOML）
- 既定ファイル: `discodex.toml`（`DISCODEX_CONFIG` で別パス指定可）
- 例と全項目は CONFIG.md を参照

## 仕組み（MCP）
- 起動: `codex mcp` を子プロセスとして起動（プロセスグループで管理）
- 初期化: `initialize` → `initialized`
- 会話: `tools/call`（`codex`/`codex-reply`）を送信
- イベント: `codex/event` を受信して delta/推論/完了などを処理
  - Discordユーザー名を `user` 引数としてMCPに渡す（ニックネーム/グローバル名優先）

詳細は ARCHITECTURE.md を参照。

## トラブルシュート
- 応答がない: Message Content Intent を有効に。`DISCODEX_DEBUG=1` で `mcp =>/<=` を確認
- 終了しない: SIG 後も残る場合は issue へ。内部は pgkill→kill を実装済み
- MCP応答形式が違う: ログとレスポンス例を添付して issue へ

## 開発
- Go 1.22+
- ビルド: `go build ./...`
- 主要パッケージ: `internal/discordbot`, `internal/codex`, `internal/config`
