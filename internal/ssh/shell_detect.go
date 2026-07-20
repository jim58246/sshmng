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
//
// 关键：`export PS1=` 必须放在 RC 最后一行。真实 bash 在交互模式下逐行执行 RC，
// 每行执行完都会显示 PS1。若 `export PS1=` 在 RC 中间（如第 5 行），injectRC 等
// 第一个 `__P_<sid>__> ` sentinel 时会在该行后立刻匹配，但 RC 后续行（if/set/stty）
// 还没执行。后续行的 prompt 输出残留在 stdoutCh 里被下次 Run 误消费，导致 Run
// 立刻匹配残留 sentinel 返回空 output + exit_code=0，命令实际未执行。
// 把 `export PS1=` 放最后，injectRC 等到 sentinel 时 RC 已全部执行完。
func BuildRC(shell string, sid string) string {
	ps1 := fmt.Sprintf("export PS1='__P_%s__> '", sid)
	exitSentinel := fmt.Sprintf("echo \"__E_%s__:$?__\"", sid)

	common := `export TERM=dumb
export NO_COLOR=1
export LANG=C.UTF-8
stty cols 120 rows 100 2>/dev/null
`

	switch shell {
	case "bash":
		return common + fmt.Sprintf(`if [ -n "$PROMPT_COMMAND" ]; then
    PROMPT_COMMAND="$PROMPT_COMMAND; %s"
else
    PROMPT_COMMAND='%s'
fi
set +o history
stty -echo 2>/dev/null
%s
`, exitSentinel, exitSentinel, ps1)
	case "zsh":
		return common + fmt.Sprintf(`function _sshmng_precmd() { %s }
precmd_functions+=(_sshmng_precmd)
unset HISTFILE
stty -echo 2>/dev/null
%s
`, exitSentinel, ps1)
	default:
		// dash/ash/未知：只覆盖 PS1
		return common + fmt.Sprintf("stty -echo 2>/dev/null\n%s\n", ps1)
	}
}
