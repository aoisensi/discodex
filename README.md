# discodex

## 概要
discodex は、ローカルの Codex インタラクティブCLIを常駐起動し、Discord とやり取りを橋渡しする Bot。入力は Codex プロセスの標準入力に流し、出力はユーザーディレクトリ配下の `.codex/sessions/*.jsonl` を tail して取得して返信する。

### 基本動作
- 紐付けたチャンネルではメンション不要で通常メッセージをそのまま Codex に渡す。
- 紐付けがないチャンネルでは、Bot メンションを付けると一度だけ実行して返信する。

## 目的（暫定）
- Go を用いた実運用可能なサービス／ツールの作成
- 小さく始めて継続的に改善できる設計・運用体制の確立

## ステータス
- プロジェクト初期化前（この README の作成のみ）

## 想定スタック（暫定）
- 言語: Go 1.22+
- 依存管理: `go mod`
- テスト: `go test`

## 次の一歩
- 初期要件の確定（目的・主要ユースケース・最初の機能）
- リポジトリ初期化（`go mod init`、ベースディレクトリ構成）
- CI の雛形追加（テストと lint）

## 実行手順（開発）
- 前提: Codex CLI がローカル環境で実行できること（`codex exec` が動く）。
- `discodex.toml` を用意（`discodex.example.toml` をコピーして編集）。

### ビルドと実行
```powershell
go build -o .\\bin\\discodex .\\cmd\\discodex
.\\bin\\discodex.exe   # 環境変数は不要、TOMLのみで起動
```

### 使い方（初期実装）
- チャット会話:
  - 紐付けされたチャンネルでは、メンション不要で通常メッセージが Codex に渡る。
  - 紐付けがないチャンネルでは、Bot メンションを付けて送る。
  - 例: `@discodex このディレクトリのファイル一覧を見せて`

注: 本Botは Codex のMCPサーバーにSTDIO JSON‑RPCで接続し、`tools/call` の `codex` / `codex-reply` を叩いて会話を継続する。

## 設定（TOML）
- 既定ファイル名は `discodex.toml`（環境変数不要）。別パスを使う場合は `DISCODEX_CONFIG` を設定可。
- 例:

```toml
# Discord設定
[discord]
bot_token = "YOUR_BOT_TOKEN"
# guild_id を指定すると、そのギルドにのみコマンド登録（開発時に便利）
guild_id  = "123456789012345678" # 空ならグローバル登録

# チャンネルごとの実行設定
[[channels]]
channel_id = "123456789012345678"  # DiscordのチャンネルID
# command = "codex -a never --sandbox workspace-write --color never"  # チャンネル個別の起動コマンド
# workdir = "/home/aoi/work"        # カレントディレクトリ（任意）
# env = { OPENAI_API_KEY = "sk-..." } # 実行時に設定する環境変数

[[channels]]
channel_id = "987654321098765432"
distro     = "Debian"

# Codexブリッジ
[codex]
# MCP起動に使うコマンド（空なら既定: codex mcp）
command = ""
timeout_seconds = 120
```

- 紐付けされたチャンネルのメッセージは、指定ディストリのコンテキストとして処理される。
- `/wsl start` と `/wsl stop` は、呼び出しチャンネルが紐付け済みならそのディストリを対象に動作。未紐付けなら既定を対象。

### 仕組み（MCP）
- 起動時に `codex mcp` をSTDIOで起動
- JSON‑RPCで `initialize`→`initialized` を送信
- 会話開始: `tools/call { name: "codex", arguments: { prompt, sandbox, approval-policy, cwd? } }`
- 継続: `tools/call { name: "codex-reply", arguments: { conversationId, prompt } }`

---
追記してほしい具体的な目的やユースケースがあれば知らせてください。内容に合わせて本概要を更新する。
