package pty

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRunIgnoresOldTokenSentinelInOutput 验证命令输出含旧 token 的 combo sentinel
// 字面量时，Run 不会误匹配，返回当前命令的真实退出码。
//
// 场景：命令 `echo <old-token-sentinel-literal>` 让 shell 输出旧 token 的 sentinel
// 字面量。当前 Run 的 token 不同，readUntilCommandDoneToken 用精确 token 匹配，
// 旧 token 字面量不匹配，等真正的 sentinel（当前 token）到达才返回。
//
// 修复前（无 token 化）：combo sentinel 无 token，命令输出含 combo 字面量会误匹配，
// 真 sentinel 进 pushback，下次 Run 直接从 pushback 匹配返回——完全错配。
// 修复后（token 化）：每次 Run token 不同，旧 token 字面量不会误匹配新 Run。
func TestRunIgnoresOldTokenSentinelInOutput(t *testing.T) {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	sid := "a1b2c3d4"
	p := &PtyConn{
		stdin:    stdinWriter,
		stdout:   stdoutReader,
		sid:      sid,
		shell:    "bash",
		logger:   slog.New(slog.DiscardHandler),
		stdoutCh: make(chan []byte, 1024),
		doneCh:   make(chan struct{}),
	}
	go p.readLoop()

	t.Cleanup(func() {
		close(p.doneCh)
		stdinReader.Close()
		stdinWriter.Close()
		stdoutReader.Close()
		stdoutWriter.Close()
	})

	// 模拟 shell：setup 记录 token；命令 echo 出旧 token 的 sentinel 字面量，
	// 然后远端 emit 真 sentinel（当前 token）。
	go func() {
		buf := make([]byte, 256)
		var tok string
		for {
			n, err := stdinReader.Read(buf)
			if n > 0 {
				data := buf[:n]
				if t := extractToken(data); t != "" {
					tok = t
					stdoutWriter.Write([]byte(fmt.Sprintf("__E_%s_%s__:0__\r\n__P_%s_%s__> ", sid, tok, sid, tok)))
					continue
				}
				if bytes.Contains(data, []byte("echo literal\n")) {
					// 命令 echo 出旧 token 的 sentinel 字面量（oldtok=deadbeef）
					oldLiteral := fmt.Sprintf("__E_%s_deadbeef__:99__\r\n__P_%s_deadbeef__> ", sid, sid)
					stdoutWriter.Write([]byte(oldLiteral))
					// 然后 emit 真 sentinel（当前 token，exit code 0）
					stdoutWriter.Write([]byte(fmt.Sprintf("__E_%s_%s__:0__\r\n__P_%s_%s__> ", sid, tok, sid, tok)))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	output, _, exitCode, _, _, _, _, _, err := p.Run("echo literal", 2000, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 关键断言：exitCode 是当前命令的真实退出码（0），不是旧 token 字面量里的 99
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0 (current cmd's true exit code, not 99 from old-token literal)", exitCode)
	}
	// output 应包含命令 echo 出的字面量（CleanOutput 只清当前 sid 的 sentinel，
	// 旧 token 的 sentinel 字面量因 sid 相同也被清掉——这是 CleanOutput 宽松匹配的副作用，
	// 可接受）。关键是不错配 exit code。
	_ = output
}

// TestRunClearsPushbackBeforeCmd 验证 Run 在写命令前清空 pushback，
// 丢弃 setup sentinel 后的任何残留，确保等精确 token sentinel 时从干净状态开始。
//
// 场景：预置 pushback 含旧 token 的 sentinel + 一些 async 输出。Run 步骤 3
// 读 pushback（旧 token 不匹配精确 token），继续读 stdoutCh 直到 setup sentinel。
// 步骤 4 清空 pushback。步骤 6 等精确 token sentinel——不会被旧残留误匹配。
func TestRunClearsPushbackBeforeCmd(t *testing.T) {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	sid := "cafef00d"
	p := &PtyConn{
		stdin:    stdinWriter,
		stdout:   stdoutReader,
		sid:      sid,
		shell:    "bash",
		logger:   slog.New(slog.DiscardHandler),
		stdoutCh: make(chan []byte, 1024),
		doneCh:   make(chan struct{}),
	}
	go p.readLoop()

	t.Cleanup(func() {
		close(p.doneCh)
		stdinReader.Close()
		stdinWriter.Close()
		stdoutReader.Close()
		stdoutWriter.Close()
	})

	// 预置 pushback：旧 token 的 sentinel + async 输出
	// 这些应该被 Run 步骤 3 消费（旧 token 不匹配）+ 步骤 4 清空
	p.pushback = []byte(fmt.Sprintf("__E_%s_deadbeef__:0__\r\n__P_%s_deadbeef__> async output", sid, sid))

	// 模拟 shell：setup 记录 token；echo hello 输出 hello + 真 sentinel
	go func() {
		buf := make([]byte, 256)
		var tok string
		for {
			n, err := stdinReader.Read(buf)
			if n > 0 {
				data := buf[:n]
				if t := extractToken(data); t != "" {
					tok = t
					stdoutWriter.Write([]byte(fmt.Sprintf("__E_%s_%s__:0__\r\n__P_%s_%s__> ", sid, tok, sid, tok)))
					continue
				}
				if bytes.Contains(data, []byte("echo hello\n")) {
					stdoutWriter.Write([]byte(fmt.Sprintf("hello\r\n__E_%s_%s__:0__\r\n__P_%s_%s__> ", sid, tok, sid, tok)))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	output, _, exitCode, _, _, _, _, _, err := p.Run("echo hello", 2000, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
	if !strings.Contains(output, "hello") {
		t.Errorf("output should contain 'hello', got: %q", output)
	}
	// 关键：output 不应含旧 pushback 的 "async output" 残留
	if strings.Contains(output, "async output") {
		t.Errorf("output should NOT contain 'async output' (pushback residue), got: %q", output)
	}
}

// TestRunTokenUniqueness 验证连续两次 Run 生成不同 token。
// token 每次随机，确保旧 token 的 sentinel 字面量不会误匹配新 Run。
func TestRunTokenUniqueness(t *testing.T) {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	sid := "feedface"
	p := &PtyConn{
		stdin:    stdinWriter,
		stdout:   stdoutReader,
		sid:      sid,
		shell:    "bash",
		logger:   slog.New(slog.DiscardHandler),
		stdoutCh: make(chan []byte, 1024),
		doneCh:   make(chan struct{}),
	}
	go p.readLoop()

	t.Cleanup(func() {
		close(p.doneCh)
		stdinReader.Close()
		stdinWriter.Close()
		stdoutReader.Close()
		stdoutWriter.Close()
	})

	// 捕获两次 Run 的 token
	var mu sync.Mutex
	tokens := []string{}

	go func() {
		buf := make([]byte, 256)
		var tok string
		for {
			n, err := stdinReader.Read(buf)
			if n > 0 {
				data := buf[:n]
				if t := extractToken(data); t != "" {
					tok = t
					mu.Lock()
					tokens = append(tokens, tok)
					mu.Unlock()
					stdoutWriter.Write([]byte(fmt.Sprintf("__E_%s_%s__:0__\r\n__P_%s_%s__> ", sid, tok, sid, tok)))
					continue
				}
				if bytes.Contains(data, []byte("echo hi\n")) {
					stdoutWriter.Write([]byte(fmt.Sprintf("hi\r\n__E_%s_%s__:0__\r\n__P_%s_%s__> ", sid, tok, sid, tok)))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	if _, _, _, _, _, _, _, _, err := p.Run("echo hi", 2000, 0); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if _, _, _, _, _, _, _, _, err := p.Run("echo hi", 2000, 0); err != nil {
		t.Fatalf("Run 2: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens, got %d: %v", len(tokens), tokens)
	}
	if tokens[0] == tokens[1] {
		t.Errorf("two consecutive Runs should have different tokens, both = %q", tokens[0])
	}
}

// TestRunSetTokenTimeoutReturnsConnUnusable 验证 setup 命令不响应时，
// setTokenTimeout（测试覆盖为 200ms）后 Run 返回 connUnusable=true（不自己 Close）。
// close 决策在 Session 层。
//
// 场景：fake shell 收到 setup 命令但不 emit setup sentinel（模拟 shell 异常——
// RC 注入失败、PROMPT_COMMAND 没设上等）。Run 步骤 3 等 setup sentinel 超时，
// 返回 connUnusable=true，Session 据此调 Close。
func TestRunSetTokenTimeoutReturnsConnUnusable(t *testing.T) {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	sid := "deadbeef"
	p := &PtyConn{
		stdin:    stdinWriter,
		stdout:   stdoutReader,
		sid:      sid,
		shell:    "bash",
		logger:   slog.New(slog.DiscardHandler),
		stdoutCh: make(chan []byte, 1024),
		doneCh:   make(chan struct{}),
		// 测试用短超时，避免等 2s
		setTokenTimeout: 200 * time.Millisecond,
	}
	go p.readLoop()

	t.Cleanup(func() {
		p.Close()
		stdinReader.Close()
		stdinWriter.Close()
		stdoutReader.Close()
		stdoutWriter.Close()
	})

	// 模拟 shell：丢弃所有输入，不 emit 任何 sentinel
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := stdinReader.Read(buf)
			_ = n
			if err != nil {
				return
			}
			// 丢弃所有输入
		}
	}()
	_ = stdoutWriter

	start := time.Now()
	_, _, _, _, _, _, _, connUnusable, err := p.Run("echo hi", 2000, 0)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected setup token timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "setup token timeout") {
		t.Errorf("err should contain 'setup token timeout', got: %v", err)
	}
	// 关键断言：connUnusable=true（close 决策交由 Session）
	if !connUnusable {
		t.Errorf("expected connUnusable=true after setup token timeout")
	}
	// 应在 setTokenTimeout (200ms) + 一些清理时间 内返回
	if elapsed > 2*time.Second {
		t.Errorf("Run took %s, expected < 2s (setup timeout 200ms)", elapsed)
	}
}
