package loginflow

import (
	"strings"
	"testing"
	"time"

	"sshmng/internal/config"
)

// fakePTY 是测试用的 PTY 替身：Send 累积输入；Read 返回已队列化的输出。
// 命中 mustContain 时 timedOut=false；否则 timedOut=true（模拟等不到 pattern）。
// forceTimeout=true 时 Read 总是返回 timedOut=true，用于测超时分支。
type fakePTY struct {
	sent         []string
	queuedOut    string
	forceTimeout bool
}

func (f *fakePTY) Send(s string) error {
	f.sent = append(f.sent, s)
	return nil
}

func (f *fakePTY) Read(deadline time.Time, mustContain string) (string, bool, error) {
	out := f.queuedOut
	f.queuedOut = ""
	if f.forceTimeout {
		return out, true, nil
	}
	timedOut := mustContain != "" && !strings.Contains(out, mustContain)
	return out, timedOut, nil
}

// queueOut 把一段输出加入队列，供下次 Read 返回。
func (f *fakePTY) queueOut(s string) { f.queuedOut += s }

// --- happy path ---

// TestRunHappyPathSingleAction：单 Action 发送 "u\n"，期望 "Welcome*" 命中，
// Next="success" → 登录成功，trace 含一条 entry。
func TestRunHappyPathSingleAction(t *testing.T) {
	pty := &fakePTY{}
	pty.queueOut("Welcome to server")
	flow := map[string]config.LoginAction{
		"entry": {
			Name:    "entry",
			Send:    "u\n",
			Expects: []config.Expect{{Pattern: "Welcome*", Next: "success"}},
		},
	}

	trace, err := Run(pty, flow, "entry", Options{})
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if len(trace) != 1 {
		t.Fatalf("trace length = %d, want 1", len(trace))
	}
	entry := trace[0]
	if entry.Send != "u\n" {
		t.Errorf("Send = %q, want %q", entry.Send, "u\n")
	}
	if entry.Expect != "Welcome*" {
		t.Errorf("Expect = %q, want %q", entry.Expect, "Welcome*")
	}
	if entry.Output != "Welcome to server" {
		t.Errorf("Output = %q, want %q", entry.Output, "Welcome to server")
	}
	if len(pty.sent) != 1 || pty.sent[0] != "u\n" {
		t.Errorf("pty.sent = %v, want [u\\n]", pty.sent)
	}
}

// --- 空 Send 跳过发送 ---

// TestRunEmptySendSkipsSend：入口 Action Send 为空时不调 pty.Send，直接 Read + expect。
// 典型场景：等远端 MOTD / 菜单输出。
func TestRunEmptySendSkipsSend(t *testing.T) {
	pty := &fakePTY{}
	pty.queueOut("Welcome MOTD")
	flow := map[string]config.LoginAction{
		"entry": {
			Name:    "entry",
			Send:    "", // 空 Send
			Expects: []config.Expect{{Pattern: "Welcome*", Next: "success"}},
		},
	}

	trace, err := Run(pty, flow, "entry", Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(pty.sent) != 0 {
		t.Errorf("pty.sent should be empty, got %v", pty.sent)
	}
	if trace[0].Send != "" {
		t.Errorf("trace[0].Send = %q, want empty", trace[0].Send)
	}
}

// --- 多 Expect 按顺序匹配第一个 ---

// TestRunMultipleExpectsFirstMatchWins：多个 Expects 中第一个命中的决定 Next，
// 即使后面的 pattern 也能匹配。
func TestRunMultipleExpectsFirstMatchWins(t *testing.T) {
	pty := &fakePTY{}
	pty.queueOut("menu: choice 1")
	flow := map[string]config.LoginAction{
		"entry": {
			Name: "entry",
			Send: "\n",
			Expects: []config.Expect{
				{Pattern: "menu*", Next: "got_menu"},
				{Pattern: "choice*", Next: "got_choice"}, // 也能匹配但不应被选
			},
		},
		"got_menu":   {Name: "got_menu", Expects: []config.Expect{{Pattern: "*", Next: "success"}}},
		"got_choice": {Name: "got_choice", Expects: []config.Expect{{Pattern: "*", Next: "success"}}},
	}

	trace, err := Run(pty, flow, "entry", Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trace) != 2 {
		t.Fatalf("trace length = %d, want 2 (entry + got_menu)", len(trace))
	}
	if trace[0].Expect != "menu*" {
		t.Errorf("trace[0].Expect = %q, want %q", trace[0].Expect, "menu*")
	}
}

// --- 所有 Expects 未命中 → 失败 ---

// TestRunNoExpectMatchFailsWithTrace：所有 pattern 都不命中时返回 error，
// trace 含本步 entry（Expect=""，记录 Output 供诊断）。
func TestRunNoExpectMatchFailsWithTrace(t *testing.T) {
	pty := &fakePTY{}
	pty.queueOut("unexpected output")
	flow := map[string]config.LoginAction{
		"entry": {
			Name: "entry",
			Send: "\n",
			Expects: []config.Expect{
				{Pattern: "Welcome*", Next: "success"},
				{Pattern: "Login:*", Next: "success"},
			},
		},
	}

	trace, err := Run(pty, flow, "entry", Options{})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no expect matched") {
		t.Errorf("err = %q, want contains 'no expect matched'", err.Error())
	}
	if len(trace) != 1 {
		t.Fatalf("trace length = %d, want 1", len(trace))
	}
	if trace[0].Expect != "" {
		t.Errorf("Expect = %q, want empty (no match)", trace[0].Expect)
	}
	if trace[0].Output != "unexpected output" {
		t.Errorf("Output = %q, want %q", trace[0].Output, "unexpected output")
	}
}

// --- 单 Action 超时 → 失败 ---

// TestRunActionTimeoutFails：PTY Read 返回 timedOut=true 时，Run 报超时错误，
// trace 仍记录本步的 output（可能为空或部分）。
func TestRunActionTimeoutFails(t *testing.T) {
	pty := &fakePTY{forceTimeout: true}
	pty.queueOut("partial output")
	flow := map[string]config.LoginAction{
		"entry": {
			Name:      "entry",
			Send:      "\n",
			TimeoutMs: 100,
			Expects:   []config.Expect{{Pattern: "Welcome*", Next: "success"}},
		},
	}

	trace, err := Run(pty, flow, "entry", Options{})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %q, want contains 'timed out'", err.Error())
	}
	if len(trace) != 1 {
		t.Fatalf("trace length = %d, want 1", len(trace))
	}
	if trace[0].Output != "partial output" {
		t.Errorf("Output = %q, want %q", trace[0].Output, "partial output")
	}
}

// --- MaxSteps 超限 ---

// TestRunMaxStepsExceeded：循环 flow（A→B→A→...）不达 success，
// 超过 MaxSteps 时报错。
func TestRunMaxStepsExceeded(t *testing.T) {
	pty := &fakePTY{}
	pty.queueOut("loop output") // 单次 queueOut 只供一次 Read；后续 Read 拿空字符串
	// 让每次 Read 都有内容：用 Send 触发 queueOut
	// 简化：每次 Send 时把 output 重新排队
	pty2 := &loopPTY{}
	flow := map[string]config.LoginAction{
		"a": {Name: "a", Send: "a\n", Expects: []config.Expect{{Pattern: "*", Next: "b"}}},
		"b": {Name: "b", Send: "b\n", Expects: []config.Expect{{Pattern: "*", Next: "a"}}},
	}

	trace, err := Run(pty2, flow, "a", Options{MaxSteps: 3})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "max steps") {
		t.Errorf("err = %q, want contains 'max steps'", err.Error())
	}
	if len(trace) != 3 {
		t.Errorf("trace length = %d, want 3 (MaxSteps=3)", len(trace))
	}
}

// loopPTY 每次 Send 后自动 queueOut 一段内容，使 Read 总有数据匹配 "*"。
type loopPTY struct{ sent []string }

func (p *loopPTY) Send(s string) error {
	p.sent = append(p.sent, s)
	return nil
}
func (p *loopPTY) Read(deadline time.Time, mustContain string) (string, bool, error) {
	return "loop output", false, nil
}

// --- GlobalTimeout 超限 ---

// TestRunGlobalTimeoutExceeded：循环 flow + 极小 GlobalTimeout → 第一步就报超时。
func TestRunGlobalTimeoutExceeded(t *testing.T) {
	pty := &loopPTY{}
	flow := map[string]config.LoginAction{
		"a": {Name: "a", Send: "a\n", Expects: []config.Expect{{Pattern: "*", Next: "b"}}},
		"b": {Name: "b", Send: "b\n", Expects: []config.Expect{{Pattern: "*", Next: "a"}}},
	}

	// GlobalTimeout=1ns：globalDeadline 在过去，首次循环 check 即触发
	trace, err := Run(pty, flow, "a", Options{MaxSteps: 1000000, GlobalTimeout: 1 * time.Nanosecond})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "global timeout") {
		t.Errorf("err = %q, want contains 'global timeout'", err.Error())
	}
	if len(trace) != 0 {
		t.Errorf("trace length = %d, want 0 (timeout fires before any work)", len(trace))
	}
}

// --- glob pattern 匹配 ---

// TestRunGlobPatternMatch：无前缀 pattern 用 glob 语义（filepath.Match）。
func TestRunGlobPatternMatch(t *testing.T) {
	pty := &fakePTY{}
	pty.queueOut("Please select:\n  1) prod-db\n  2) prod-web\nYour choice: ")
	flow := map[string]config.LoginAction{
		"entry": {
			Name:    "entry",
			Expects: []config.Expect{{Pattern: "Please select*", Next: "success"}},
		},
	}

	_, err := Run(pty, flow, "entry", Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// --- re: 前缀正则匹配 ---

// TestRunRegexPatternMatch：`re:` 前缀用正则匹配。
func TestRunRegexPatternMatch(t *testing.T) {
	pty := &fakePTY{}
	pty.queueOut("Permission denied (publickey)")
	flow := map[string]config.LoginAction{
		"entry": {
			Name:    "entry",
			Expects: []config.Expect{{Pattern: "re:^Permission denied \\(.*\\)$", Next: "success"}},
		},
	}

	_, err := Run(pty, flow, "entry", Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// --- ANSI 过滤在匹配前应用 ---

// TestRunANSIFilterBeforeMatch：output 含 ANSI 颜色码时不影响 pattern 匹配。
func TestRunANSIFilterBeforeMatch(t *testing.T) {
	pty := &fakePTY{}
	pty.queueOut("\x1b[0;31mWelcome\x1b[0m to server")
	flow := map[string]config.LoginAction{
		"entry": {
			Name:    "entry",
			Expects: []config.Expect{{Pattern: "Welcome to server", Next: "success"}},
		},
	}

	trace, err := Run(pty, flow, "entry", Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 原始 output（含 ANSI）记入 trace；匹配在 stripANSI 之后
	if trace[0].Output != "\x1b[0;31mWelcome\x1b[0m to server" {
		t.Errorf("Output should preserve ANSI for trace, got %q", trace[0].Output)
	}
	if trace[0].Expect != "Welcome to server" {
		t.Errorf("Expect = %q, want %q (matched after ANSI strip)", trace[0].Expect, "Welcome to server")
	}
}

// --- trace 结构 ---

// TestRunTraceStructure：多步 flow 的 trace 字段（Time / ElapsedMs / Send / Expect / Output）结构正确。
func TestRunTraceStructure(t *testing.T) {
	pty := &loopPTY{}
	flow := map[string]config.LoginAction{
		"entry": {
			Name:    "entry",
			Send:    "u\n",
			Expects: []config.Expect{{Pattern: "*", Next: "step2"}},
		},
		"step2": {
			Name:    "step2",
			Send:    "p\n",
			Expects: []config.Expect{{Pattern: "*", Next: "success"}},
		},
	}

	trace, err := Run(pty, flow, "entry", Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trace) != 2 {
		t.Fatalf("trace length = %d, want 2", len(trace))
	}
	for i, e := range trace {
		if e.Time == "" {
			t.Errorf("trace[%d].Time empty", i)
		}
		if _, err := time.Parse("2006-01-02 15:04:05.000", e.Time); err != nil {
			t.Errorf("trace[%d].Time = %q, want format '2006-01-02 15:04:05.000': %v", i, e.Time, err)
		}
		if e.ElapsedMs < 0 {
			t.Errorf("trace[%d].ElapsedMs = %d, want >= 0", i, e.ElapsedMs)
		}
		if e.Expect != "*" {
			t.Errorf("trace[%d].Expect = %q, want %q", i, e.Expect, "*")
		}
		if e.Output != "loop output" {
			t.Errorf("trace[%d].Output = %q, want %q", i, e.Output, "loop output")
		}
	}
	if trace[0].Send != "u\n" {
		t.Errorf("trace[0].Send = %q, want %q", trace[0].Send, "u\n")
	}
	if trace[1].Send != "p\n" {
		t.Errorf("trace[1].Send = %q, want %q", trace[1].Send, "p\n")
	}
}
