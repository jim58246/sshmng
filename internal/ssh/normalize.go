// Package ssh 实现 SSH 会话管理工具的连接层：拨号、PTY、sentinel 解析、
// 终端规范化、TOFU host key、session 状态机。
//
// 本文件（normalize.go）只做纯字符串处理：ANSI 转义剥离、sentinel 行清理、
// 行结束符标准化。无副作用，便于单测。
package ssh

import (
	"regexp"
	"strings"
)

// ansiCSI 匹配 ANSI CSI 序列：ESC [ + 参数字节（0x30-0x3F）+ 中间字节（0x20-0x2F）
// + 终止字节（0x40-0x7E）。常用颜色 / 光标控制 / 清屏都走 CSI。
var ansiCSI = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

// StripANSI 剥离 ANSI CSI 转义序列。sentinel 字面量是纯 ASCII，不会受影响。
func StripANSI(s string) string {
	return ansiCSI.ReplaceAllString(s, "")
}

// CleanOutput 把 PTY 原始流清洗为给用户看的命令输出。
// 处理步骤：
//  1. 剥离 ANSI CSI 序列
//  2. 移除 exit-sentinel 行（__E_<sid>__:<code>__）
//  3. 移除 PS1 sentinel 残留（__P_<sid>__> 及其变体）
//  4. 标准化行结束：\r\n → \n，孤立 \r → "" （PTY 中 \r 通常是光标回退）
//  5. 去除末尾空行
//
// sid 是当前 session 的标识，只清除本 sid 的 sentinel；其他 sid 字面量（命令 echo
// 出来的）保留。
func CleanOutput(s string, sid string) string {
	s = StripANSI(s)

	// 移除 exit-sentinel 行（可能带前后 \r\n / \n）
	exitRe := regexp.MustCompile(`(?m)^__E_` + regexp.QuoteMeta(sid) + `__:-?\d+__\r?\n?`)
	s = exitRe.ReplaceAllString(s, "")

	// 移除 PS1 sentinel 残留（行尾或独立出现）
	ps1Re := regexp.MustCompile(`__P_` + regexp.QuoteMeta(sid) + `__>\s?`)
	s = ps1Re.ReplaceAllString(s, "")

	// 标准化行结束：\r\n → \n，孤立 \r → ""
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "")

	// 去除末尾空行
	s = strings.TrimRight(s, "\n")

	return s
}
