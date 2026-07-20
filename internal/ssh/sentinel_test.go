package ssh

import (
	"strings"
	"testing"
)

// TestDetectShellReady 验证 PTY 流末尾出现 PS1 sentinel 时判定 shell 就绪。
func TestDetectShellReady(t *testing.T) {
	sid := "a3f2b1c9"
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"ends with PS1 sentinel", "output\r\n__E_" + sid + "__:0__\r\n__P_" + sid + "__> ", true},
		{"only PS1 sentinel", "__P_" + sid + "__> ", true},
		{"PS1 with trailing space", "__P_" + sid + "__>  ", true},
		{"no sentinel", "some output", false},
		{"empty", "", false},
		{"sentinel in middle not ready", "__P_" + sid + "__> some text", false},
		{"only exit sentinel not ready", "__E_" + sid + "__:0__", false},
		{"other sid not ready", "__P_deadbeef__> ", false},
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

// TestExtractExitCode 验证从 PTY 流中提取 exit code。
func TestExtractExitCode(t *testing.T) {
	sid := "a3f2b1c9"
	cases := []struct {
		name      string
		in        string
		wantCode  int
		wantFound bool
	}{
		{"zero", "output\r\n__E_" + sid + "__:0__\r\n", 0, true},
		{"positive", "__E_" + sid + "__:42__\r\n", 42, true},
		{"negative", "__E_" + sid + "__:-1__\r\n", -1, true},
		{"two digit", "__E_" + sid + "__:127__", 127, true},
		{"large", "__E_" + sid + "__:255__", 255, true},
		{"no sentinel", "output", 0, false},
		{"empty", "", 0, false},
		{"other sid", "__E_deadbeef__:0__", 0, false},
		{"truncated sentinel", "__E_" + sid + "__:0", 0, false},
		{"with ANSI", "\x1b[0m__E_" + sid + "__:0__\x1b[0m", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, found := ExtractExitCode(c.in, sid)
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
	in := "__E_" + sid + "__:0__\r\n__E_" + sid + "__:1__\r\n__P_" + sid + "__> "
	code, found := ExtractExitCode(in, sid)
	if !found || code != 1 {
		t.Errorf("got code=%d found=%v, want code=1 found=true (last sentinel wins)", code, found)
	}
}

// TestExtractExitCodeLiteralCollision 验证命令输出含 sentinel 字面量不影响解析。
// 命令 echo __E_<sid>__:99__ 输出会被 echo 出来，但实际命令退出码是 0。
// 由于 echo 输出后还有真正的 sentinel，提取最后一个应该拿到 0。
func TestExtractExitCodeLiteralCollision(t *testing.T) {
	sid := "a3f2b1c9"
	in := "echo __E_" + sid + "__:99__\r\n__E_" + sid + "__:0__\r\n__P_" + sid + "__> "
	code, found := ExtractExitCode(in, sid)
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
