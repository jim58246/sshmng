package pty

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// TestParseShellDetect 验证从 PTY 输出中解析 shell 类型。
// 探测命令格式：echo __SHELL_DETECT__:$0:${BASH_VERSION:-}:${ZSH_VERSION:-}; echo __DETECT_END_<rand>__
// 输出形如：__SHELL_DETECT__:/bin/bash:5.2.15(1)-release:
func TestParseShellDetect(t *testing.T) {
	rand := "abc12345"
	sid := "deadbeef"
	cases := []struct {
		name  string
		in    string
		shell string
		ok    bool
	}{
		{
			"bash",
			"__SHELL_DETECT__:/bin/bash:5.2.15(1)-release:\r\n__DETECT_END_abc12345__\r\n",
			"bash",
			true,
		},
		{
			"zsh",
			"__SHELL_DETECT__:/bin/zsh::5.9\r\n__DETECT_END_abc12345__\r\n",
			"zsh",
			true,
		},
		{
			"dash",
			"__SHELL_DETECT__:/bin/sh::\r\n__DETECT_END_abc12345__\r\n",
			"dash",
			true,
		},
		{
			"ash busybox",
			"__SHELL_DETECT__:/bin/ash::\r\n__DETECT_END_abc12345__\r\n",
			"ash",
			true,
		},
		{
			"with leading echo",
			"echo __SHELL_DETECT__:$0:${BASH_VERSION:-}:${ZSH_VERSION:-}\r\n__SHELL_DETECT__:/bin/bash:5.2.15:\r\n__DETECT_END_abc12345__\r\n",
			"bash",
			true,
		},
		{
			"missing end marker",
			"__SHELL_DETECT__:/bin/bash:5.2.15:\r\n",
			"",
			false,
		},
		{
			"no detect line",
			"some output\r\n__DETECT_END_abc12345__\r\n",
			"",
			false,
		},
		{
			"wrong rand",
			"__SHELL_DETECT__:/bin/bash:5.2.15:\r\n__DETECT_END_wrongrand__\r\n",
			"",
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			shell, ok := ParseShellDetect(c.in, rand)
			if ok != c.ok {
				t.Errorf("ok = %v, want %v", ok, c.ok)
			}
			if shell != c.shell {
				t.Errorf("shell = %q, want %q", shell, c.shell)
			}
		})
	}
	_ = sid
}

// TestBuildRCBash 验证 bash RC 包含必要字段。
func TestBuildRCBash(t *testing.T) {
	sid := "a3f2b1c9"
	rc := BuildRC("bash", sid)
	mustContain := []string{
		"export TERM=dumb",
		"export NO_COLOR=1",
		"export LANG=C.UTF-8",
		"stty cols 120 rows 100",
		"export PS1='__P_" + sid + "__> '",
		"PROMPT_COMMAND",
		"__sshmng_precmd",
		"__sshmng_rc=$?",
		"__sshmng_user_prompt",
		"__E_" + sid + "_${__sshmng_tok}__:${__sshmng_rc}__",
		"set +o history",
		"stty -echo",
	}
	for _, s := range mustContain {
		if !strings.Contains(rc, s) {
			t.Errorf("bash RC missing %q\nRC:\n%s", s, rc)
		}
	}
}

// TestBuildRCZsh 验证 zsh RC 用 precmd_functions 而非 PROMPT_COMMAND。
func TestBuildRCZsh(t *testing.T) {
	sid := "a3f2b1c9"
	rc := BuildRC("zsh", sid)
	mustContain := []string{
		"export PS1='__P_" + sid + "__> '",
		"precmd_functions",
		"_sshmng_precmd",
		"__sshmng_rc=$?",
		"__E_" + sid + "_${__sshmng_tok}__:${__sshmng_rc}__",
		"unset HISTFILE",
		"stty -echo",
	}
	for _, s := range mustContain {
		if !strings.Contains(rc, s) {
			t.Errorf("zsh RC missing %q\nRC:\n%s", s, rc)
		}
	}
	if strings.Contains(rc, "PROMPT_COMMAND") {
		t.Errorf("zsh RC should not use PROMPT_COMMAND")
	}
	// 必须前置（而非 += 追加），确保我们的 precmd 在用户 precmd 之前运行，捕获原始 $?
	if !strings.Contains(rc, "precmd_functions=(_sshmng_precmd $precmd_functions)") {
		t.Errorf("zsh RC should prepend _sshmng_precmd to precmd_functions to capture $? before user precmds\nRC:\n%s", rc)
	}
}

// TestBuildRCDash 验证 dash/ash RC 只覆盖 PS1（无 PROMPT_COMMAND）。
func TestBuildRCDash(t *testing.T) {
	sid := "a3f2b1c9"
	rc := BuildRC("dash", sid)
	if !strings.Contains(rc, "export PS1='__P_"+sid+"__> '") {
		t.Errorf("dash RC missing PS1 sentinel")
	}
	if strings.Contains(rc, "PROMPT_COMMAND") {
		t.Errorf("dash RC should not set PROMPT_COMMAND (not supported)")
	}
	if strings.Contains(rc, "precmd_functions") {
		t.Errorf("dash RC should not set precmd_functions (zsh-specific)")
	}
}

// TestBuildRCUnknownShellFallsBackToPS1Only 验证未知 shell 退化为只设 PS1。
func TestBuildRCUnknownShellFallsBackToPS1Only(t *testing.T) {
	sid := "a3f2b1c9"
	rc := BuildRC("unknown-shell", sid)
	if !strings.Contains(rc, "export PS1='__P_"+sid+"__> '") {
		t.Errorf("unknown shell RC should still set PS1 sentinel")
	}
	if strings.Contains(rc, "PROMPT_COMMAND") {
		t.Errorf("unknown shell RC should not set PROMPT_COMMAND")
	}
}

// TestBuildRCSidEscaped 验证 sid 不被 shell 特殊解释。
// sid 是十六进制串本身不含特殊字符，但 RC 中应安全使用。
func TestBuildRCSidEscaped(t *testing.T) {
	sid := "deadbeef"
	rc := BuildRC("bash", sid)
	// sid 应原样出现在 PS1 和 PROMPT_COMMAND 中
	if !strings.Contains(rc, "__P_"+sid+"__> ") {
		t.Errorf("PS1 should contain sid")
	}
	if !strings.Contains(rc, "__E_"+sid+"_${__sshmng_tok}__:${__sshmng_rc}__") {
		t.Errorf("PROMPT_COMMAND should contain sid")
	}
}

// TestBuildRCBashPreservesExitCodeWithUserPromptCommand 验证 bash RC 在用户已有
// PROMPT_COMMAND 时仍能正确捕获用户命令的退出码。
//
// 场景：用户 PROMPT_COMMAND='true'（退出码 0），用户命令 `false`（退出码 1）。
// 旧实现 `PROMPT_COMMAND="$PROMPT_COMMAND; echo ...$?..."` 会让 $? 反映 `true` 的
// 退出码（0），sentinel 错误地输出 `__E_<sid>:0__`。
// 新实现用函数包装器在第一时间保存 $?，sentinel 应输出 `__E_<sid>:1__`。
//
// token 化后 sentinel 形如 `__E_<sid>_<token>__:<code>__`。测试需先 export __sshmng_tok
// 让 ${__sshmng_tok} 展开，sentinel 才含 token。
func TestBuildRCBashPreservesExitCodeWithUserPromptCommand(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	sid := "deadbeef"
	tok := "abc12345"
	rc := BuildRC("bash", sid)

	script := `export PROMPT_COMMAND='true'
export __sshmng_tok=` + tok + `
` + rc + `
false
eval "$PROMPT_COMMAND"
`
	cmd := exec.Command("bash", "-c", script)
	output, _ := cmd.CombinedOutput()
	out := string(output)

	wantSentinel := fmt.Sprintf("__E_%s_%s__:1__", sid, tok)
	badSentinel := fmt.Sprintf("__E_%s_%s__:0__", sid, tok)
	if !strings.Contains(out, wantSentinel) {
		t.Errorf("expected output to contain %q (user command exit code 1), got: %s", wantSentinel, out)
	}
	if strings.Contains(out, badSentinel) {
		t.Errorf("output should NOT contain %q (user PROMPT_COMMAND exit code 0 — means $? was overwritten), got: %s", badSentinel, out)
	}
}

// TestBuildRCZshPreservesExitCodeWithUserPrecmd 验证 zsh RC 在用户已有 precmd_functions
// 时仍能正确捕获用户命令的退出码。
//
// 场景：用户 precmd_functions=(user_precmd)（user_precmd 返回 0），用户命令 `false`
// （退出码 1）。旧实现 `precmd_functions+=(_sshmng_precmd)` 让 user_precmd 先运行，
// 覆盖 $?，sentinel 错误地输出 `__E_<sid>:0__`。新实现前置 _sshmng_precmd 并在函数
// 开头保存 $?，sentinel 应输出 `__E_<sid>:1__`。
//
// token 化后 sentinel 形如 `__E_<sid>_<token>__:<code>__`。测试需先 export __sshmng_tok
// 让 ${__sshmng_tok} 展开，sentinel 才含 token。
func TestBuildRCZshPreservesExitCodeWithUserPrecmd(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}
	sid := "deadbeef"
	tok := "abc12345"
	rc := BuildRC("zsh", sid)

	script := `function user_precmd() { true; }
precmd_functions=(user_precmd)
export __sshmng_tok=` + tok + `
` + rc + `
false
for f in $precmd_functions; do $f; done
`
	cmd := exec.Command("zsh", "-c", script)
	output, _ := cmd.CombinedOutput()
	out := string(output)

	wantSentinel := fmt.Sprintf("__E_%s_%s__:1__", sid, tok)
	badSentinel := fmt.Sprintf("__E_%s_%s__:0__", sid, tok)
	if !strings.Contains(out, wantSentinel) {
		t.Errorf("expected output to contain %q (user command exit code 1), got: %s", wantSentinel, out)
	}
	if strings.Contains(out, badSentinel) {
		t.Errorf("output should NOT contain %q (user precmd exit code 0 — means $? was overwritten), got: %s", badSentinel, out)
	}
}
