package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aoisensi/discodex/internal/codex"
	"github.com/aoisensi/discodex/internal/config"
	"github.com/aoisensi/discodex/internal/discordbot"
)

func main() {
	log.Println("hi")
	// 設定ロード（必須）
	conf, err := config.LoadDefault()
	if err != nil {
		log.Fatalf("config load: %v", err)
	}
	if conf.Discord.BotToken == "" {
		log.Fatal("discord.bot_token が未設定 (TOML)")
	}

	bot, err := discordbot.New(conf.Discord.BotToken, conf.Discord.GuildID)
	if err != nil {
		log.Fatalf("bot init: %v", err)
	}
	// チャンネル→設定のマップ
	cmap := map[string]config.Channel{}
	for _, ch := range conf.Channels {
		cmap[ch.ChannelID] = ch
	}

	// Codexクライアント（MCP常駐）
	runner := codex.NewMCPBridge(conf.Codex)
	chatFn := func(ctx context.Context, ch config.Channel, prompt string) (string, error) {
		return runner.Chat(ctx, ch, prompt)
	}

	bot.WithChannelMap(cmap).WithChatHandler(chatFn)

	// Run with graceful shutdown support
	go func() {
		if err := bot.Run(); err != nil {
			log.Printf("bot run ended: %v", err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	// graceful shutdown
	log.Println("shutdown...")
	runner.Close()
	bot.Stop()
	time.Sleep(300 * time.Millisecond)
}
