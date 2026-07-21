package pty

import (
	"testing"
)

// TestStripANSIRemovesCSISequences 验证 ANSI CSI 序列被剥离。
// CSI = \x1b[ + 参数字节 + 终止字节。
func TestStripANSIRemovesCSISequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"no ansi", "hello world", "hello world"},
		{"color reset", "\x1b[0mhello\x1b[0m", "hello"},
		{"red text", "\x1b[0;31merror\x1b[0m", "error"},
		{"bold green", "\x1b[1;32mok\x1b[0m", "ok"},
		{"cursor move", "abc\x1b[2Adef", "abcdef"},
		{"erase line", "\x1b[2Kclean", "clean"},
		{"256 color", "\x1b[38;5;196mred\x1b[0m", "red"},
		{"truecolor", "\x1b[38;2;255;0;0mred\x1b[0m", "red"},
		{"multiple", "\x1b[1mfoo\x1b[0m \x1b[31mbar\x1b[0m", "foo bar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := StripANSI(c.in)
			if got != c.want {
				t.Errorf("StripANSI(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestStripANSIPreservesSentinels 验证 sentinel 字面量不被剥离。
// sentinel 是 ASCII 字符串，不含 ANSI 序列，理应原样保留。
func TestStripANSIPreservesSentinels(t *testing.T) {
	cases := []string{
		"__P_a3f2b1c9__> ",
		"__E_a3f2b1c9__:0__",
		"__E_a3f2b1c9__:-1__",
		"__SHELL_DETECT__:/bin/bash:5.1.0:",
		"__DETECT_END_1a2b3c4d__",
	}
	for _, s := range cases {
		got := StripANSI(s)
		if got != s {
			t.Errorf("StripANSI should preserve sentinel %q, got %q", s, got)
		}
	}
}

// TestStripANSIPreservesSentinelsInMixedOutput 验证混合 ANSI + sentinel 的输出剥离 ANSI 后 sentinel 完整。
func TestStripANSIPreservesSentinelsInMixedOutput(t *testing.T) {
	in := "\x1b[0;31mfile1\x1b[0m\r\n__E_a3f2b1c9__:0__\r\n__P_a3f2b1c9__> "
	want := "file1\r\n__E_a3f2b1c9__:0__\r\n__P_a3f2b1c9__> "
	got := StripANSI(in)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestStripANSIRemovesOSCSequences 验证 ANSI OSC 序列被剥离。
// OSC = \x1b] + 内容 + 终止符（BEL \x07 或 ST = \x1b\\）。
// bash/zsh 等会发 OSC 0;title 设置窗口标题，若不剥离会污染 LoginFlow pattern 匹配。
func TestStripANSIRemovesOSCSequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"osc with bel", "\x1b]0;root@host:~\x07[root@host ~]# ", "[root@host ~]# "},
		{"osc with st", "\x1b]0;root@host:~\x1b\\[root@host ~]# ", "[root@host ~]# "},
		{"osc empty title bel", "before\x1b]0;\x07after", "beforeafter"},
		{"osc empty title st", "before\x1b]0;\x1b\\after", "beforeafter"},
		{"osc mixed with csi", "\x1b]0;title\x07\x1b[0;31mred\x1b[0m", "red"},
		{"multiple osc", "\x1b]0;a\x07\x1b]0;b\x07end", "end"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := StripANSI(c.in)
			if got != c.want {
				t.Errorf("StripANSI(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestCleanOutputRemovesSentinelLines 验证 CleanOutput 移除 sentinel 行、PS1 残留、ANSI 转义。
func TestCleanOutputRemovesSentinelLines(t *testing.T) {
	sid := "a3f2b1c9"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "command output with sentinel",
			in:   "file1\r\nfile2\r\n__E_" + sid + "__:0__\r\n__P_" + sid + "__> ",
			want: "file1\nfile2",
		},
		{
			name: "ansi + sentinel",
			in:   "\x1b[0;31merror\x1b[0m\r\n__E_" + sid + "__:1__\r\n__P_" + sid + "__> ",
			want: "error",
		},
		{
			name: "no sentinel passthrough",
			in:   "hello world",
			want: "hello world",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
		{
			name: "only sentinel",
			in:   "__E_" + sid + "__:0__\r\n__P_" + sid + "__> ",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CleanOutput(c.in, sid)
			if got != c.want {
				t.Errorf("CleanOutput(%q, %q) = %q, want %q", c.in, sid, got, c.want)
			}
		})
	}
}

// TestCleanOutputNormalizeLineEndings 验证 CleanOutput 把 \r\n 标准化为 \n。
func TestCleanOutputNormalizeLineEndings(t *testing.T) {
	sid := "a3f2b1c9"
	in := "line1\r\nline2\r\nline3\r\n__E_" + sid + "__:0__\r\n__P_" + sid + "__> "
	want := "line1\nline2\nline3"
	got := CleanOutput(in, sid)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCleanOutputTrailingNewline 验证 CleanOutput 处理尾部 \r\n 的行为：
// 命令输出后通常有 \r\n 然后 sentinel，清洗后应去除末尾空行。
func TestCleanOutputTrailingNewline(t *testing.T) {
	sid := "a3f2b1c9"
	in := "output\r\n__E_" + sid + "__:0__\r\n__P_" + sid + "__> "
	want := "output"
	got := CleanOutput(in, sid)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCleanOutputPreservesInternalNewlines 验证命令输出中的多行内容保留。
func TestCleanOutputPreservesInternalNewlines(t *testing.T) {
	sid := "a3f2b1c9"
	in := "line1\r\n\r\nline3\r\n__E_" + sid + "__:0__\r\n__P_" + sid + "__> "
	want := "line1\n\nline3"
	got := CleanOutput(in, sid)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCleanOutputWithDifferentSid 验证 CleanOutput 不会误删其他 sid 的 sentinel。
// 命令输出中可能含字面量 __E_<other>__:0__，不应被清除。
func TestCleanOutputWithDifferentSid(t *testing.T) {
	sid := "a3f2b1c9"
	// 输出含其他 sid 字面量（命令 echo 出来的），不应被当 sentinel 清除
	in := "echo __E_deadbeef__:0__\r\n__E_" + sid + "__:0__\r\n__P_" + sid + "__> "
	got := CleanOutput(in, sid)
	want := "echo __E_deadbeef__:0__"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCleanOutputStrayCR 验证孤立的 \r（无 \n）被处理。
// PTY 流中 \r 可能用于光标回退，清洗后应去除。
func TestCleanOutputStrayCR(t *testing.T) {
	sid := "a3f2b1c9"
	in := "progress\r100%\r\n__E_" + sid + "__:0__\r\n__P_" + sid + "__> "
	got := CleanOutput(in, sid)
	want := "progress100%"
	// 或者 "progress\n100%"，取决于实现。我们倾向把 \r 替换为空（PTY 中 \r 通常是光标回退）。
	// 这里接受 "progress100%" 或 "progress\n100%"
	if got != want && got != "progress\n100%" {
		t.Errorf("got %q, want %q or similar", got, want)
	}
}
