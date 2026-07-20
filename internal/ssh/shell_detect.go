package ssh

import (
	"fmt"
	"regexp"
	"strings"
)

// ParseShellDetect 从 PTY 流中解析 shell 类型。
// 探测命令格式：echo __SHELL_DETECT__:$0:${BASH_VERSION:-}:${ZSH_VERSION:-}; echo __DETECT_END_<rand>__
// 输出形如：__SHELL_DETECT__:/bin/bash:5.2.15(1)-release:
//
// 返回 (shell, ok)：shell ∈ {"bash","zsh","dash","ash"}，ok=false 表示未找到 / 未完成。
func ParseShellDetect(stream string, rand string) (string, bool) {
	// 必须出现结束标记
	endRe := regexp.MustCompile(`__DETECT_END_` + regexp.QuoteMeta(rand) + `__`)
	if !endRe.MatchString(stream) {
		return "", false
	}
	// 取最后一条 __SHELL_DETECT__:... 行（可能因 echo 回显出现两次）
	detectRe := regexp.MustCompile(`(?m)^__SHELL_DETECT__:([^:\r\n]*):([^:\r\n]*):([^:\r\n]*)\r?$`)
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

// BuildRC 根据 shell 类型生成 RC 注入脚本。
//   - bash: 用 PROMPT_COMMAND（兼容所有版本）
//   - zsh: 用 precmd_functions
//   - dash/ash/未知: 只覆盖 PS1（边界检测，无 exit code）
//
// sid 是当前 session 的 8 字节十六进制 ID。
func BuildRC(shell string, sid string) string {
	ps1 := fmt.Sprintf("export PS1='__P_%s__> '", sid)
	exitSentinel := fmt.Sprintf("echo \"__E_%s__:$?__\"", sid)

	common := fmt.Sprintf(`export TERM=dumb
export NO_COLOR=1
export LANG=C.UTF-8
stty cols 120 rows 100 2>/dev/null
%s
`, ps1)

	switch shell {
	case "bash":
		return common + fmt.Sprintf(`if [ -n "$PROMPT_COMMAND" ]; then
    PROMPT_COMMAND="$PROMPT_COMMAND; %s"
else
    PROMPT_COMMAND='%s'
fi
set +o history
stty -echo 2>/dev/null
`, exitSentinel, exitSentinel)
	case "zsh":
		return common + fmt.Sprintf(`function _sshmng_precmd() { %s }
precmd_functions+=(_sshmng_precmd)
unset HISTFILE
stty -echo 2>/dev/null
`, exitSentinel)
	default:
		// dash/ash/未知：只覆盖 PS1
		return common + "stty -echo 2>/dev/null\n"
	}
}
