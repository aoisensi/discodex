package discordbot

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aoisensi/discodex/internal/codex"
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

	onChat  func(ctx context.Context, ch config.Channel, prompt string) ([]string, error)
	onReset func(ctx context.Context, ch config.Channel) error

	// streaming state
	streams map[string]*streamState

	// detailed error log destination channel
	logChannelID string

	// typing indicator controllers per channel
	typing map[string]context.CancelFunc
}

type streamState struct {
	messageID string
	content   string
	lastEdit  time.Time
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
		streams: map[string]*streamState{},
		typing:  map[string]context.CancelFunc{},
	}
	b.session.Identify.Intents = discordgo.IntentGuilds | discordgo.IntentGuildMessages | discordgo.IntentMessageContent
	b.session.AddHandler(b.onReady)
	b.session.AddHandler(b.onMessageCreate)
	return b, nil
}

func (b *Bot) WithChatHandler(chat func(ctx context.Context, ch config.Channel, prompt string) ([]string, error)) *Bot {
	b.onChat = chat
	return b
}

func (b *Bot) WithResetHandler(reset func(ctx context.Context, ch config.Channel) error) *Bot {
	b.onReset = reset
	return b
}

func (b *Bot) WithChannelMap(m map[string]config.Channel) *Bot {
	b.channelMap = m
	return b
}

func (b *Bot) WithLogChannel(id string) *Bot {
	b.logChannelID = strings.TrimSpace(id)
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

// SetReasoningStatus updates bot presence with reasoning text.
func (b *Bot) SetReasoningStatus(text string) {
	if b.session == nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if len(text) > 120 {
		text = text[:120]
	}
	_ = b.session.UpdateStatusComplex(discordgo.UpdateStatusData{
		Status:     "online",
		Activities: []*discordgo.Activity{{Name: text, Type: discordgo.ActivityTypeWatching}},
	})
}

// ClearStatus clears bot activities, keeping status online.
func (b *Bot) ClearStatus() {
	if b.session == nil {
		return
	}
	_ = b.session.UpdateStatusComplex(discordgo.UpdateStatusData{Status: "online", Activities: nil})
}

// SetAway sets presence to idle with "退出中" activity.
func (b *Bot) SetAway() {
	if b.session == nil {
		return
	}
	_ = b.session.UpdateStatusComplex(discordgo.UpdateStatusData{Status: "idle", Activities: nil})
}

// ApplyStreamDelta appends delta for request and edits the message.
func (b *Bot) ApplyStreamDelta(channelID string, requestID int64, delta string) {
	if b.session == nil {
		return
	}
	// ensure typing indicator is active during streaming
	b.startTyping(channelID)
	key := fmt.Sprintf("%s#%d", channelID, requestID)
	st, ok := b.streams[key]
	if !ok {
		// create new message with initial delta
		msg, err := b.session.ChannelMessageSend(channelID, delta)
		if err != nil {
			return
		}
		b.streams[key] = &streamState{messageID: msg.ID, content: delta, lastEdit: time.Now()}
		return
	}
	st.content += delta
	// simple throttle to avoid hitting rate limits
	if time.Since(st.lastEdit) < 250*time.Millisecond {
		return
	}
	st.lastEdit = time.Now()
	_, _ = b.session.ChannelMessageEdit(channelID, st.messageID, st.content)
}

// EndStream finalizes the stream by setting final text and clearing state.
func (b *Bot) EndStream(channelID string, requestID int64, final string) {
	if b.session == nil {
		return
	}
	key := fmt.Sprintf("%s#%d", channelID, requestID)
	st, ok := b.streams[key]
	if !ok {
		if strings.TrimSpace(final) != "" {
			_, _ = b.session.ChannelMessageSend(channelID, final)
		}
		return
	}
	if strings.TrimSpace(final) != "" {
		st.content = final
	}
	_, _ = b.session.ChannelMessageEdit(channelID, st.messageID, st.content)
	delete(b.streams, key)
	b.stopTyping(channelID)
}

// NotifyShutdown posts a shutdown notice to mapped channels and sets presence offline.
func (b *Bot) NotifyShutdown(msg string) {
	if b.session == nil {
		return
	}
	if strings.TrimSpace(msg) == "" {
		msg = "discodex: 終了する"
	}
	for chID := range b.channelMap {
		_, _ = b.session.ChannelMessageSend(chID, msg)
	}
	_ = b.session.UpdateStatusComplex(discordgo.UpdateStatusData{Status: "invisible", Activities: nil})
}

func (b *Bot) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}
	botID := s.State.User.ID
	ch, mapped := b.channelMap[m.ChannelID]
	if debugEnabled() {
		log.Printf("msg: ch=%s author=%s content.len=%d mentions=%d mapped=%v", m.ChannelID, m.Author.ID, len(m.Content), len(m.Mentions), mapped)
	}
	var prompt string
	if mapped {
		// 紐付け済みチャンネルはメンション不要で全メッセージを扱う
		prompt = strings.TrimSpace(m.Content)
	} else {
		if !isMentioned(m.Content, botID) && !isMentionedByArray(m.Mentions, botID) {
			return
		}
		prompt = stripMention(m.Content, botID)
		// 会話コンテキストをチャンネル単位で保持できるよう、未紐付けでも一時的にIDを設定
		if ch.ChannelID == "" {
			ch.ChannelID = m.ChannelID
		}
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "/reset" {
		if b.onReset != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := b.onReset(ctx, ch); err != nil {
				b.reportErrorf("reset", err)
				_, _ = s.ChannelMessageSend(m.ChannelID, "リセットに失敗した")
			} else {
				// clear local stream state too
				b.ResetChannelStreams(m.ChannelID)
				b.ClearStatus()
				_, _ = s.ChannelMessageSend(m.ChannelID, "会話をリセットした")
			}
		}
		return
	}
	if strings.TrimSpace(prompt) == "" {
		if debugEnabled() {
			log.Printf("msg: empty content; Message Content Intent 未許可の可能性")
		}
		return
	}
	// タイピングインジケータ（5秒ごとに再表示）
	b.startTyping(m.ChannelID)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// attach user tag for Codex
	tag := buildUserTag(m)
	if tag != "" {
		ctx = codex.WithUserTag(ctx, tag)
	}
	if b.onChat == nil {
		_, _ = s.ChannelMessageSend(m.ChannelID, "ごめん、まだ会話は未実装だよ")
		return
	}
	replies, err := b.onChat(ctx, ch, prompt)
	if err != nil {
		b.reportErrorf("chat", err)
		replies = []string{"エラーが発生した"}
	}
	if len(replies) == 0 {
		// streamingの場合はEndStreamで止める
		return
	}
	// 非ストリーミング（即時応答）はここでtyping停止
	b.stopTyping(m.ChannelID)
	for _, msg := range replies {
		msg = strings.TrimSpace(msg)
		if msg == "" {
			continue
		}
		_, _ = s.ChannelMessageSend(m.ChannelID, msg)
	}
}

func buildUserTag(m *discordgo.MessageCreate) string {
	if m == nil || m.Author == nil {
		return ""
	}
	user := m.Author
	// prefer guild nickname, then global name, then username
	display := ""
	if m.Member != nil && strings.TrimSpace(m.Member.Nick) != "" {
		display = m.Member.Nick
	}
	if display == "" && strings.TrimSpace(user.GlobalName) != "" {
		display = user.GlobalName
	}
	if display == "" {
		display = user.Username
	}
	tag := display
	if d := strings.TrimSpace(user.Discriminator); d != "" && d != "0" {
		tag = fmt.Sprintf("%s#%s", display, d)
	}
	return tag
}

// ResetChannelStreams clears any in-flight streaming state for a channel.
func (b *Bot) ResetChannelStreams(channelID string) {
	if b.streams == nil {
		return
	}
	for k := range b.streams {
		if strings.HasPrefix(k, channelID+"#") {
			delete(b.streams, k)
		}
	}
	b.stopTyping(channelID)
}

// startTyping begins a 5s ticker to send ChannelTyping until stopped.
func (b *Bot) startTyping(channelID string) {
	if b.session == nil || channelID == "" {
		return
	}
	if b.typing == nil {
		b.typing = map[string]context.CancelFunc{}
	}
	if _, ok := b.typing[channelID]; ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.typing[channelID] = cancel
	go func() {
		// immediate fire
		_ = b.session.ChannelTyping(channelID)
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = b.session.ChannelTyping(channelID)
			}
		}
	}()
}

// stopTyping cancels the typing ticker for a channel.
func (b *Bot) stopTyping(channelID string) {
	if b.typing == nil {
		return
	}
	if cancel, ok := b.typing[channelID]; ok {
		cancel()
		delete(b.typing, channelID)
	}
}

func isMentioned(content, botID string) bool {
	return strings.Contains(content, "<@"+botID+">") || strings.Contains(content, "<@!"+botID+">")
}

func stripMention(content, botID string) string {
	content = strings.ReplaceAll(content, "<@"+botID+">", "")
	content = strings.ReplaceAll(content, "<@!"+botID+">", "")
	return strings.TrimSpace(content)
}

func isMentionedByArray(mentions []*discordgo.User, botID string) bool {
	for _, u := range mentions {
		if u != nil && u.ID == botID {
			return true
		}
	}
	return false
}

func debugEnabled() bool {
	if v, ok := os.LookupEnv("DISCODEX_DEBUG"); ok {
		v = strings.ToLower(strings.TrimSpace(v))
		return v != "" && v != "0" && v != "false"
	}
	return false
}

// reportErrorf posts detailed error to log channel if configured.
func (b *Bot) reportErrorf(tag string, err error) {
	if err == nil {
		return
	}
	msg := fmt.Sprintf("[%s] %v", tag, err)
	if b.logChannelID != "" && b.session != nil {
		for _, part := range splitDiscordMessage(msg) {
			_, _ = b.session.ChannelMessageSend(b.logChannelID, part)
		}
		return
	}
	log.Printf("%s", msg)
}

// splitDiscordMessage chunks text within ~1900 chars to avoid 2000 limit.
func splitDiscordMessage(s string) []string {
	const lim = 1900
	if len(s) <= lim {
		return []string{s}
	}
	var out []string
	for i := 0; i < len(s); i += lim {
		j := i + lim
		if j > len(s) {
			j = len(s)
		}
		out = append(out, s[i:j])
	}
	return out
}
