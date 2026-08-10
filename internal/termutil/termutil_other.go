//go:build !windows

package termutil

// EnableVTOutput 在 Unix 上是 no-op：POSIX 终端原生 LF 语义就是"下移一行、
// 列不变"（与 xterm/curses 一致），无 Windows console 的 LF→CR+LF 翻译问题。
// 返回 0；RestoreOutputMode(0) 同样 no-op。
//
// 仅 Windows 需要 ENABLE_VIRTUAL_TERMINAL_PROCESSING（见 termutil_windows.go）。
func EnableVTOutput() (uint32, error) { return 0, nil }

// RestoreOutputMode 在 Unix 上是 no-op。
func RestoreOutputMode(uint32) {}
