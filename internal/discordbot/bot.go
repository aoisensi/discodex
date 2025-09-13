package discordbot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aoisensi/discodex/internal/config"
	"github.com/bwmarrin/discordgo"
)

type Bot struct {
	token   string
	session *discordgo.Session

	appID   string
	guildID string

	channelMap map[string]config.Channel
	stopCh     chan struct{}

	onChat func(ctx context.Context, ch config.Channel, prompt string) (string, error)
}

func New(token string, guildID string) (*Bot, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	b := &Bot{
		token:   token,
		session: s,
		guildID: guildID,
		stopCh:  make(chan struct{}),
	}
	b.session.Identify.Intents = discordgo.IntentGuilds | discordgo.IntentGuildMessages | discordgo.IntentMessageContent
	b.session.AddHandler(b.onReady)
	b.session.AddHandler(b.onMessageCreate)
	return b, nil
}

func (b *Bot) WithChatHandler(chat func(ctx context.Context, ch config.Channel, prompt string) (string, error)) *Bot {
	b.onChat = chat
	return b
}

func (b *Bot) WithChannelMap(m map[string]config.Channel) *Bot {
	b.channelMap = m
	return b
}

func (b *Bot) Run() error {
	if err := b.session.Open(); err != nil {
		return err
	}

	// Cache application ID
	if app, err := b.session.Application("@me"); err == nil {
		b.appID = app.ID
	}

	// Block until Stop is called
	<-b.stopCh
	_ = b.session.Close()
	return nil
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	log.Printf("logged in as %s#%s", r.User.Username, r.User.Discriminator)
}

// Stop closes the Discord session and unblocks Run.
func (b *Bot) Stop() {
	select {
	case <-b.stopCh:
		// already closed
	default:
		close(b.stopCh)
	}
}

func (b *Bot) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}
	botID := s.State.User.ID
	ch, mapped := b.channelMap[m.ChannelID]
	var prompt string
	if mapped {
		// 紐付け済みチャンネルはメンション不要で全メッセージを扱う
		prompt = strings.TrimSpace(m.Content)
	} else {
		if !isMentioned(m.Content, botID) {
			return
		}
		prompt = stripMention(m.Content, botID)
		// 会話コンテキストをチャンネル単位で保持できるよう、未紐付けでも一時的にIDを設定
		if ch.ChannelID == "" {
			ch.ChannelID = m.ChannelID
		}
	}
	if strings.TrimSpace(prompt) == "" {
		return
	}
	// タイピングインジケータ
	_ = s.ChannelTyping(m.ChannelID)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if b.onChat == nil {
		_, _ = s.ChannelMessageSend(m.ChannelID, "ごめん、まだ会話は未実装だよ")
		return
	}
	reply, err := b.onChat(ctx, ch, prompt)
	if err != nil {
		reply = fmt.Sprintf("エラー: %v", err)
	}
	if reply == "" {
		reply = "(no output)"
	}
	_, _ = s.ChannelMessageSendReply(m.ChannelID, reply, m.Reference())
}

func isMentioned(content, botID string) bool {
	return strings.Contains(content, "<@"+botID+">") || strings.Contains(content, "<@!"+botID+">")
}

func stripMention(content, botID string) string {
	content = strings.ReplaceAll(content, "<@"+botID+">", "")
	content = strings.ReplaceAll(content, "<@!"+botID+">", "")
	return strings.TrimSpace(content)
}
