package pty

import (
	"strings"
	"testing"

	"github.com/jim58246/sshmng/internal/config"
	"github.com/jim58246/sshmng/internal/ssh/conn"
)

// auto-capture（run_in_session / sshmng ssh <name> cmd）路径经 BuildRC 注入
// TERM=dumb + NO_COLOR=1 压制 ANSI，但命令自身仍可能发出 TUI 转义序列
// （颜色、光标、alt-screen、OSC 标题、清屏）。这些序列混在输出流里，威胁
// sentinel 的字符串匹配：若 sentinel 检测被 ANSI 噪声干扰，会误判命令边界、
// 吞掉输出或拿到错误 exit code。
//
// 本组测试用 fakeShellServer（runFakeShell 命令阶段用 sh -c 真实执行命令并
// CombinedOutput 原样转发）端到端验证：各类 ANSI 转义序列存在时，Run 仍能
// 正确捕获正文 + exit code，sentinel 不受影响。stripANSI 在清洗阶段剥离序列，
// sentinel 字面量是纯 ASCII 不被影响。
//
// 无需容器/真实 sshd：fakeShellServer 是进程内 Go SSH server。

// newAutoCapturePty 装配一个 fakeShellServer + 已 InjectRC 的 PtyConn，
// 供 auto-capture 路径测试复用。返回 (ptyConn, cleanup)。
func newAutoCapturePty(t *testing.T) (*PtyConn, func()) {
	t.Helper()
	srv := newFakeShellServer(t)
	d := newDialerWithTempKnownHosts(t)

	client, err := d.Dial(conn.DialOptions{
		Addr:          srv.Addr(),
		User:          "alice",
		Auth:          config.SSHAuth{Password: "wonderland"},
		HostKeyVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	sid, err := conn.RandomSID()
	if err != nil {
		t.Fatalf("RandomSID: %v", err)
	}
	ptyConn, _, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		client.Close()
		t.Fatalf("NewPtyConn: %v", err)
	}
	return ptyConn, func() { ptyConn.Close() }
}

// runAssert 在已装配的 PtyConn 上执行 cmd，断言 output 含 wantSubstr、
// exitCode 等于 wantCode、未超时。失败打印 output 辅助诊断。
func runAssert(t *testing.T, p *PtyConn, cmd, wantSubstr string, wantCode int) {
	t.Helper()
	output, _, exitCode, timedOut, _, _, _, _, err := p.Run(cmd, 5000, 0)
	if err != nil {
		t.Fatalf("Run(%q): %v", cmd, err)
	}
	if timedOut {
		t.Fatalf("Run(%q) timed out (output: %q)", cmd, output)
	}
	if exitCode != wantCode {
		t.Errorf("Run(%q) exitCode = %d, want %d (output: %q)", cmd, exitCode, wantCode, output)
	}
	if wantSubstr != "" && !strings.Contains(output, wantSubstr) {
		t.Errorf("Run(%q) output should contain %q, got: %q", cmd, wantSubstr, output)
	}
}

// TestAutoCaptureANSIColor 颜色码不破坏 sentinel：输出夹 SGR 颜色序列，
// stripANSI 剥离后正文正确，exit code 正常。
func TestAutoCaptureANSIColor(t *testing.T) {
	p, cleanup := newAutoCapturePty(t)
	defer cleanup()

	// \033[31m RED \033[32m GREEN \033[0m reset
	runAssert(t, p, `printf '\033[31mRED\033[32mGREEN\033[0m done\n'`, "REDGREEN done", 0)
}

// TestAutoCaptureCursorMove 光标移动序列（CUP）不破坏 sentinel。
func TestAutoCaptureCursorMove(t *testing.T) {
	p, cleanup := newAutoCapturePty(t)
	defer cleanup()

	// 光标定位到 5,10 再输出正文
	runAssert(t, p, `printf '\033[5;10Hmoved\n'`, "moved", 0)
}

// TestAutoCaptureAltScreen alt-screen 进出序列不破坏 sentinel。
// 命令进入 alt-screen 输出正文再退出，sentinel 仍应命中。
func TestAutoCaptureAltScreen(t *testing.T) {
	p, cleanup := newAutoCapturePty(t)
	defer cleanup()

	runAssert(t, p, `printf '\033[?1049hinside\033[?1049l\n'`, "inside", 0)
}

// TestAutoCaptureClearScreen 清屏序列（ED + cursor home）不误吞 sentinel。
func TestAutoCaptureClearScreen(t *testing.T) {
	p, cleanup := newAutoCapturePty(t)
	defer cleanup()

	// \033[2J 清屏 \033[H 光标归位，再输出正文
	runAssert(t, p, `printf '\033[2J\033[Hcleared\n'`, "cleared", 0)
}

// TestAutoCaptureMixedCSIOSC CSI + OSC 混合噪声：颜色、OSC 标题设置、
// 光标同时出现，stripANSI 全剥后正文 + exit code 正确。
// OSC 序列以 BEL(\007) 结束，测试 ansiRe 的 OSC 分支。
func TestAutoCaptureMixedCSIOSC(t *testing.T) {
	p, cleanup := newAutoCapturePty(t)
	defer cleanup()

	// OSC 标题 \033]0;title\007 + 颜色 + 正文
	runAssert(t, p, `printf '\033]0;mytitle\007\033[36mbody\033[0m\n'`, "body", 0)
}

// TestAutoCaptureExitCodeInANSINoise ANSI 噪声中 exit code 正确传播：
// 先发颜色序列再返回非 0 退出码，sentinel 仍应捕获真实 exit code。
func TestAutoCaptureExitCodeInANSINoise(t *testing.T) {
	p, cleanup := newAutoCapturePty(t)
	defer cleanup()

	// printf 发颜色后整个命令 exit 1（sh -c 最后一条命令 false 决定退出码）
	runAssert(t, p, `printf '\033[31m'; false`, "", 1)
	// 对照：发颜色后 exit 0
	runAssert(t, p, `printf '\033[32m'; true`, "", 0)
}
