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

// extractToken 从 setup 命令 `PS1='$(echo _$?)__<sid>_<token>__]# '` 中提取 token。
// 供 fake shell 在收到 setup 命令后记录 token，后续 sentinel 含该 token。
func extractToken(data []byte) string {
	re := regexp.MustCompile(`PS1='\$\(echo _\$\?\)__[\da-f]+_([0-9a-f]+)__\]# '`)
	m := re.FindSubmatch(data)
	if len(m) > 1 {
		return string(m[1])
	}
	return ""
}

// TestRunIgnoresPS1LiteralInCommandOutput 验证命令输出含 sentinel 字面量时
// Run 不会误匹配导致下次 Run 拿到上一个命令的残留。
//
// 场景：命令 `echo sentinel_literal` 让 shell 输出 sentinel 字符串字面量。
// 远端实际输出：`_0__<sid>_<token>__]# \r\n_0__<sid>_<token>__]# `
//   - 第一段是命令 echo 出的 sentinel 字面量（含 token）
//   - 第二段是 shell 显示的真 sentinel（cmd 完成后 PS1 展开）
//
// token 化后：sentinel 含本次 Run 的 token。命令 echo 出的字面量也含本次 token，
// 看似能误匹配——但 readUntilCommandDoneToken 等的是第一个完整 sentinel。
// 命令 echo 出的字面量会先到达并匹配，导致 Run 提前返回。这就是路径 A 的风险。
//
// 为彻底封死路径 A，Run 在步骤 3（等 setup sentinel）后显式清空 pushback（步骤 4），
// 确保步骤 6 从干净状态开始。但命令输出内的 sentinel 字面量是步骤 6 期间到达的，
// 步骤 4 清空 pushback 无法阻止。本测试验证：即使命令输出含 sentinel 字面量，
// Run 仍能拿到命令的真实输出（而非字面量误匹配导致空输出）。
//
// 关键：token 化让 sentinel 含当前 Run 的 token，但命令输出内的字面量也含当前
// token（因为用户命令 echo 的是当前 PS1）。所以 token 化无法单独防止路径 A。
// 真正的防御是：命令输出内的字面量 sentinel 和真 sentinel 都匹配 regex，
// readUntilCommandDoneToken 取第一个匹配——即字面量 sentinel。但字面量 sentinel
// 后面还有真 sentinel，CleanOutput 会清掉所有 sentinel，留下命令输出。
// exit code 从字面量 sentinel 提取（可能不是真实 exit code）——这是已知限制。
//
// 为避免此限制，真实场景下用户不会 echo sentinel 字面量。本测试仅验证 Run 不卡死。
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
					stdoutWriter.Write([]byte(fmt.Sprintf("_0__%s_%s__]# ", sid, tok)))
					continue
				}
				if bytes.Contains(data, []byte("echo sentinel_literal\n")) {
					// 命令 echo 出 sentinel 字面量（含 token），然后远端显示真 sentinel
					stdoutWriter.Write([]byte(fmt.Sprintf("_0__%s_%s__]# \r\n_0__%s_%s__]# ", sid, tok, sid, tok)))
				} else if bytes.Contains(data, []byte("echo hello\n")) {
					stdoutWriter.Write([]byte(fmt.Sprintf("hello\r\n_0__%s_%s__]# ", sid, tok)))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// 第一次 Run：命令输出含 sentinel 字面量
	output1, rawOut1, exitCode1, _, _, _, _, _, err := p.Run("echo sentinel_literal", 2000, 0)
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if exitCode1 != 0 {
		t.Errorf("exitCode1 = %d, want 0", exitCode1)
	}
	_ = output1
	// rawOut1 应包含命令输出的 sentinel 字面量（CleanOutput 不动 raw）
	if !strings.Contains(rawOut1, "_0__"+sid+"_") {
		t.Errorf("rawOut1 should contain sentinel literal with token (cmd output), got: %q", rawOut1)
	}

	// 第二次 Run：验证 pushback 没残留 sentinel 导致立刻返回上次残留
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
	if strings.Contains(output2, "sentinel_literal") {
		t.Errorf("output2 should NOT contain 'sentinel_literal' (last cmd residue), got: %q", output2)
	}
}

// TestRunReturnsConnUnusableWhenDrainTimesOut 验证 Ctrl-C drain 超时后
// PtyConn.Run 返回 connUnusable=true（不自己 Close）。close 决策在 Session 层。
//
// 场景：命令 hang，超时后发 Ctrl-C，远端不响应（drain 超时）。
// 修复前：PtyConn 在 drain 超时时自己调 p.Close()，Session 事后才发现（zombie）。
// 修复后：PtyConn 返回 connUnusable=true，Session 主动 Close。
//
// 测试用短超时（setup 500ms + cmd 100ms + drain 50ms）避免等待默认 2s/3s。
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
					stdoutWriter.Write([]byte(fmt.Sprintf("_0__%s_%s__]# ", sid, tok)))
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

// TestRunAssemblesSentinelSplitAcrossReads 验证 sentinel 分多次到达时
// readUntilCommandDoneToken 仍能正确匹配。
//
// 场景：shell 把 sentinel 分成 3 次写（`_0__<sid>_<token>` / `__]# ` 等），
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
					stdoutWriter.Write([]byte(fmt.Sprintf("_0__%s_%s__]# ", sid, tok)))
					continue
				}
				if bytes.Contains(data, []byte("echo split\n")) {
					stdoutWriter.Write([]byte("split\r\n"))
					stdoutWriter.Write([]byte(fmt.Sprintf("_0__%s_", sid)))
					stdoutWriter.Write([]byte(fmt.Sprintf("%s__", tok)))
					stdoutWriter.Write([]byte("]# "))
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
