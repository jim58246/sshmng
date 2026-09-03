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
			"ps1 prefix on same line",
			"user@host:~$ __SHELL_DETECT__:/bin/bash:5.2.15(1)-release:\r\n__DETECT_END_abc12345__\r\n",
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
//
// 新设计（PS1-only）：不使用 PROMPT_COMMAND，避免 readonly PROMPT_COMMAND 导致 RC
// 注入失败（审计机器场景）。PS1 用 `$(echo _$?)` 在 prompt 展开时捕获 exit code，
// sentinel 含 sid 和 token（token 由 Run 前的 setup 命令动态注入 PS1）。
func TestBuildRCBash(t *testing.T) {
	sid := "a3f2b1c9"
	rc := BuildRC("bash", sid)
	mustContain := []string{
		"export TERM=dumb",
		"export NO_COLOR=1",
		"export LANG=C.UTF-8",
		"export LC_ALL=C.UTF-8",
		"stty cols 120 rows 100",
		"export PS1='$(echo _$?)__" + sid + "___]# '",
		"set +o history",
		"stty -echo",
	}
	for _, s := range mustContain {
		if !strings.Contains(rc, s) {
			t.Errorf("bash RC missing %q\nRC:\n%s", s, rc)
		}
	}
	// 必须不使用 PROMPT_COMMAND（readonly 安全）
	mustNotContain := []string{
		"PROMPT_COMMAND",
		"__sshmng_precmd",
		"__sshmng_user_prompt",
		"__sshmng_tok",
		"__sshmng_rc",
		"__E_" + sid,
	}
	for _, s := range mustNotContain {
		if strings.Contains(rc, s) {
			t.Errorf("bash RC should NOT contain %q (PS1-only design)\nRC:\n%s", s, rc)
		}
	}
}

// TestBuildRCZsh 验证 zsh RC 用 setopt PROMPT_SUBST + PS1-only（无 precmd_functions）。
//
// zsh 默认不在 PS1 中展开 $(...)，必须 setopt PROMPT_SUBST 才能展开。
func TestBuildRCZsh(t *testing.T) {
	sid := "a3f2b1c9"
	rc := BuildRC("zsh", sid)
	mustContain := []string{
		"export PS1='$(echo _$?)__" + sid + "___]# '",
		"setopt PROMPT_SUBST",
		"unset HISTFILE",
		"stty -echo",
		"export LC_ALL=C.UTF-8",
	}
	for _, s := range mustContain {
		if !strings.Contains(rc, s) {
			t.Errorf("zsh RC missing %q\nRC:\n%s", s, rc)
		}
	}
	mustNotContain := []string{
		"PROMPT_COMMAND",
		"precmd_functions",
		"_sshmng_precmd",
		"__sshmng_tok",
		"__sshmng_rc",
		"__E_" + sid,
	}
	for _, s := range mustNotContain {
		if strings.Contains(rc, s) {
			t.Errorf("zsh RC should NOT contain %q (PS1-only design)\nRC:\n%s", s, rc)
		}
	}
}

// TestBuildRCDash 验证 dash/ash RC 只覆盖 PS1（无 PROMPT_COMMAND，无 $(echo _$?)）。
// dash/ash 不展开 $(...) 在 PS1 中，保持简单 PS1 sentinel。
func TestBuildRCDash(t *testing.T) {
	sid := "a3f2b1c9"
	rc := BuildRC("dash", sid)
	if !strings.Contains(rc, "export PS1='__P_"+sid+"__> '") {
		t.Errorf("dash RC missing PS1 sentinel")
	}
	if !strings.Contains(rc, "export LC_ALL=C.UTF-8") {
		t.Errorf("dash RC missing export LC_ALL=C.UTF-8 (common block)")
	}
	if strings.Contains(rc, "PROMPT_COMMAND") {
		t.Errorf("dash RC should not set PROMPT_COMMAND (not supported)")
	}
	if strings.Contains(rc, "precmd_functions") {
		t.Errorf("dash RC should not set precmd_functions (zsh-specific)")
	}
	if strings.Contains(rc, "$(echo _$?)") {
		t.Errorf("dash RC should not use $(echo _$?) (dash/ash don't expand it in PS1)")
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
	if strings.Contains(rc, "$(echo _$?)") {
		t.Errorf("unknown shell RC should not use $(echo _$?) (unknown shell may not expand it)")
	}
}

// TestBuildRCLocaleProbe 验证 common 块含 locale 探测：优先 C.UTF-8（glibc≥2.35 /
// musl / macOS 内建），回退 en_US.UTF-8（旧 glibc 跑过 locale-gen），最终 C（ASCII 兜底）。
//
// C.UTF-8 需较新 libc；旧 glibc（CentOS 7 / Debian 10）无内建 C.UTF-8，硬编码会静默
// 回退到 C（ASCII，中文文件名可能显示为 ?）。探测让 locale 选择显式，且在 en_US.UTF-8
// 可用时升级——比硬编码 C.UTF-8 在旧系统上静默回退严格更优。
//
// 探测在 RC 执行期跑一次（InjectRC，login 时），之后整个 session 的 LANG/LC_ALL 持续
// 生效；run_in_session 不重跑 RC。
func TestBuildRCLocaleProbe(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "dash", "unknown-shell"} {
		t.Run(shell, func(t *testing.T) {
			rc := BuildRC(shell, "a3f2b1c9")
			// 探测守卫：无 locale 二进制时直接落 C
			if !strings.Contains(rc, "command -v locale >/dev/null 2>&1") {
				t.Errorf("%s RC missing locale probe guard (command -v locale)", shell)
			}
			// 探测命令：locale -a
			if !strings.Contains(rc, "locale -a 2>/dev/null") {
				t.Errorf("%s RC missing 'locale -a' probe command", shell)
			}
			// 优先分支：C.UTF-8（含大小写变体 C.utf8）
			if !strings.Contains(rc, "*C.UTF-8*|*C.utf8*") {
				t.Errorf("%s RC missing C.UTF-8 preferred branch", shell)
			}
			// 回退分支：en_US.UTF-8（旧 glibc + locale-gen）
			if !strings.Contains(rc, "*en_US.UTF-8*|*en_US.utf8*") {
				t.Errorf("%s RC missing en_US.UTF-8 fallback branch", shell)
			}
			// 最终兜底分支：export LANG=C; export LC_ALL=C
			// （带 "; " 分隔，与 "export LANG=C.UTF-8; ..." 区分，不会误匹配优先分支）
			if !strings.Contains(rc, "export LANG=C; export LC_ALL=C") {
				t.Errorf("%s RC missing C final fallback branch", shell)
			}
			// 探测必须在 PS1 之前：locale 先于 sentinel 设置
			probeIdx := strings.Index(rc, "command -v locale")
			ps1Idx := strings.Index(rc, "export PS1=")
			if probeIdx < 0 || ps1Idx < 0 || probeIdx > ps1Idx {
				t.Errorf("%s RC: locale probe must precede PS1 (probe=%d ps1=%d)", shell, probeIdx, ps1Idx)
			}
		})
	}
}

// TestBuildRCSidEscaped 验证 sid 原样出现在 PS1 中。
func TestBuildRCSidEscaped(t *testing.T) {
	sid := "deadbeef"
	rc := BuildRC("bash", sid)
	if !strings.Contains(rc, "$(echo _$?)__"+sid+"___]# ") {
		t.Errorf("PS1 should contain sid, got RC:\n%s", rc)
	}
}

// TestBuildRCBashPS1ExpandsExitCode 验证 bash PS1 的 $(echo _$?) 在命令执行后正确
// 展开为 _<exit_code>。
//
// 用 `${PS1@P}`（bash 4.4+）触发 prompt 展开验证。macOS 自带 bash 3.2 不支持，
// 用 feature detection 跳过——不影响 CI（Linux 通常 bash 5+）。
func TestBuildRCBashPS1ExpandsExitCode(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// Feature detection: bash 3.2 不支持 ${PS1@P}
	probe := exec.Command("bash", "-c", `test "${PS1@P}"`)
	if err := probe.Run(); err != nil {
		t.Skip("bash does not support ${PS1@P} (need bash 4.4+)")
	}
	sid := "deadbeef"
	rc := BuildRC("bash", sid)
	script := rc + `
false
printf '%s\n' "${PS1@P}"
`
	cmd := exec.Command("bash", "--norc", "-c", script)
	output, _ := cmd.CombinedOutput()
	out := string(output)
	// false 退出码 1，PS1 展开后应为 `_1__deadbeef___]# `
	wantSentinel := fmt.Sprintf("_1__%s___]# ", sid)
	if !strings.Contains(out, wantSentinel) {
		t.Errorf("expected PS1 expansion to contain %q (false exit code 1), got: %s", wantSentinel, out)
	}
}

// TestBuildRCZshPS1ExpandsExitCode 验证 zsh PS1 的 $(echo _$?) 在命令执行后正确
// 展开为 _<exit_code>（需 setopt PROMPT_SUBST）。
func TestBuildRCZshPS1ExpandsExitCode(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}
	sid := "deadbeef"
	rc := BuildRC("zsh", sid)
	script := rc + `
false
print -P "$PS1"
`
	cmd := exec.Command("zsh", "-f", "-c", script)
	output, _ := cmd.CombinedOutput()
	out := string(output)
	wantSentinel := fmt.Sprintf("_1__%s___]# ", sid)
	if !strings.Contains(out, wantSentinel) {
		t.Errorf("expected PS1 expansion to contain %q (false exit code 1), got: %s", wantSentinel, out)
	}
}
