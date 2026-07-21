// Package pty 实现 SSH 会话管理工具的 PTY 层：PTY 连接、sentinel 解析、
// 终端规范化、shell 探测。
//
// 本文件（normalize.go）只做纯字符串处理：ANSI 转义剥离、sentinel 行清理、
// 行结束符标准化。无副作用，便于单测。
package pty

import (
	"regexp"
	"strings"
)

// ansiRe 匹配 ANSI CSI 与 OSC 序列：
//   - CSI: ESC [ + 参数字节 + 中间字节 + 终止字节（颜色 / 光标 / 清屏）
//   - OSC: ESC ] + 内容 + 终止符（BEL \x07 或 ST = ESC \）。bash/zsh 设置窗口标题走 OSC。
//
// OSC 必须剥离，否则窗口标题序列会污染 LoginFlow pattern 匹配（如 [root@host ~]# 前的
// \x1b]0;root@host:~\x07 让 anchored 正则 ^\[root@ 失败）。sentinel 字面量是纯 ASCII，
// 不会受影响。
var ansiRe = regexp.MustCompile("\x1b(?:\\[[0-9;?]*[A-Za-z]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\))")

// StripANSI 剥离 ANSI CSI / OSC 转义序列。sentinel 字面量是纯 ASCII，不会受影响。
func StripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// CleanOutput 把 PTY 原始流清洗为给用户看的命令输出。
// 处理步骤：
//  1. 剥离 ANSI CSI 序列
//  2. 移除 exit-sentinel 行（__E_<sid>_<token>__:<code>__，宽松匹配任意 token，包括无 token）
//  3. 移除 PS1 sentinel 残留（__P_<sid>_<token>__> 及其变体，宽松匹配）
//  4. 标准化行结束：\r\n → \n，孤立 \r → "" （PTY 中 \r 通常是光标回退）
//  5. 去除末尾空行
//
// sid 是当前 session 的标识，只清除本 sid 的 sentinel；其他 sid 字面量（命令 echo
// 出来的）保留。宽松匹配 token（(?:_\w+)?）确保 token 化后所有 Run 的 sentinel 都被清掉。
func CleanOutput(s string, sid string) string {
	s = StripANSI(s)

	// 移除 exit-sentinel 行（可能带前后 \r\n / \n）。宽松匹配任意 token（含无 token）。
	exitRe := regexp.MustCompile(`(?m)^__E_` + regexp.QuoteMeta(sid) + `(?:_\w+)?__:-?\d+__\r?\n?`)
	s = exitRe.ReplaceAllString(s, "")

	// 移除 PS1 sentinel 残留（行尾或独立出现）。宽松匹配任意 token（含无 token）。
	ps1Re := regexp.MustCompile(`__P_` + regexp.QuoteMeta(sid) + `(?:_\w+)?__>\s?`)
	s = ps1Re.ReplaceAllString(s, "")

	// 标准化行结束：\r\n → \n，孤立 \r → ""
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "")

	// 去除末尾空行
	s = strings.TrimRight(s, "\n")

	return s
}
