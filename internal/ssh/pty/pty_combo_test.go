package pty

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"
)

// extractToken 从 setup 命令 `__sshmng_tok=<token>; ...` 中提取 token。
// 供 fake shell 在收到 setup 命令后记录 token，后续 sentinel 含该 token。
func extractToken(data []byte) string {
	re := regexp.MustCompile(`__sshmng_tok=([0-9a-f]+)`)
	m := re.FindSubmatch(data)
	if len(m) > 1 {
		return string(m[1])
	}
	return ""
}

// TestRunIgnoresPS1LiteralInCommandOutput 验证命令输出含 PS1 字面量时
// Run 不会误匹配导致下次 Run 拿到上一个命令的残留。
//
// 场景：命令 `echo ps1_literal` 让 shell 输出 PS1 字符串字面量（如 `echo $PS1`）。
// 远端实际输出：`__P_<sid>_<token>__> \r\n__E_<sid>_<token>__:0__\r\n__P_<sid>_<token>__> `
//   - 第一段 `__P_<sid>_<token>__> ` 是命令 echo 出的 PS1 字面量（含 token）
//   - 第二段 `__E_<sid>_<token>__:0__\r\n__P_<sid>_<token>__> ` 是 PROMPT_COMMAND 输出的 exit sentinel + 真 PS1
//
// token 化后：sentinel 含本次 Run 的 token，命令 echo 出的字面量含本次 token 也
// 不会误匹配——因为 readUntilCommandDoneToken 等的是 combo sentinel（exit+PS1 连续），
// 单独 PS1 字面量前面没有 exit sentinel，不匹配 combo。
func TestRunIgnoresPS1LiteralInCommandOutput(t *testing.T) {
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

	// 模拟 shell：根据收到的命令输出不同响应
	go func() {
		buf := make([]byte, 256)
		var tok string
		for {
			n, err := stdinReader.Read(buf)
			if n > 0 {
				data := buf[:n]
				// setup 命令：记录 token，emit setup sentinel
				if t := extractToken(data); t != "" {
					tok = t
					stdoutWriter.Write([]byte(fmt.Sprintf("__E_%s_%s__:0__\r\n__P_%s_%s__> ", sid, tok, sid, tok)))
					continue
				}
				if bytes.Contains(data, []byte("echo ps1_literal\n")) {
					// 命令 echo 出 PS1 字面量（含 token），然后远端显示真 sentinel
					stdoutWriter.Write([]byte(fmt.Sprintf("__P_%s_%s__> \r\n__E_%s_%s__:0__\r\n__P_%s_%s__> ", sid, tok, sid, tok, sid, tok)))
				} else if bytes.Contains(data, []byte("echo hello\n")) {
					stdoutWriter.Write([]byte(fmt.Sprintf("hello\r\n__E_%s_%s__:0__\r\n__P_%s_%s__> ", sid, tok, sid, tok)))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// 第一次 Run：命令输出含 PS1 字面量
	// rawOut1 保留命令的真实输出（含 PS1 字面量）；output1 被 CleanOutput 清掉 PS1 残留。
	output1, rawOut1, exitCode1, _, _, _, _, _, err := p.Run("echo ps1_literal", 2000, 0)
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if exitCode1 != 0 {
		t.Errorf("exitCode1 = %d, want 0 (true exit code, not -1 from missing sentinel)", exitCode1)
	}
	_ = output1 // CleanOutput 会清掉 PS1 字面量，不可断言
	// rawOut1 应包含命令输出的 PS1 字面量（CleanOutput 不动 raw）
	if !strings.Contains(rawOut1, "__P_"+sid+"_") {
		t.Errorf("rawOut1 should contain PS1 literal with token (cmd output), got: %q", rawOut1)
	}

	// 第二次 Run：验证 pushback 没残留 PS1 导致立刻返回上次残留
	output2, _, exitCode2, _, _, _, _, _, err := p.Run("echo hello", 2000, 0)
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if exitCode2 != 0 {
		t.Errorf("exitCode2 = %d, want 0", exitCode2)
	}
	if !strings.Contains(output2, "hello") {
		t.Errorf("output2 should contain 'hello', got: %q", output2)
	}
	// 关键：output2 不应包含上次命令的残留
	if strings.Contains(output2, "ps1_literal") {
		t.Errorf("output2 should NOT contain 'ps1_literal' (last cmd residue), got: %q", output2)
	}
}

// TestRunReturnsConnUnusableWhenDrainTimesOut 验证 Ctrl-C drain 超时后
// PtyConn.Run 返回 connUnusable=true（不自己 Close）。close 决策在 Session 层。
//
// 场景：命令 hang，超时后发 Ctrl-C，远端不响应（drain 超时）。
// 修复前：PtyConn 在 drain 超时时自己调 p.Close()，Session 事后才发现（zombie）。
// 修复后：PtyConn 返回 connUnusable=true，Session 主动 Close。
//
// 测试用短超时（setup 50ms + cmd 100ms + drain 50ms）避免等待默认 2s/3s。
// fake shell 收到 setup 命令后正常 emit setup sentinel（让 setup 步骤通过），
// 但收到 cmd 后不响应（模拟卡死 + Ctrl-C 失效）。
func TestRunReturnsConnUnusableWhenDrainTimesOut(t *testing.T) {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	sid := "cafef00d"
	p := &PtyConn{
		stdin:             stdinWriter,
		stdout:            stdoutReader,
		sid:               sid,
		shell:             "bash",
		logger:            slog.New(slog.DiscardHandler),
		stdoutCh:          make(chan []byte, 1024),
		doneCh:            make(chan struct{}),
		ctrlCDrainTimeout: 50 * time.Millisecond, // 测试用短超时
		setTokenTimeout:   500 * time.Millisecond,
	}
	go p.readLoop()

	t.Cleanup(func() {
		p.Close()
		stdinReader.Close()
		stdinWriter.Close()
		stdoutReader.Close()
		stdoutWriter.Close()
	})

	// 模拟 shell：setup 命令正常响应；后续命令卡死不响应
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
				// setup 后的命令（hang-cmd）+ Ctrl-C：丢弃不响应
			}
			if err != nil {
				return
			}
		}
	}()

	// Run 应在 cmd timeout + drain timeout 后返回
	start := time.Now()
	_, _, _, timedOut, _, _, _, connUnusable, err := p.Run("hang-cmd", 100, 0) // cmd 100ms 超时
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !timedOut {
		t.Errorf("expected timedOut=true")
	}
	// 关键断言：connUnusable=true（close 决策交由 Session）
	if !connUnusable {
		t.Errorf("expected connUnusable=true after drain timeout")
	}
	// 应在 setup(<500ms) + 100ms cmd + 50ms drain + 一些清理时间 内返回
	if elapsed > 2*time.Second {
		t.Errorf("Run took %s, expected < 2s", elapsed)
	}
}

// TestRunAssemblesSentinelSplitAcrossReads 验证 combo sentinel 分多次到达时
// readUntilCommandDoneToken 仍能正确匹配。
//
// 场景：shell 把 sentinel 分成 3 次写（`__E_<sid>_<token>__:` / `0__\r\n` / `__P_<sid>_<token>__> `），
// 模拟 PTY 流因网络/缓冲分片。readLoop 每次读到一块就投递到 stdoutCh，
// readUntilCommandDoneToken 累积 buf 后再跑正则。
//
// 修复前/后：正则一直对累积 buf 跑，分片不影响结果——这是回归测试，确保未来重构
// readUntilCommandDoneToken 不破坏分片处理。
func TestRunAssemblesSentinelSplitAcrossReads(t *testing.T) {
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

	// 模拟 shell：setup 正常响应；echo split 命令分 3 次写 sentinel
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
				if bytes.Contains(data, []byte("echo split\n")) {
					stdoutWriter.Write([]byte("split\r\n"))
					stdoutWriter.Write([]byte(fmt.Sprintf("__E_%s_%s__:", sid, tok)))
					stdoutWriter.Write([]byte(fmt.Sprintf("0__\r\n")))
					stdoutWriter.Write([]byte(fmt.Sprintf("__P_%s_%s__> ", sid, tok)))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	output, _, exitCode, _, _, _, _, _, err := p.Run("echo split", 2000, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0 (sentinel should assemble from pieces)", exitCode)
	}
	if !strings.Contains(output, "split") {
		t.Errorf("output should contain 'split', got: %q", output)
	}
}
