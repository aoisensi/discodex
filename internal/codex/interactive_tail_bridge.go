package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aoisensi/discodex/internal/config"
)

// InteractiveTailBridge runs `codex` interactively and tails .codex/sessions/*.jsonl for outputs.
type InteractiveTailBridge struct {
	conf config.Codex
	mu   sync.Mutex
	// channelID -> session
	m map[string]*itSession
}

func NewInteractiveTailBridge(conf config.Codex) *InteractiveTailBridge {
	return &InteractiveTailBridge{conf: conf, m: map[string]*itSession{}}
}

func (b *InteractiveTailBridge) Chat(ctx context.Context, ch config.Channel, prompt string) (string, error) {
	s, err := b.ensure(ctx, ch)
	if err != nil {
		return "", err
	}
	// Send prompt
	if _, err := io.WriteString(s.stdin, strings.TrimSpace(prompt)+"\n"); err != nil {
		return "", err
	}
	// Collect agent message from tail until idle or timeout
	to := b.conf.TimeoutSeconds
	if to <= 0 {
		to = 180
	}
	idle := time.NewTimer(1200 * time.Millisecond)
	defer idle.Stop()
	deadline := time.NewTimer(time.Duration(to) * time.Second)
	defer deadline.Stop()
	var last string
	for {
		select {
		case <-ctx.Done():
			if last == "" {
				return "", ctx.Err()
			}
			return last, nil
		case <-deadline.C:
			if last == "" {
				return "", errors.New("interactive tail timeout")
			}
			return last, nil
		case m := <-s.out:
			if m != "" {
				last = m
			}
			idle.Reset(1200 * time.Millisecond)
		case <-idle.C:
			if last != "" {
				return last, nil
			}
			idle.Reset(1200 * time.Millisecond)
		}
	}
}

type itSession struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   chan string
}

type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func newLimitedBuffer(max int) *limitedBuffer { return &limitedBuffer{max: max} }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	// Store up to max bytes; always report full write to avoid backpressure
	remaining := b.max - b.buf.Len()
	if remaining > 0 {
		if len(p) <= remaining {
			_, _ = b.buf.Write(p)
		} else {
			_, _ = b.buf.Write(p[:remaining])
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

func (b *InteractiveTailBridge) ensure(ctx context.Context, ch config.Channel) (*itSession, error) {
	b.mu.Lock()
	s := b.m[ch.ChannelID]
	b.mu.Unlock()
	if s != nil {
		return s, nil
	}
	// Determine sessions root and take a baseline snapshot before starting Codex
	root, rerr := b.waitRoot()
	if rerr != nil {
		return nil, rerr
	}
	before := snapshotSessionFiles(root)
	// Build command line
	// default: `codex -a never --sandbox workspace-write`
	var cmd *exec.Cmd
	line := strings.TrimSpace(ch.Command)
	if line != "" {
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "powershell", "-NoLogo", "-Command", line)
		} else {
			cmd = exec.CommandContext(ctx, "bash", "-lc", line)
		}
	} else {
		cmd = exec.CommandContext(ctx, "codex", "-a", "never", "--sandbox", "workspace-write")
	}
	if strings.TrimSpace(ch.Workdir) != "" {
		cmd.Dir = ch.Workdir
	}
	if len(ch.Env) > 0 {
		env := append([]string{}, os.Environ()...)
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
		return nil, err
	}
	// Always pipe stdout/stderr and drain to avoid blocking when buffers fill
	stdout, e1 := cmd.StdoutPipe()
	if e1 != nil {
		return nil, e1
	}
	stderr, e2 := cmd.StderrPipe()
	if e2 != nil {
		return nil, e2
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Capture early stdout/stderr (with size cap) for troubleshooting if the process exits immediately
	lbOut := newLimitedBuffer(64 * 1024)
	lbErr := newLimitedBuffer(64 * 1024)
	go func(r io.Reader) { _, _ = io.Copy(lbOut, r) }(stdout)
	go func(r io.Reader) { _, _ = io.Copy(lbErr, r) }(stderr)
	go func() {
		_ = cmd.Wait()
		if time.Since(start) < time.Second {
			log.Printf("codex exited early (%v). stdout:\n%s\nstderr:\n%s", time.Since(start), lbOut.String(), lbErr.String())
		}
	}()

	// Find session file by diffing with baseline
	sessPath, err := waitNewSessionFileFromSnapshot(root, before, start, 20*time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	out := make(chan string, 8)
	go tailJSONL(sessPath, out)

	ns := &itSession{cmd: cmd, stdin: stdin, out: out}
	b.mu.Lock()
	b.m[ch.ChannelID] = ns
	b.mu.Unlock()
	return ns, nil
}

func (b *InteractiveTailBridge) waitRoot() (string, error) {
	if r := strings.TrimSpace(b.conf.SessionRoot); r != "" {
		return filepath.Clean(r), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(home, ".codex", "sessions")), nil
}

func waitLatestSessionFileAt(root string, since time.Time, timeout time.Duration) (string, error) {
	root = filepath.Clean(root)
	deadline := time.Now().Add(timeout)
	var latestPath string
	var latestMod time.Time
	for time.Now().Before(deadline) {
		latestPath = ""
		latestMod = time.Time{}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(strings.ToLower(d.Name()), ".jsonl") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			mt := info.ModTime()
			// Prefer files modified after process start (with small grace)
			if mt.Before(since.Add(-2 * time.Second)) {
				return nil
			}
			if mt.After(latestMod) {
				latestMod = mt
				latestPath = path
			}
			return nil
		})
		if latestPath != "" {
			return latestPath, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "", fmt.Errorf("session file not found in %q", root)
}

type sessionSnapshot map[string]time.Time

func snapshotSessionFiles(root string) sessionSnapshot {
	snap := make(sessionSnapshot)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".jsonl") {
			return nil
		}
		if info, e := d.Info(); e == nil {
			snap[path] = info.ModTime()
		}
		return nil
	})
	return snap
}

func waitNewSessionFileFromSnapshot(root string, before sessionSnapshot, since time.Time, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var candidate string
		var latest time.Time
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(strings.ToLower(d.Name()), ".jsonl") {
				return nil
			}
			info, e := d.Info()
			if e != nil {
				return nil
			}
			mt := info.ModTime()
			// new or updated compared to baseline, and modified after start (with grace)
			if mt.Before(since.Add(-2 * time.Second)) {
				return nil
			}
			if bt, ok := before[path]; ok && !mt.After(bt) {
				return nil
			}
			if mt.After(latest) {
				latest, candidate = mt, path
			}
			return nil
		})
		if candidate != "" {
			return candidate, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "", fmt.Errorf("session file not found in %q", root)
}

func tailJSONL(path string, out chan<- string) {
	// open and seek to end
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	// start from end to avoid flooding old logs
	if _, err := f.Seek(0, io.SeekEnd); err != nil { /* ignore */
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if errors.Is(err, io.EOF) {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if err != nil {
			return
		}
		if m := extractAgentMessageFromAnyJSON(line); m != "" {
			out <- m
		}
	}
}

func extractAgentMessageFromAnyJSON(line string) string {
	s := strings.TrimSpace(line)
	if s == "" {
		return ""
	}
	var v map[string]any
	if json.Unmarshal([]byte(s), &v) != nil {
		return ""
	}
	// Possible shapes: {msg:{type,message}}, {type,message}, {messages:[...]} etc.
	if msg, ok := v["msg"].(map[string]any); ok {
		if t, _ := msg["type"].(string); t == "agent_message" {
			if m, _ := msg["message"].(string); m != "" {
				return strings.TrimSpace(m)
			}
		}
	}
	if t, _ := v["type"].(string); t == "agent_message" {
		if m, _ := v["message"].(string); m != "" {
			return strings.TrimSpace(m)
		}
	}
	// messages array fallback
	if arr, ok := v["messages"].([]any); ok {
		for i := len(arr) - 1; i >= 0; i-- {
			if m, ok := arr[i].(map[string]any); ok {
				if t, _ := m["type"].(string); t == "agent_message" {
					if s, _ := m["message"].(string); s != "" {
						return strings.TrimSpace(s)
					}
				}
			}
		}
	}
	return ""
}
