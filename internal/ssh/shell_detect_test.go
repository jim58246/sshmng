package ssh

import (
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
		"__E_" + sid + "__:$?__",
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
		"__E_" + sid + "__:$?__",
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
	if !strings.Contains(rc, "__E_"+sid+"__:$?__") {
		t.Errorf("PROMPT_COMMAND should contain sid")
	}
}
