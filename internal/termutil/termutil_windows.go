//go:build windows

package termutil

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// EnableVTFile enables ENABLE_VIRTUAL_TERMINAL_PROCESSING |
// DISABLE_NEWLINE_AUTO_RETURN on the Windows console output handle backing f
// (e.g. os.Stdout or os.Stderr), returning the previous mode for RestoreVTFile.
//
// This is the per-handle core; EnableVTOutput/RestoreOutputMode delegate to it
// with os.Stdout for backward compatibility with pty.Relay.
//
// If f is not a console handle (redirected to a file/pipe), GetConsoleMode
// reports an error — treated as "nothing to do" (non-interactive), returns
// (0, nil); RestoreVTFile sees old==0 and skips.
//
// Both flags are required (matching OpenSSH Windows console.c ConEnterRawMode):
//   - ENABLE_VIRTUAL_TERMINAL_PROCESSING (0x4): interpret ANSI/VT sequences
//     (colors, cursor movement, clear-line/screen, alt-screen).
//   - DISABLE_NEWLINE_AUTO_RETURN (0x8): stop LF from auto-CR so bare \n is
//     pure xterm "down-one-line, column-unchanged" (curses relative positioning).
//
// Flags are OR'd onto the current GetConsoleMode mode (not hardcoded) to keep
// defaults (PROCESSED_OUTPUT | WRAP_AT_EOL_OUTPUT) and any ConPTY/third-party
// flags. Equivalent to OpenSSH's
// dwAttributes |= VT_PROCESSING | DISABLE_NEWLINE_AUTO_RETURN.
func EnableVTFile(f *os.File) (uint32, error) {
	if f == nil {
		return 0, nil
	}
	h := windows.Handle(f.Fd())
	var old uint32
	if err := windows.GetConsoleMode(h, &old); err != nil {
		// Not a console handle (redirected). Non-interactive; nothing to do.
		return 0, nil
	}
	mode := old | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
	if err := windows.SetConsoleMode(h, mode); err != nil {
		return 0, fmt.Errorf("set console output mode: %w", err)
	}
	return old, nil
}

// RestoreVTFile restores the console output mode saved by EnableVTFile on the
// same handle. old=0 means no modification was made; skip.
func RestoreVTFile(f *os.File, old uint32) {
	if old == 0 || f == nil {
		return
	}
	windows.SetConsoleMode(windows.Handle(f.Fd()), old)
}

// EnableVTOutput enables VT on stdout (pty.Relay's output). Kept for backward
// compatibility; delegates to EnableVTFile(os.Stdout).
func EnableVTOutput() (uint32, error) {
	return EnableVTFile(os.Stdout)
}

// RestoreOutputMode restores stdout's mode saved by EnableVTOutput.
func RestoreOutputMode(old uint32) {
	RestoreVTFile(os.Stdout, old)
}
