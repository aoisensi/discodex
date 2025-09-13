# TROUBLESHOOTING

## 応答が返ってこない
- Botの Message Content Intent が有効か確認
- `DISCODEX_DEBUG=1` で起動し、標準出力の `mcp => / <=` ログを確認
- レスポンス例（数行）を保存して報告

## MCPが終了しない
- Ctrl+C後に `ps -o pid,ppid,pgid,cmd -e | rg codex` で残存確認
- 本体はプロセスグループkill→killを実装済み。残る場合はログを添付

## 既定動作を変えたい
- ストリーミング編集の間隔は `internal/discordbot/bot.go` の 250ms を調整
- Presenceの種別（Watching/Listening/Playing）は `SetReasoningStatus` を変更

## 設定が読まれない
- `DISCODEX_CONFIG` に指定があればそちらを読む
- TOMLの `channels[].channel_id` が空だと起動エラー

## MCPの応答形式が違う
- `extractAgentMessages` や `extractTextFromResult` が拾えていない可能性
- `DISCODEX_DEBUG=1` で数行の応答例を共有してください

