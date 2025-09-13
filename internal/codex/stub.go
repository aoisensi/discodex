package codex

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoisensi/discodex/internal/config"
)

// Client is a placeholder for a Codex client that would live inside WSL.
// For now, it just echoes the prompt with a canned header and distro.
type Client struct{}

func NewClient() *Client { return &Client{} }

func (c *Client) Chat(ctx context.Context, ch config.Channel, prompt string) (string, error) {
	p := strings.TrimSpace(prompt)
	if p == "" {
		return "", nil
	}
	return fmt.Sprintf("[codex-stub]\n%s", p), nil
}

func (c *Client) ChatMulti(ctx context.Context, ch config.Channel, prompt string) ([]string, error) {
	s, err := c.Chat(ctx, ch, prompt)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	return []string{s}, nil
}
