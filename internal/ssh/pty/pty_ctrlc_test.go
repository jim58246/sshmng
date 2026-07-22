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

// TestRunSendsCtrlCOnTimeout 验证 Run 超时后自动发送 Ctrl-C 中断远程命令，
// 并 drain 残留输出（exit sentinel + 新 PS1）使下次 Run 从干净状态开始。
//
// 场景：命令 "hang-cmd" 不产生任何输出（模拟 hang），Run 在 100ms 后超时。
// 修复前：Run 直接返回 timedOut=true，远程命令仍在跑，下次 Run 会卡。
// 修复后：Run 发送 \x03 到 stdin，drain 等待新 PS1，返回时 exit code = 130 (SIGINT)。
//
// token 化后：fake shell 需先响应 setup 命令（记录 token，emit setup sentinel），
// 再在收到 Ctrl-C 时 emit 含 token 的 exit sentinel + PS1。
func TestRunSendsCtrlCOnTimeout(t *testing.T) {
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
	}
	go p.readLoop()

	t.Cleanup(func() {
		close(p.doneCh)
		stdinReader.Close()
		stdinWriter.Close()
		stdoutReader.Close()
		stdoutWriter.Close()
	})

	// 读 stdin：捕获 Run 写入的字节，收到 Ctrl-C 时模拟 shell 恢复（写 exit sentinel + PS1）
	var stdinBuf bytes.Buffer
	var stdinMu sync.Mutex
	var tok string
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := stdinReader.Read(buf)
			if n > 0 {
				data := buf[:n]
				stdinMu.Lock()
				stdinBuf.Write(data)
				stdinMu.Unlock()
				// setup 命令：记录 token，emit setup sentinel
				if t := extractToken(data); t != "" {
					tok = t
					stdoutWriter.Write([]byte(fmt.Sprintf("_0__%s_%s__]# ", sid, tok)))
					continue
				}
				if bytes.Contains(data, []byte{0x03}) {
					// 模拟 shell 收到 SIGINT 后恢复：发送含 token 的 sentinel（exit code 130 = SIGINT）
					stdoutWriter.Write([]byte(fmt.Sprintf("_130__%s_%s__]# ", sid, tok)))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	start := time.Now()
	output, _, exitCode, timedOut, _, _, _, _, err := p.Run("hang-cmd", 100, 0)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !timedOut {
		t.Errorf("expected timedOut=true, got false")
	}
	// 应在 setup + 100ms 超时 + drain 时间（~即时）内返回
	if elapsed > 2*time.Second {
		t.Errorf("Run took %s, expected < 2s (100ms timeout + quick drain)", elapsed)
	}

	// 验证 Ctrl-C 被发送到 stdin
	stdinMu.Lock()
	stdinData := stdinBuf.String()
	stdinMu.Unlock()
	if !strings.Contains(stdinData, "\x03") {
		t.Errorf("expected stdin to contain Ctrl-C (\\x03), got: %q", stdinData)
	}

	// 验证 drain 捕获了 exit sentinel — exit code 应为 130 (SIGINT)
	if exitCode != 130 {
		t.Errorf("exitCode = %d, want 130 (SIGINT from Ctrl-C)", exitCode)
	}

	_ = output
}

// TestRunReturnsCtrlCSentOnTimeout 验证 Run 超时发送 Ctrl-C 后返回 ctrlCSent=true。
// 正常完成时返回 ctrlCSent=false。供 RunInSession 记入 CommandTrace 供 get_trace 诊断。
func TestRunReturnsCtrlCSentOnTimeout(t *testing.T) {
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

	var tok string
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := stdinReader.Read(buf)
			if n > 0 {
				data := buf[:n]
				if t := extractToken(data); t != "" {
					tok = t
					stdoutWriter.Write([]byte(fmt.Sprintf("_0__%s_%s__]# ", sid, tok)))
					continue
				}
				if bytes.Contains(data, []byte{0x03}) {
					stdoutWriter.Write([]byte(fmt.Sprintf("_130__%s_%s__]# ", sid, tok)))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	_, _, _, timedOut, ctrlCSent, _, _, _, err := p.Run("hang-cmd", 100, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !timedOut {
		t.Errorf("expected timedOut=true, got false")
	}
	if !ctrlCSent {
		t.Errorf("expected ctrlCSent=true on timeout, got false")
	}
}

// TestRunReturnsCtrlCSentFalseOnSuccess 验证命令正常完成时 ctrlCSent=false。
func TestRunReturnsCtrlCSentFalseOnSuccess(t *testing.T) {
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
	}
	go p.readLoop()

	t.Cleanup(func() {
		close(p.doneCh)
		stdinReader.Close()
		stdinWriter.Close()
		stdoutReader.Close()
		stdoutWriter.Close()
	})

	var tok string
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := stdinReader.Read(buf)
			if n > 0 {
				data := buf[:n]
				if t := extractToken(data); t != "" {
					tok = t
					stdoutWriter.Write([]byte(fmt.Sprintf("_0__%s_%s__]# ", sid, tok)))
					continue
				}
				if bytes.Contains(data, []byte("echo hi\n")) {
					stdoutWriter.Write([]byte(fmt.Sprintf("hi\r\n_0__%s_%s__]# ", sid, tok)))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	_, _, _, timedOut, ctrlCSent, _, _, _, err := p.Run("echo hi", 2000, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if timedOut {
		t.Errorf("should not time out for echo hi")
	}
	if ctrlCSent {
		t.Errorf("expected ctrlCSent=false on success, got true")
	}
}

// TestRunCtrlCNotSentOnSuccess 验证命令正常完成时不发 Ctrl-C。
func TestRunCtrlCNotSentOnSuccess(t *testing.T) {
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

	var stdinBuf bytes.Buffer
	var stdinMu sync.Mutex
	var tok string
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := stdinReader.Read(buf)
			if n > 0 {
				data := buf[:n]
				stdinMu.Lock()
				stdinBuf.Write(data)
				stdinMu.Unlock()
				// setup 命令：记录 token，emit setup sentinel
				if t := extractToken(data); t != "" {
					tok = t
					stdoutWriter.Write([]byte(fmt.Sprintf("_0__%s_%s__]# ", sid, tok)))
					continue
				}
				// 模拟 shell：收到命令后输出 hello + 含 token 的 sentinel
				if bytes.Contains(data, []byte("echo hello\n")) {
					stdoutWriter.Write([]byte(fmt.Sprintf("hello\r\n_0__%s_%s__]# ", sid, tok)))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	output, _, exitCode, timedOut, _, _, _, _, err := p.Run("echo hello", 2000, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if timedOut {
		t.Errorf("should not time out for echo hello")
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
	if !strings.Contains(output, "hello") {
		t.Errorf("output should contain 'hello', got: %q", output)
	}

	// 验证没有发 Ctrl-C
	stdinMu.Lock()
	stdinData := stdinBuf.String()
	stdinMu.Unlock()
	if strings.Contains(stdinData, "\x03") {
		t.Errorf("stdin should NOT contain Ctrl-C on successful run, got: %q", stdinData)
	}
}
