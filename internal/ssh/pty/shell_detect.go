package pty

import (
	"fmt"
	"regexp"
	"strings"
)

// ParseShellDetect 从 PTY 流中解析 shell 类型。
// 探测命令格式：__sshmng_dr=<rand>; echo __SHELL_DETECT__:$0:${BASH_VERSION:-}:${ZSH_VERSION:-}; echo __DETECT_END_${__sshmng_dr}__
// end marker 用变量构造（非字面量），防止回显行含 end marker 字面量导致 readUntilPattern 误匹配。
// 输出形如：__SHELL_DETECT__:/bin/bash:5.2.15(1)-release:
//
// 返回 (shell, ok)：shell ∈ {"bash","zsh","dash","ash"}，ok=false 表示未找到 / 未完成。
func ParseShellDetect(stream string, rand string) (string, bool) {
	// 必须出现结束标记
	endRe := regexp.MustCompile(`__DETECT_END_` + regexp.QuoteMeta(rand) + `__`)
	if !endRe.MatchString(stream) {
		return "", false
	}
	// 取最后一条 __SHELL_DETECT__:... 行（可能因 echo 回显出现两次）。
	// 不锚定行首 ^：某些 shell 下 __SHELL_DETECT__ 前面可能紧跟 PS1（同一行），
	// 锚定行首会漏匹配。$ 仍保留行尾约束避免匹配行中间片段。多个匹配取最后一个
	// （回显行在前、结果行在后），保证取到真正结果。
	detectRe := regexp.MustCompile(`(?m)__SHELL_DETECT__:([^:\r\n]*):([^:\r\n]*):([^:\r\n]*)\r?$`)
	matches := detectRe.FindAllStringSubmatch(stream, -1)
	if len(matches) == 0 {
		return "", false
	}
	last := matches[len(matches)-1]
	shellPath := last[1]
	bashVer := last[2]
	zshVer := last[3]
	return classifyShell(shellPath, bashVer, zshVer), true
}

// classifyShell 根据 $0 / BASH_VERSION / ZSH_VERSION 判定 shell 类型。
func classifyShell(shellPath string, bashVer, zshVer string) string {
	base := shellPath
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}
	if bashVer != "" {
		return "bash"
	}
	if zshVer != "" {
		return "zsh"
	}
	switch base {
	case "bash":
		return "bash"
	case "zsh":
		return "zsh"
	case "ash", "busybox":
		return "ash"
	case "dash", "sh":
		return "dash"
	default:
		return base
	}
}

// BuildRC 根据 shell 类型生成 RC 注入脚本（PS1-only 设计）。
//   - bash: PS1 用 `$(echo _$?)` 在 prompt 展开时捕获 exit code，sentinel 含 sid 和 token
//   - zsh: 同 bash，额外 setopt PROMPT_SUBST 让 PS1 展开 $(...)
//   - dash/ash/未知: 只覆盖 PS1（边界检测，无 exit code）
//
// sid 是当前 session 的 8 字节十六进制 ID。
//
// 新设计（PS1-only）替代旧 PROMPT_COMMAND 方案，原因：
//   - 审计机器可能把 PROMPT_COMMAND 设为 readonly，旧方案注入失败。
//   - PS1 是 shell 内置变量，不会被设为 readonly，无此风险。
//   - `$(echo _$?)` 在 PS1 展开时（即 prompt 显示前）执行，捕获的是上一条命令的
//     真实退出码（不被其他 PROMPT_COMMAND/precmd 干扰）。
//
// sentinel 格式：`_<rc>__<sid>_<token>__]# `
//   - <rc> 是 exit code（0-255），由 `$(echo _$?)` 展开
//   - <sid> 是 session ID
//   - <token> 是 Run 前动态注入的 8 字节 hex（初始 PS1 中 token 为空，即 `_<sid>___]# `）
//
// 关键约束 1：`export PS1=` 必须放在 RC 最后一行。真实 bash 在交互模式下逐行执行 RC，
// 每行执行完都会显示 PS1。若 `export PS1=` 在 RC 中间（如第 5 行），injectRC 等
// 第一个 sentinel 时会在该行后立刻匹配，但 RC 后续行（set/stty）还没执行。后续行的
// prompt 输出残留在 stdoutCh 里被下次 Run 误消费，导致 Run 立刻匹配残留 sentinel
// 返回空 output + exit_code=0，命令实际未执行。把 `export PS1=` 放最后，injectRC
// 等到 sentinel 时 RC 已全部执行完。
//
// 关键约束 2：zsh 必须在 PS1 赋值前 `setopt PROMPT_SUBST`，否则 PS1 中的 `$(...)`
// 不会被展开。bash 默认展开 PS1 中的 `$(...)`，无需额外设置。
//
// 关键约束 3：token 不在 RC 中硬编码。RC 设的初始 PS1 token 为空（`__<sid>___]# `），
// Run 前通过 setup 命令 `PS1='$(echo _$?)__<sid>_<token>__]# '` 动态升级 PS1 为带 token
// 版本。token 化确保命令输出含旧 token 的 sentinel 字面量不会误匹配当前 Run。
func BuildRC(shell string, sid string) string {
	common := `export TERM=dumb
export NO_COLOR=1
export LANG=C.UTF-8
stty cols 120 rows 100 2>/dev/null
`

	switch shell {
	case "bash":
		// PS1 中 `$(echo _$?)` 在 prompt 展开时执行，输出 `_<rc>`。
		// 初始 token 为空（`__<sid>___]# `，3 个下划线 = `_` + 空 token + `__`）。
		// Run 前 setup 命令动态升级 PS1 为带 token 版本。
		ps1 := fmt.Sprintf("export PS1='$(echo _$?)__%s___]# '", sid)
		return common + fmt.Sprintf(`set +o history
stty -echo 2>/dev/null
%s
`, ps1)
	case "zsh":
		// zsh 需 setopt PROMPT_SUBST 才展开 PS1 中的 $(...)。
		ps1 := fmt.Sprintf("export PS1='$(echo _$?)__%s___]# '", sid)
		return common + fmt.Sprintf(`setopt PROMPT_SUBST
unset HISTFILE
stty -echo 2>/dev/null
%s
`, ps1)
	default:
		// dash/ash/未知：只覆盖 PS1，无 $(echo _$?) 扩展（dash/ash 不展开 $(...) 在 PS1 中）。
		// 无 exit code，runPS1Only 路径 exit code 恒 -1。
		ps1 := fmt.Sprintf("export PS1='__P_%s__> '", sid)
		return common + fmt.Sprintf("stty -echo 2>/dev/null\n%s\n", ps1)
	}
}
