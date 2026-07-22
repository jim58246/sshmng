package pty

import (
	"strings"
	"testing"
)

// TestDetectShellReady 验证 PTY 流末尾出现本 sid 的初始 PS1 sentinel 时判定 shell 就绪。
// 新 sentinel 格式（bash/zsh）：`_<rc>__<sid>_<token>__]# `，token 可空（injectRC 初始 PS1）。
// dash/ash 仍用 `__P_<sid>__> `（无 $(echo _$?) 扩展）。
func TestDetectShellReady(t *testing.T) {
	sid := "a3f2b1c9"
	tok := "11223344"
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"ends with initial PS1 (empty token)", "output\r\n_0__" + sid + "___]# ", true},
		{"only initial PS1", "_0__" + sid + "___]# ", true},
		{"PS1 with token", "_0__" + sid + "_" + tok + "__]# ", true},
		{"PS1 with token trailing space", "_0__" + sid + "_" + tok + "__]#  ", true},
		{"exit code 1", "_1__" + sid + "_" + tok + "__]# ", true},
		{"no sentinel", "some output", false},
		{"empty", "", false},
		{"sentinel in middle not ready", "_0__" + sid + "_" + tok + "__]# some text", false},
		{"other sid not ready", "_0__deadbeef___]# ", false},
		{"other sid with token not ready", "_0__deadbeef_aabb__]# ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DetectShellReady(c.in, sid)
			if got != c.want {
				t.Errorf("DetectShellReady(%q, %q) = %v, want %v", c.in, sid, got, c.want)
			}
		})
	}
}

// TestExtractExitCode 验证从 PTY 流中提取本 sid + 本 token 的 exit code。
// 精确匹配 token：命令输出含旧 token 的 sentinel 字面量不会误匹配。
// 新 sentinel 格式：`_<rc>__<sid>_<token>__]# `，rc 是数字（exit code）。
func TestExtractExitCode(t *testing.T) {
	sid := "a3f2b1c9"
	tok := "11223344"
	cases := []struct {
		name      string
		in        string
		wantCode  int
		wantFound bool
	}{
		{"zero", "output\r\n_0__" + sid + "_" + tok + "__]# ", 0, true},
		{"positive", "_42__" + sid + "_" + tok + "__]# ", 42, true},
		{"two digit", "_127__" + sid + "_" + tok + "__]# ", 127, true},
		{"large", "_255__" + sid + "_" + tok + "__]# ", 255, true},
		{"no sentinel", "output", 0, false},
		{"empty", "", 0, false},
		{"other sid", "_0__deadbeef_" + tok + "__]# ", 0, false},
		{"truncated sentinel", "_0__" + sid + "_" + tok + "__]#", 0, false},
		{"with ANSI", "\x1b[0m_0__" + sid + "_" + tok + "__]# \x1b[0m", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, found := ExtractExitCode(c.in, sid, tok)
			if found != c.wantFound {
				t.Errorf("found = %v, want %v", found, c.wantFound)
			}
			if c.wantFound && code != c.wantCode {
				t.Errorf("code = %d, want %d", code, c.wantCode)
			}
		})
	}
}

// TestExtractExitCodeMultipleSentinels 验证多 sentinel 时提取最后一个（最新命令的退出码）。
func TestExtractExitCodeMultipleSentinels(t *testing.T) {
	sid := "a3f2b1c9"
	tok := "11223344"
	in := "_0__" + sid + "_" + tok + "__]# \r\n_1__" + sid + "_" + tok + "__]# "
	code, found := ExtractExitCode(in, sid, tok)
	if !found || code != 1 {
		t.Errorf("got code=%d found=%v, want code=1 found=true (last sentinel wins)", code, found)
	}
}

// TestExtractExitCodeTokenMismatch 验证 token 不匹配时返回 found=false。
// 这是 token 化的核心保证：命令输出含旧 token 的 sentinel 字面量不会误匹配新 Run。
func TestExtractExitCodeTokenMismatch(t *testing.T) {
	sid := "a3f2b1c9"
	currentTok := "11223344"
	oldTok := "deadbeef"
	in := "_0__" + sid + "_" + oldTok + "__]# "
	code, found := ExtractExitCode(in, sid, currentTok)
	if found {
		t.Errorf("expected found=false for mismatched token, got code=%d found=%v", code, found)
	}
}

// TestExtractExitCodeLiteralCollision 验证命令输出含 sentinel 字面量不影响解析。
// 命令 echo 出旧 token 的 sentinel 字面量，但真正的 sentinel 含当前 token，提取最后一个应拿到当前 token 的 exit code。
func TestExtractExitCodeLiteralCollision(t *testing.T) {
	sid := "a3f2b1c9"
	tok := "11223344"
	// 命令输出含旧 token sentinel 字面量（99），真 sentinel 含当前 token（0）
	in := "echo _99__" + sid + "_oldtok__]# \r\n_0__" + sid + "_" + tok + "__]# "
	code, found := ExtractExitCode(in, sid, tok)
	if !found || code != 0 {
		t.Errorf("got code=%d found=%v, want code=0 found=true", code, found)
	}
}

// TestTruncateOutputKeepHead 验证超长输出保头截断。
func TestTruncateOutputKeepHead(t *testing.T) {
	long := strings.Repeat("x", 1000)
	got, truncated, totalBytes := TruncateOutput(long, 100)
	if !truncated {
		t.Errorf("truncated should be true")
	}
	if totalBytes != 1000 {
		t.Errorf("totalBytes = %d, want 1000", totalBytes)
	}
	if len(got) != 100 {
		t.Errorf("len(got) = %d, want 100", len(got))
	}
	if got != long[:100] {
		t.Errorf("got should be first 100 bytes")
	}
}

// TestTruncateOutputNoTruncationUnderLimit 验证未超限时不截断。
func TestTruncateOutputNoTruncationUnderLimit(t *testing.T) {
	in := "short output"
	got, truncated, totalBytes := TruncateOutput(in, 100)
	if truncated {
		t.Errorf("truncated should be false")
	}
	if totalBytes != len(in) {
		t.Errorf("totalBytes = %d, want %d", totalBytes, len(in))
	}
	if got != in {
		t.Errorf("got = %q, want %q", got, in)
	}
}

// TestTruncateOutputZeroMaxNoTruncation 验证 max=0 表示不截断。
func TestTruncateOutputZeroMaxNoTruncation(t *testing.T) {
	long := strings.Repeat("x", 1000)
	got, truncated, totalBytes := TruncateOutput(long, 0)
	if truncated {
		t.Errorf("truncated should be false when max=0")
	}
	if totalBytes != 1000 {
		t.Errorf("totalBytes = %d, want 1000", totalBytes)
	}
	if got != long {
		t.Errorf("got should be unchanged")
	}
}

// TestTruncateOutputExactLimit 验证正好等于 limit 时不截断。
func TestTruncateOutputExactLimit(t *testing.T) {
	in := strings.Repeat("x", 100)
	got, truncated, _ := TruncateOutput(in, 100)
	if truncated {
		t.Errorf("truncated should be false at exact limit")
	}
	if got != in {
		t.Errorf("got should be unchanged")
	}
}

// TestTruncateOutputEmpty 验证空输入。
func TestTruncateOutputEmpty(t *testing.T) {
	got, truncated, totalBytes := TruncateOutput("", 100)
	if truncated {
		t.Errorf("truncated should be false")
	}
	if totalBytes != 0 {
		t.Errorf("totalBytes = %d, want 0", totalBytes)
	}
	if got != "" {
		t.Errorf("got = %q, want empty", got)
	}
}
