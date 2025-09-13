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

// context keys for passing metadata (e.g., user)
type ctxKey int

const (
	ctxKeyUserTag ctxKey = iota + 1
)

// WithUserTag attaches a user tag (e.g., Discord display name) to context.
func WithUserTag(ctx context.Context, tag string) context.Context {
	return context.WithValue(ctx, ctxKeyUserTag, tag)
}

type MCPBridge struct {
	conf  config.Codex
	debug bool

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

	// request id -> owner channelID
	owners map[int64]string

	// live reasoning buffer per request id
	reasonBuf map[int64]string

	// callbacks
	onReasoning    func(channelID string, text string)
	onReasoningEnd func(channelID string)
	onAgentDelta   func(channelID string, requestID int64, delta string)
	onAgentDone    func(channelID string, requestID int64, final string)

	// lifecycle callbacks
	onUp   func()
	onDown func()

	// idle shutdown
	idleSeconds int
	idleTimer   *time.Timer
	lastActive  time.Time
}

func NewMCPBridge(conf config.Codex) *MCPBridge {
	dbg := conf.Debug
	if !dbg {
		if v, ok := os.LookupEnv("DISCODEX_DEBUG"); ok {
			dbg = v != "" && v != "0" && strings.ToLower(v) != "false"
		}
	}
	idle := conf.IdleSeconds
	return &MCPBridge{conf: conf, debug: dbg, pending: map[int64]chan json.RawMessage{}, owners: map[int64]string{}, reasonBuf: map[int64]string{}, idleSeconds: idle}
}

// WithReasoningHandler registers callbacks for reasoning status updates.
func (m *MCPBridge) WithReasoningHandler(on func(channelID, text string), done func(channelID string)) *MCPBridge {
	m.onReasoning = on
	m.onReasoningEnd = done
	return m
}

// WithStreamHandler registers callbacks for agent_message streaming.
func (m *MCPBridge) WithStreamHandler(onDelta func(channelID string, requestID int64, delta string), onDone func(channelID string, requestID int64, final string)) *MCPBridge {
	m.onAgentDelta = onDelta
	m.onAgentDone = onDone
	return m
}

// WithStateHandler registers lifecycle callbacks for MCP process up/down.
func (m *MCPBridge) WithStateHandler(onUp func(), onDown func()) *MCPBridge {
	m.onUp = onUp
	m.onDown = onDown
	return m
}

func (m *MCPBridge) touchActivity() {
	if m.idleSeconds <= 0 {
		return
	}
	m.mu.Lock()
	m.lastActive = time.Now()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	sec := m.idleSeconds
	m.idleTimer = time.AfterFunc(time.Duration(sec)*time.Second, func() {
		m.mu.Lock()
		if m.idleSeconds <= 0 {
			m.mu.Unlock()
			return
		}
		last := m.lastActive
		m.mu.Unlock()
		if time.Since(last) < time.Duration(sec)*time.Second {
			return
		}
		if m.debug {
			log.Printf("mcp: idle timeout; closing")
		}
		m.Close()
	})
	m.mu.Unlock()
}

func (m *MCPBridge) ensureStarted(ctx context.Context, ch config.Channel) error {
	m.mu.Lock()
	already := m.ready && m.deadCh != nil && !m.isDead()
	m.mu.Unlock()
	if already {
		return nil
	}
	// start new process (without holding the lock to avoid deadlocks)
	return m.start(ctx, ch)
}

func (m *MCPBridge) isDead() bool {
	select {
	case <-m.deadCh:
		return true
	default:
		return false
	}
}

func (m *MCPBridge) start(ctx context.Context, ch config.Channel) error {
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
	// OS-specific process attributes (e.g., create new process group on Unix)
	setProcAttrs(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if m.debug {
		log.Printf("mcp: starting dir=%q", cmd.Dir)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// set fields under lock
	sc := bufio.NewScanner(stdout)
	buf := make([]byte, 64*1024)
	sc.Buffer(buf, 1024*1024)
	dead := make(chan struct{})
	m.mu.Lock()
	m.cmd = cmd
	m.stdin = stdin
	m.scan = sc
	m.deadCh = dead
	m.mu.Unlock()
	go m.readLoop()
	go func() {
		_ = cmd.Wait()
		m.mu.Lock()
		m.ready = false
		close(dead)
		m.mu.Unlock()
		if m.onDown != nil {
			m.onDown()
		}
	}()

	// Initialize
	type initParams struct {
		ProtocolVersion string            `json:"protocolVersion"`
		Capabilities    map[string]any    `json:"capabilities"`
		ClientInfo      map[string]string `json:"clientInfo"`
	}
	if m.debug {
		log.Printf("mcp: send initialize")
	}
	// Don't block on initialize; some servers delay or omit the response.
	ictx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	_, _ = m.request(ictx, "initialize", initParams{
		ProtocolVersion: "2024-05-31",
		Capabilities:    map[string]any{},
		ClientInfo:      map[string]string{"name": "discodex", "version": "0.1.0"},
	})
	cancel()
	if m.debug {
		log.Printf("mcp: notify initialized")
	}
	_ = m.notify("initialized", map[string]any{})
	m.mu.Lock()
	m.ready = true
	m.mu.Unlock()
	if m.onUp != nil {
		m.onUp()
	}
	// schedule idle shutdown
	m.touchActivity()
	return nil
}

func (m *MCPBridge) Chat(ctx context.Context, ch config.Channel, prompt string) (string, error) {
	msgs, err := m.ChatMulti(ctx, ch, prompt)
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return "", nil
	}
	return strings.Join(msgs, "\n\n"), nil
}

// ChatMulti runs a prompt and returns one Discord message per agent_message.
func (m *MCPBridge) ChatMulti(ctx context.Context, ch config.Channel, prompt string) ([]string, error) {
	if err := m.ensureStarted(ctx, ch); err != nil {
		return nil, err
	}
	m.touchActivity()
	// Decide tool
	var tool string
	args := map[string]any{"prompt": prompt}
	if v := ctx.Value(ctxKeyUserTag); v != nil {
		args["user"] = fmt.Sprintf("%v", v)
	}
	if v, ok := m.convo.Load(ch.ChannelID); ok {
		tool = "codex-reply"
		args["conversationId"] = v.(string)
	} else {
		tool = "codex"
		// preamble を先頭に差し込む
		pre := strings.TrimSpace(m.conf.Preamble)
		if pre != "" {
			p := strings.TrimSpace(prompt)
			prompt = pre + "\n\n" + p
			args["prompt"] = prompt
		}
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
	if m.debug {
		log.Printf("mcp => tools/call %s", tool)
	}
	res, err := m.requestForChannel(ctx, "tools/call", callParams{Name: tool, Arguments: args}, ch.ChannelID)
	if err != nil {
		// attempt restart on write error or closed pipe
		m.mu.Lock()
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
			res, err = m.requestForChannel(ctx, "tools/call", callParams{Name: tool, Arguments: args}, ch.ChannelID)
		}
		if err != nil {
			return nil, err
		}
	}
	m.touchActivity()
	var obj map[string]any
	if err := json.Unmarshal(res, &obj); err == nil {
		if cid, ok := obj["conversationId"].(string); ok && cid != "" {
			m.convo.Store(ch.ChannelID, cid)
		}
		// When streaming callbacks are set, avoid returning messages to prevent duplicates.
		if m.onAgentDelta != nil || m.onAgentDone != nil {
			return nil, nil
		}
		if arr := extractAgentMessages(obj); len(arr) > 0 {
			return arr, nil
		}
		if msg := extractTextFromResult(obj); msg != "" {
			return []string{msg}, nil
		}
	}
	s := strings.TrimSpace(string(res))
	if s == "" {
		return nil, nil
	}
	return []string{s}, nil
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

// extractAgentMessages returns one string per agent_message in a result object.
func extractAgentMessages(obj map[string]any) []string {
	// Typical: { result: { messages: [ {type:"agent_message", message:"..."}, ... ] } }
	if res, ok := obj["result"].(map[string]any); ok {
		if out := extractAgentMessages(res); len(out) > 0 {
			return out
		}
	}
	if data, ok := obj["data"].(map[string]any); ok {
		if out := extractAgentMessages(data); len(out) > 0 {
			return out
		}
	}
	var out []string
	if arr, ok := obj["messages"].([]any); ok {
		for _, it := range arr {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t != "agent_message" {
				continue
			}
			if s, _ := m["message"].(string); strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
				continue
			}
			if s, _ := m["content"].(string); strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
				continue
			}
			if parts, ok := m["content"].([]any); ok {
				var b strings.Builder
				for _, p := range parts {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					if txt, _ := pm["text"].(string); txt != "" {
						b.WriteString(txt)
					}
				}
				s := strings.TrimSpace(b.String())
				if s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
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
	if m.debug {
		log.Printf("mcp => %s %s", method, truncate(string(b), 240))
	}
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

// requestForChannel is like request but records the owner channelID for event correlation.
func (m *MCPBridge) requestForChannel(ctx context.Context, method string, params any, channelID string) (json.RawMessage, error) {
	id := atomic.AddInt64(&m.reqID, 1)
	ch := make(chan json.RawMessage, 1)
	m.mu.Lock()
	m.pending[id] = ch
	if channelID != "" {
		m.owners[id] = channelID
	}
	m.mu.Unlock()
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	b, _ := json.Marshal(req)
	if m.debug {
		log.Printf("mcp => %s %s", method, truncate(string(b), 240))
	}
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
	if m.debug {
		log.Printf("mcp => %s %s", method, truncate(string(b), 240))
	}
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
		if m.debug {
			// 軽量に先頭だけログ
			log.Printf("mcp <= %s", truncate(line, 240))
		}
		// handle notifications (e.g., codex/event)
		if method, _ := raw["method"].(string); method != "" {
			m.handleNotify(raw)
			continue
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

func (m *MCPBridge) handleNotify(raw map[string]any) {
	method, _ := raw["method"].(string)
	if method != "codex/event" {
		return
	}
	params, _ := raw["params"].(map[string]any)
	meta, _ := params["_meta"].(map[string]any)
	var owner string
	if rv, ok := meta["requestId"]; ok {
		switch t := rv.(type) {
		case float64:
			id := int64(t)
			m.mu.Lock()
			owner = m.owners[id]
			m.mu.Unlock()
		case string:
			if id, err := parseID(t); err == nil {
				m.mu.Lock()
				owner = m.owners[id]
				m.mu.Unlock()
			}
		}
	}
	msg, _ := params["msg"].(map[string]any)
	if msg == nil {
		return
	}
	typ, _ := msg["type"].(string)
	// any event counts as activity
	m.touchActivity()
	switch typ {
	case "agent_reasoning_delta":
		delta, _ := msg["delta"].(string)
		if delta == "" {
			return
		}
		// append to buffer keyed by request id (if available) or by ownerless 0
		var key int64
		if rv, ok := meta["requestId"].(float64); ok {
			key = int64(rv)
		}
		m.mu.Lock()
		m.reasonBuf[key] = m.reasonBuf[key] + delta
		text := m.reasonBuf[key]
		m.mu.Unlock()
		if m.onReasoning != nil && owner != "" {
			m.onReasoning(owner, truncate(text, 120))
		}
	case "agent_reasoning":
		final, _ := msg["message"].(string)
		if final == "" {
			return
		}
		if m.onReasoning != nil && owner != "" {
			m.onReasoning(owner, truncate(final, 120))
		}
	case "agent_message_delta":
		d, _ := msg["delta"].(string)
		if d == "" {
			return
		}
		var reqID int64
		if rv, ok := meta["requestId"].(float64); ok {
			reqID = int64(rv)
		}
		if m.onAgentDelta != nil && owner != "" {
			m.onAgentDelta(owner, reqID, d)
		}
	case "agent_message":
		final, _ := msg["message"].(string)
		var reqID int64
		if rv, ok := meta["requestId"].(float64); ok {
			reqID = int64(rv)
		}
		if m.onAgentDone != nil && owner != "" {
			m.onAgentDone(owner, reqID, final)
		}
		fallthrough
	case "task_complete":
		// clear buffer and notify end
		var key int64
		if rv, ok := meta["requestId"].(float64); ok {
			key = int64(rv)
		}
		m.mu.Lock()
		delete(m.reasonBuf, key)
		m.mu.Unlock()
		if m.onReasoningEnd != nil && owner != "" {
			m.onReasoningEnd(owner)
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
			// try kill process group (Unix), then process
			killProcessGroup(cmd)
			_ = cmd.Process.Kill()
		}
	}
}

// Reset clears conversation state for the channel.
func (m *MCPBridge) Reset(channelID string) {
	if channelID == "" {
		return
	}
	m.convo.Delete(channelID)
	if m.debug {
		log.Printf("mcp: reset conversation for channel %s", channelID)
	}
}
