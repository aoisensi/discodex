# ARCHITECTURE

## フロー概要
- Discord → Bot: メッセージ受信（チャンネル紐付け時はメンション不要）
- Bot → MCPBridge: `ChatMulti` 呼び出し
- MCPBridge → Codex: `codex mcp` を子プロセス起動し、STDIOでJSON‑RPC
- MCPBridge ← Codex: `tools/call` の `result` と `codex/event` を受信
- MCPBridge → Bot: コールバックでストリーム/推論更新を通知
- Bot → Discord: メッセージ投稿/編集、Presence更新

## 主要コンポーネント
- `internal/discordbot`
  - Discordセッション管理
  - メッセ受信、送信（ストリーム編集含む）、Presence操作
- `internal/codex`
  - `MCPBridge`: Codex MCP子プロセス管理、JSON‑RPC、イベント処理
  - レスポンス解釈（`agent_message(_delta)`, `agent_reasoning(_delta)`, `token_count`, etc.）
- `internal/config`
  - TOMLロード、簡易バリデーション

## イベントと対応
- `agent_message_delta`
  - onAgentDelta → Discordメッセ編集に追記（250msスロットル）
- `agent_message`
  - onAgentDone → 最終文で確定
- `agent_reasoning_delta` / `agent_reasoning`
  - WithReasoningHandler → Presenceに短文表示
- `task_started` / `task_complete`
  - タイミング情報。`task_complete` で推論表示をクリア
- `token_count`
  - コスト可視化に利用可能（未表示）

## プロセス管理
- 子プロセス: `codex mcp`
- Unix: 新しいプロセスグループで起動 → 終了時に pgkill→kill
- 初期化: `initialize` は700ms待ち（応答遅延に耐性）→ `initialized` 通知

## ストリーミング設計
- `requestId` と DiscordチャンネルIDを関連付け
- 1リクエストにつきDiscordの1メッセージを作り、deltaで編集
- 完了イベントで確定・クリーンアップ

