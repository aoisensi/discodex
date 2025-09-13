package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aoisensi/discodex/internal/config"
)

type MCPBridge struct {
	conf config.Codex

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scan    *bufio.Scanner
	ready   bool
	reqID   int64
	pending map[int64]chan json.RawMessage

	// channelID -> conversationId
	convo sync.Map

	// closed when the running process exits
	deadCh chan struct{}
}

func NewMCPBridge(conf config.Codex) *MCPBridge {
	return &MCPBridge{conf: conf, pending: map[int64]chan json.RawMessage{}}
}

func (m *MCPBridge) ensureStarted(ctx context.Context, ch config.Channel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ready && m.deadCh != nil && !m.isDead() {
		return nil
	}
	// start new process
	return m.startLocked(ctx, ch)
}

func (m *MCPBridge) isDead() bool {
	select {
	case <-m.deadCh:
		return true
	default:
		return false
	}
}

func (m *MCPBridge) startLocked(ctx context.Context, ch config.Channel) error {
	// assume m.mu is held
	// Build command
	var cmd *exec.Cmd
	line := strings.TrimSpace(ch.Command)
	if line == "" {
		line = strings.TrimSpace(m.conf.Command)
	}
	if line == "" {
		// default to codex mcp: resolve actual binary/script and execute directly
		if p, e := exec.LookPath("codex"); e == nil {
			if runtime.GOOS == "windows" {
				ext := strings.ToLower(filepath.Ext(p))
				switch ext {
				case ".ps1":
					cmd = exec.CommandContext(ctx, "powershell", "-NoLogo", "-ExecutionPolicy", "Bypass", "-File", p, "mcp")
				default:
					cmd = exec.CommandContext(ctx, p, "mcp")
				}
			} else {
				cmd = exec.CommandContext(ctx, p, "mcp")
			}
		} else {
			// fallback
			cmd = exec.CommandContext(ctx, "codex", "mcp")
		}
	} else {
		// run via shell (portable)
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "powershell", "-NoLogo", "-Command", line)
		} else {
			cmd = exec.CommandContext(ctx, "bash", "-lc", line)
		}
	}
	if ch.Workdir != "" {
		cmd.Dir = ch.Workdir
	}
	if len(ch.Env) > 0 {
		env := []string{}
		env = append(env, cmd.Env...)
		keys := make([]string, 0, len(ch.Env))
		for k := range ch.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, fmt.Sprintf("%s=%s", k, ch.Env[k]))
		}
		cmd.Env = env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	m.cmd = cmd
	m.stdin = stdin
	sc := bufio.NewScanner(stdout)
	buf := make([]byte, 64*1024)
	sc.Buffer(buf, 1024*1024)
	m.scan = sc
	go m.readLoop()
	dead := make(chan struct{})
	m.deadCh = dead
	go func() {
		_ = cmd.Wait()
		m.mu.Lock()
		m.ready = false
		close(dead)
		m.mu.Unlock()
	}()

	// Initialize
	type initParams struct {
		ProtocolVersion string            `json:"protocolVersion"`
		Capabilities    map[string]any    `json:"capabilities"`
		ClientInfo      map[string]string `json:"clientInfo"`
	}
	_, err = m.request(ctx, "initialize", initParams{
		ProtocolVersion: "2024-05-31",
		Capabilities:    map[string]any{},
		ClientInfo:      map[string]string{"name": "discodex", "version": "0.1.0"},
	})
	if err != nil {
		return err
	}
	_ = m.notify("initialized", map[string]any{})
	m.ready = true
	return nil
}

func (m *MCPBridge) Chat(ctx context.Context, ch config.Channel, prompt string) (string, error) {
	if err := m.ensureStarted(ctx, ch); err != nil {
		return "", err
	}
	// Decide tool
	var tool string
	args := map[string]any{"prompt": prompt}
	if v, ok := m.convo.Load(ch.ChannelID); ok {
		tool = "codex-reply"
		args["conversationId"] = v.(string)
	} else {
		tool = "codex"
		args["sandbox"] = "workspace-write"
		args["approval-policy"] = "never"
		if ch.Workdir != "" {
			args["cwd"] = ch.Workdir
		}
	}
	type callParams struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	// Try once; if write error due to closed pipe, restart and retry
	res, err := m.request(ctx, "tools/call", callParams{Name: tool, Arguments: args})
	if err != nil {
		// attempt restart on write error or closed pipe
		m.mu.Lock()
		// close old stdin/cmd if present
		if m.stdin != nil {
			_ = m.stdin.Close()
			m.stdin = nil
		}
		if m.cmd != nil {
			_ = m.cmd.Process.Kill()
		}
		m.ready = false
		m.pending = map[int64]chan json.RawMessage{}
		m.mu.Unlock()
		if e := m.ensureStarted(ctx, ch); e == nil {
			res, err = m.request(ctx, "tools/call", callParams{Name: tool, Arguments: args})
		}
		if err != nil {
			return "", err
		}
	}
	if err != nil {
		return "", err
	}
	var obj map[string]any
	if err := json.Unmarshal(res, &obj); err == nil {
		if cid, ok := obj["conversationId"].(string); ok && cid != "" {
			m.convo.Store(ch.ChannelID, cid)
		}
		if msg := extractTextFromResult(obj); msg != "" {
			return msg, nil
		}
	}
	return strings.TrimSpace(string(res)), nil
}

func extractTextFromResult(obj map[string]any) string {
	if s, ok := obj["message"].(string); ok {
		return strings.TrimSpace(s)
	}
	if s, ok := obj["content"].(string); ok {
		return strings.TrimSpace(s)
	}
	if arr, ok := obj["messages"].([]any); ok {
		for i := len(arr) - 1; i >= 0; i-- {
			if m, ok := arr[i].(map[string]any); ok {
				if s, ok := m["message"].(string); ok {
					return strings.TrimSpace(s)
				}
				if s, ok := m["content"].(string); ok {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	if content, ok := obj["content"].([]any); ok {
		for i := len(content) - 1; i >= 0; i-- {
			if part, ok := content[i].(map[string]any); ok {
				if t, ok := part["text"].(string); ok {
					return strings.TrimSpace(t)
				}
			}
		}
	}
	// Common MCP tools/call shape: { result: { content: [{type:"text", text:"..."}], conversationId } }
	if res, ok := obj["result"].(map[string]any); ok {
		if t := extractTextFromResult(res); t != "" {
			return t
		}
	}
	// Some servers nest under data/result
	if data, ok := obj["data"].(map[string]any); ok {
		if t := extractTextFromResult(data); t != "" {
			return t
		}
	}
	return ""
}

// parseID converts a string id like "1" to int64.
func parseID(s string) (int64, error) {
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-numeric id: %q", s)
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// osDebug reports whether to emit debug logs for MCP I/O.
func osDebug() bool {
	if v, ok := os.LookupEnv("DISCODEX_DEBUG"); ok {
		return v != "" && v != "0" && strings.ToLower(v) != "false"
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func (m *MCPBridge) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&m.reqID, 1)
	ch := make(chan json.RawMessage, 1)
	m.mu.Lock()
	m.pending[id] = ch
	m.mu.Unlock()
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	b, _ := json.Marshal(req)
	if _, err := m.stdin.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	to := m.conf.TimeoutSeconds
	if to <= 0 {
		to = 180
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		return res, nil
	case <-time.After(time.Duration(to) * time.Second):
		return nil, errors.New("mcp request timeout")
	}
}

func (m *MCPBridge) notify(method string, params any) error {
	req := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	b, _ := json.Marshal(req)
	_, err := m.stdin.Write(append(b, '\n'))
	return err
}

func (m *MCPBridge) readLoop() {
	for m.scan.Scan() {
		line := strings.TrimSpace(m.scan.Text())
		if line == "" {
			continue
		}
		var raw map[string]any
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		if osDebug() {
			// 軽量に先頭だけログ
			log.Printf("mcp <= %s", truncate(line, 240))
		}
		switch v := raw["id"].(type) {
		case float64:
			id := int64(v)
			if res, ok := raw["result"]; ok {
				m.deliver(id, res)
			} else if errObj, ok := raw["error"]; ok {
				b, _ := json.Marshal(errObj)
				m.deliver(id, json.RawMessage(b))
			}
		case string:
			// 一部実装は id を文字列で返すことがある
			if id, err := parseID(v); err == nil {
				if res, ok := raw["result"]; ok {
					m.deliver(id, res)
				} else if errObj, ok := raw["error"]; ok {
					b, _ := json.Marshal(errObj)
					m.deliver(id, json.RawMessage(b))
				}
			}
		}
	}
}

func (m *MCPBridge) deliver(id int64, v any) {
	b, _ := json.Marshal(v)
	m.mu.Lock()
	ch, ok := m.pending[id]
	if ok {
		delete(m.pending, id)
	}
	m.mu.Unlock()
	if ok {
		ch <- b
	}
}

// Close attempts to gracefully terminate the MCP process.
func (m *MCPBridge) Close() {
	// try graceful shutdown via MCP before killing the process
	m.mu.Lock()
	cmd := m.cmd
	stdin := m.stdin
	m.mu.Unlock()

	if stdin != nil {
		// best-effort shutdown sequence
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		// ignore errors; the goal is to nudge the server to exit
		_, _ = m.request(ctx, "shutdown", map[string]any{})
		cancel()
		_ = m.notify("exit", map[string]any{})
		time.Sleep(100 * time.Millisecond)
		_ = stdin.Close()
	}
	if cmd != nil {
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(1 * time.Second):
			_ = cmd.Process.Kill()
		}
	}
}
