package pty

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

// BuildRC 根据 shell 类型生成 RC 注入脚本。
//   - bash: 用 PROMPT_COMMAND（函数包装器）
//   - zsh: 用 precmd_functions（前置）
//   - dash/ash/未知: 只覆盖 PS1（边界检测，无 exit code）
//
// sid 是当前 session 的 8 字节十六进制 ID。
//
// 关键约束 1：`export PS1=` 必须放在 RC 最后一行。真实 bash 在交互模式下逐行执行 RC，
// 每行执行完都会显示 PS1。若 `export PS1=` 在 RC 中间（如第 5 行），injectRC 等
// 第一个 `__P_<sid>__> ` sentinel 时会在该行后立刻匹配，但 RC 后续行（if/set/stty）
// 还没执行。后续行的 prompt 输出残留在 stdoutCh 里被下次 Run 误消费，导致 Run
// 立刻匹配残留 sentinel 返回空 output + exit_code=0，命令实际未执行。
// 把 `export PS1=` 放最后，injectRC 等到 sentinel 时 RC 已全部执行完。
//
// 关键约束 2：bash 必须用函数包装器保存 $?，不能直接 `PROMPT_COMMAND="$PROMPT_COMMAND; echo ...$?..."`。
// 因为 bash 把 PROMPT_COMMAND 当作 `;` 分隔的字符串依次执行，`$?` 在 echo 时反映的是
// 上一条命令（即用户原始 PROMPT_COMMAND，如 `history -a`）的退出码，而非用户命令的退出码。
// 函数包装器在第一时间保存 $? 到 __sshmng_rc，再 eval 用户 PROMPT_COMMAND（保留副作用），
// 最后 echo sentinel 时使用保存的值。
//
// 关键约束 3：zsh 必须前置 _sshmng_precmd 到 precmd_functions（不能用 `+=` 追加）。
// 否则用户已有的 precmd 函数先运行并覆盖 $?，我们 echo 出的是用户 precmd 的退出码。
// 前置确保我们的 precmd 先运行，捕获原始 $?。
func BuildRC(shell string, sid string) string {
	// 初始 PS1（无 token）：injectRC 等这个 sentinel。Run 前动态升级为带 token 的 PS1。
	ps1 := fmt.Sprintf("export PS1='__P_%s__> '", sid)
	// exit sentinel 含 ${__sshmng_tok}：Run 前设置 __sshmng_tok 变量，sentinel 才含 token。
	// token 化确保命令输出含旧 token 的 sentinel 字面量不会误匹配当前 Run。
	exitSentinel := fmt.Sprintf("echo \"__E_%s_${__sshmng_tok}__:${__sshmng_rc}__\"", sid)

	common := `export TERM=dumb
export NO_COLOR=1
export LANG=C.UTF-8
stty cols 120 rows 100 2>/dev/null
`

	switch shell {
	case "bash":
		return common + fmt.Sprintf(`__sshmng_precmd() {
    __sshmng_rc=$?
    if [ -n "$__sshmng_user_prompt" ]; then
        eval "$__sshmng_user_prompt"
    fi
    %s
}
__sshmng_user_prompt="$PROMPT_COMMAND"
PROMPT_COMMAND=__sshmng_precmd
set +o history
stty -echo 2>/dev/null
%s
`, exitSentinel, ps1)
	case "zsh":
		return common + fmt.Sprintf(`function _sshmng_precmd() {
    __sshmng_rc=$?
    %s
}
precmd_functions=(_sshmng_precmd $precmd_functions)
unset HISTFILE
stty -echo 2>/dev/null
%s
`, exitSentinel, ps1)
	default:
		// dash/ash/未知：只覆盖 PS1，无 PROMPT_COMMAND / precmd，无 exit sentinel（无 exit code）。
		// 不 token 化（dash 行为不变）。
		return common + fmt.Sprintf("stty -echo 2>/dev/null\n%s\n", ps1)
	}
}
