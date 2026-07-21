package pty

import (
	"regexp"
	"strconv"
)

// DetectShellReady 判断 PTY 流末尾是否出现本 sid 的 PS1 sentinel，标志 shell 就绪。
// 宽松匹配：允许无 token（injectRC 初始 PS1 `__P_<sid>__> `）和有 token
// （Run 中 `__P_<sid>_<token>__> `）。用 sid 限制匹配，避免其他 session 误命中。
func DetectShellReady(stream string, sid string) bool {
	re := regexp.MustCompile(`__P_` + regexp.QuoteMeta(sid) + `(?:_\w+)?__>\s*$`)
	return re.MatchString(stream)
}

// ExtractExitCode 从 PTY 流中提取本 sid + 本 token 的 exit code。
// 精确匹配 token：命令输出含旧 token 的 sentinel 字面量不会误匹配当前 Run。
// 多个匹配时返回最后一个（最新命令的退出码）。找不到返回 (0, false)。
func ExtractExitCode(stream string, sid string, token string) (int, bool) {
	re := regexp.MustCompile(`__E_` + regexp.QuoteMeta(sid) + `_` + regexp.QuoteMeta(token) + `__:(-?\d+)__`)
	matches := re.FindAllStringSubmatch(stream, -1)
	if len(matches) == 0 {
		return 0, false
	}
	last := matches[len(matches)-1]
	code, err := strconv.Atoi(last[1])
	if err != nil {
		return 0, false
	}
	return code, true
}

// TruncateOutput 把输出截断到 maxBytes，保头截尾。
// max=0 表示不截断。返回 (截断后的输出, 是否截断, 原始字节数)。
func TruncateOutput(output string, maxBytes int) (string, bool, int) {
	total := len(output)
	if maxBytes <= 0 || total <= maxBytes {
		return output, false, total
	}
	return output[:maxBytes], true, total
}
