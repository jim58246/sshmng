// Package loginflow 实现 LoginFlow 决策树执行器：在 PTY 上按配置的 Action
// 序列发送字符串、等待输出、匹配 pattern，最终拿到 target shell。
//
// 设计要点：
//   - 纯逻辑层，不直接依赖 SSH；通过 PTY 接口抽象，便于测试用 fake 替身
//   - ANSI 过滤在 expect 匹配前应用（输出含颜色码不影响 pattern 命中）
//   - 失败时返回截至失败点的 trace，供 Agent 诊断
//   - 三层超时保护：单 Action TimeoutMs（默认 10s）、MaxSteps（默认 50）、GlobalTimeoutMs（默认 60s）
package loginflow

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"sshmng/internal/config"
)

// PTY 是执行器依赖的 PTY 抽象，便于测试用 fake 替身。
//
// Read 读取 PTY 输出，直到任意 matcher 命中或 deadline 到达。
// matchers 为空时按静默期 heuristic 返回（用于无 Expects 的 Action，理论上不应出现）。
// 命中时返回截至 match 末尾的 output（含 ANSI，供 trace 记录原始数据）；
// trailing data 由实现内部 pushback 保留，下次 Read 优先消费——这点对 Pattern B
// 两段式 LoginFlow 至关重要：第一段流的最后一次 Read 不能吞掉第二段流要等的 prompt。
//
// 返回 (output, matchedIdx, timedOut, err)。matchedIdx=-1 表示未命中
// （matchers 为空的静默期返回，或 deadline 到达的超时返回）。
type PTY interface {
	Send(s string) error
	Read(deadline time.Time, matchers []*regexp.Regexp) (output string, matchedIdx int, timedOut bool, err error)
}

// TraceEntry 是单步执行记录，与设计文档 §3.2 trace 结构一致。
type TraceEntry struct {
	Time      string `json:"time"`       // "2026-07-17 14:23:45.000"
	ElapsedMs int    `json:"elapsed_ms"` // 本步耗时
	Send      string `json:"send"`       // 本步发送的内容（空 Send 时为 ""）
	Expect    string `json:"expect"`     // 命中的 pattern；未命中为 ""
	Output    string `json:"output"`     // 本步观测到的 PTY 输出（未清洗）
}

// Options 是 Run 的可选参数；零值使用默认值。
type Options struct {
	MaxSteps       int           // 0 = 50
	GlobalTimeout  time.Duration // 0 = 60s
	DefaultTimeout time.Duration // 0 = 10s（Action.TimeoutMs=0 时用）
}

const (
	defaultTimeoutMs       = 10000
	defaultMaxSteps        = 50
	defaultGlobalTimeoutMs = 60000
	success                = "success"
)

// Run 在 PTY 上执行 LoginFlow 决策树。
//
// 入口为 entry 指向的 LoginAction；每个 Action 顺序：Send（可空）→ Read → 按 Expects
// 顺序匹配 → 命中则跳转 Next（"success" 表示成功）；任何一步失败（无匹配 / 超时 / 步数
// 超限）返回 error + 截至失败点的 trace。
func Run(pty PTY, flow map[string]config.LoginAction, entry string, opts Options) ([]TraceEntry, error) {
	maxSteps := opts.MaxSteps
	if maxSteps == 0 {
		maxSteps = defaultMaxSteps
	}
	globalTimeout := opts.GlobalTimeout
	if globalTimeout == 0 {
		globalTimeout = time.Duration(defaultGlobalTimeoutMs) * time.Millisecond
	}
	defaultTimeout := opts.DefaultTimeout
	if defaultTimeout == 0 {
		defaultTimeout = time.Duration(defaultTimeoutMs) * time.Millisecond
	}

	globalStart := time.Now()
	globalDeadline := globalStart.Add(globalTimeout)
	var trace []TraceEntry
	currentName := entry

	for step := 0; step < maxSteps; step++ {
		if time.Now().After(globalDeadline) {
			return trace, fmt.Errorf("loginflow: global timeout exceeded (%s) at step %d (%q)", globalTimeout, step, currentName)
		}

		action, ok := flow[currentName]
		if !ok {
			return trace, fmt.Errorf("loginflow: action %q not found in flow", currentName)
		}

		stepStart := time.Now()
		if action.Send != "" {
			if err := pty.Send(action.Send); err != nil {
				return trace, fmt.Errorf("loginflow: send at %q: %w", currentName, err)
			}
		}

		// 编译本步 matchers：PTY.Read 命中即停，trailing 留 pushback。
		// 这避免 Pattern B 第二段流等不到 prompt——第一段流的最后一次 Read 不能
		// 把 server 自发输出（如 target 的 "login:"）一并吞掉。
		matchers := make([]*regexp.Regexp, len(action.Expects))
		for i, exp := range action.Expects {
			re, err := compilePattern(exp.Pattern)
			if err != nil {
				return trace, fmt.Errorf("loginflow: action %q: invalid pattern %q: %w", currentName, exp.Pattern, err)
			}
			matchers[i] = re
		}

		timeout := defaultTimeout
		if action.TimeoutMs > 0 {
			timeout = time.Duration(action.TimeoutMs) * time.Millisecond
		}
		deadline := stepStart.Add(timeout)
		if deadline.After(globalDeadline) {
			deadline = globalDeadline
		}

		output, matchedIdx, timedOut, err := pty.Read(deadline, matchers)
		if err != nil {
			return trace, fmt.Errorf("loginflow: read at %q: %w", currentName, err)
		}

		matchedPattern := ""
		next := ""
		if matchedIdx >= 0 {
			matchedPattern = action.Expects[matchedIdx].Pattern
			next = action.Expects[matchedIdx].Next
		}

		trace = append(trace, TraceEntry{
			Time:      stepStart.Format("2006-01-02 15:04:05.000"),
			ElapsedMs: int(time.Since(stepStart).Milliseconds()),
			Send:      action.Send,
			Expect:    matchedPattern,
			Output:    output,
		})

		if timedOut {
			return trace, fmt.Errorf("loginflow: action %q timed out after %s", currentName, timeout)
		}
		if matchedIdx < 0 {
			return trace, fmt.Errorf("loginflow: action %q: no expect matched (output: %q)", currentName, truncateForMsg(output, 200))
		}
		if next == success {
			return trace, nil
		}
		currentName = next
	}

	return trace, fmt.Errorf("loginflow: max steps (%d) exceeded", maxSteps)
}

// compilePattern 把 LoginFlow pattern 编译成正则：
//   - "re:" 前缀：去掉前缀后直接编译
//   - 无前缀：glob → 正则（contains 语义，未锚定）
//
// "contains" 而非 "full match"：PTY 输出常带前导 \r\n / MOTD / 颜色码残留，
// 用户写 "login:*" 期望匹配 "...login: " 中的 "login: " 子串，而非要求整行以 "login:" 开头。
func compilePattern(pattern string) (*regexp.Regexp, error) {
	if strings.HasPrefix(pattern, "re:") {
		return regexp.Compile(pattern[3:])
	}
	return globToRegex(pattern)
}

// globToRegex 把 glob pattern 转成未锚定正则（contains 语义）。
// 支持：*（任意序列含 \n）、?（单字符）、[...]（字符类）、其他字符字面量。
func globToRegex(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '.', '+', '(', ')', '|', '{', '}', '^', '$', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '[':
			// 字符类：原样复制到 ']'（简化处理，不深究 POSIX 类语法）
			b.WriteByte('[')
			for i+1 < len(pattern) && pattern[i+1] != ']' {
				i++
				b.WriteByte(pattern[i])
			}
			if i+1 < len(pattern) {
				i++
				b.WriteByte(']')
			}
		default:
			b.WriteByte(c)
		}
	}
	return regexp.Compile(b.String())
}

// ansiRe 匹配 ANSI CSI 与 OSC 序列：
//   - CSI: ESC [ + 参数字节 + 终止字节（颜色 / 光标 / 清屏）
//   - OSC: ESC ] + 内容 + 终止符（BEL \x07 或 ST = ESC \）。bash/zsh 设置窗口标题走 OSC。
//
// OSC 必须剥离，否则窗口标题序列会让 anchored LoginFlow pattern 失败。sentinel 字面量
// 是纯 ASCII，不会受影响。与 internal/ssh/normalize.go 同源，独立维护以避免 loginflow → ssh
// 的循环依赖。
var ansiRe = regexp.MustCompile("\x1b(?:\\[[0-9;?]*[A-Za-z]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\))")

// stripANSI 剥离 ANSI CSI / OSC 转义序列。sentinel 字面量是纯 ASCII，不会受影响。
func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// truncateForMsg 把字符串截断到 maxLen 用于错误信息；超长加省略号。
func truncateForMsg(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
