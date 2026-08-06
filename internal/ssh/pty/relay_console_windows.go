//go:build windows

package pty

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// enableVTOutput 在 Windows console output handle (stdout) 上启用
// ENABLE_VIRTUAL_TERMINAL_PROCESSING，返回原 mode 供 restoreOutputMode 恢复。
//
// 背景：Windows console 默认 output mode 含 ENABLE_PROCESSED_OUTPUT，它把裸
// LF (0x0A) 翻译成 CR+LF —— 即光标先回行首再下移一行。而 curses/ncurses 用
// 裸 \n 作为 xterm/Unix 语义的"下移一行、列不变"（廉价相对定位）。两者对 LF
// 语义不一致：Windows 默认把光标甩到列首，导致 htop/vim/tmux 等全屏 TUI 后续
// 字段写到行首、覆盖原字符，渲染错乱。
//
// term.MakeRaw 只设 input handle 的 mode（ENABLE_VIRTUAL_TERMINAL_INPUT 等），
// 从不碰 output handle —— sshmng 的 Relay 仅调 MakeRaw，output mode 继承默认，
// 故此 bug 存在。这里显式对 output handle 开 VT processing：LF 走 xterm 语义
// （列不变），且 ANSI/VT 序列（颜色、光标、alt-screen）由 console 直接解释。
//
// 若 stdout 不是 console（管道/重定向），GetConsoleMode 报错 —— 视为无需处理，
// 返回 nil 让 Relay 继续（非交互场景本就不进 Relay）。
func enableVTOutput() (uint32, error) {
	h := windows.Handle(os.Stdout.Fd())
	var old uint32
	if err := windows.GetConsoleMode(h, &old); err != nil {
		// stdout 不是 console handle（重定向到文件/管道）。非交互场景，
		// 无 LF 翻译问题，跳过。返回 0，restoreOutputMode 检测 0 跳过恢复。
		return 0, nil
	}
	mode := old | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
	if err := windows.SetConsoleMode(h, mode); err != nil {
		return 0, fmt.Errorf("set console output mode: %w", err)
	}
	return old, nil
}

// restoreOutputMode 恢复 enableVTOutput 保存的 output mode。old=0 表示
// 当时未实际修改（非 console handle），跳过。
func restoreOutputMode(old uint32) {
	if old == 0 {
		return
	}
	windows.SetConsoleMode(windows.Handle(os.Stdout.Fd()), old)
}
