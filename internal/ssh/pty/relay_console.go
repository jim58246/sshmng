//go:build !windows

package pty

// enableVTOutput 在 Unix 上是 no-op：POSIX 终端原生 LF 语义就是"下移一行、
// 列不变"（与 xterm/curses 一致），无 Windows console 的 LF→CR+LF 翻译问题。
// 返回 0；restoreOutputMode(0) 同样 no-op。
//
// 仅 Windows 需要 ENABLE_VIRTUAL_TERMINAL_PROCESSING（见 relay_console_windows.go）。
func enableVTOutput() (uint32, error) { return 0, nil }

// restoreOutputMode 在 Unix 上是 no-op。
func restoreOutputMode(uint32) {}
