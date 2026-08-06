package pty

import (
	"context"
	"fmt"
	"os"
	"time"

	"golang.org/x/term"
)

// Relay switches the PTY to interactive mode: enables remote echo, puts the
// local terminal into raw mode, and relays bytes bidirectionally between the
// user's terminal and the SSH session. Blocks until the session ends or the
// context is cancelled.
//
// Must be called after any LoginFlow is complete. Does NOT call
// DetectShell/InjectRC - those are for automated command execution and would
// break the interactive experience (sentinel PS1, stty -echo).
//
// On return the local terminal is restored and the PtyConn is closed.
func (p *PtyConn) Relay(ctx context.Context) error {
	defer p.Close()

	// Flush any pushback data from LoginFlow to the user's terminal.
	p.mu.Lock()
	if len(p.pushback) > 0 {
		os.Stdout.Write(p.pushback)
		p.pushback = nil
	}
	p.mu.Unlock()

	// Enable remote echo so the user sees what they type.
	p.stdin.Write([]byte("stty echo\n"))
	time.Sleep(50 * time.Millisecond)

	// Windows: 启用 console output handle 的 VT processing，让裸 LF 走 xterm
	// 语义（下移一行、列不变），避免 Windows 默认 LF→CR+LF 把光标甩到行首
	// 导致全屏 TUI 渲染错乱。Unix 为 no-op。详见 relay_console_windows.go。
	oldOutMode, err := enableVTOutput()
	if err != nil {
		return fmt.Errorf("enable vt output: %w", err)
	}
	defer restoreOutputMode(oldOutMode)

	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("set terminal raw mode: %w", err)
	}
	defer term.Restore(fd, oldState)

	// Relay stdoutCh -> os.Stdout.
	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		for {
			select {
			case data, ok := <-p.stdoutCh:
				if !ok {
					return
				}
				os.Stdout.Write(data)
			case <-p.doneCh:
				return
			}
		}
	}()

	// Relay os.Stdin -> p.stdin.
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if _, werr := p.stdin.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	p.sendWindowSize()
	ww := p.notifyWindowChange()
	defer ww.stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-stdoutDone:
			return nil
		case <-stdinDone:
			return nil
		case <-ww.ch:
			p.sendWindowSize()
		}
	}
}

func (p *PtyConn) sendWindowSize() {
	fd := int(os.Stdout.Fd())
	w, h, err := term.GetSize(fd)
	if err != nil {
		return
	}
	p.mu.Lock()
	session := p.session
	p.mu.Unlock()
	if session != nil {
		session.WindowChange(h, w)
	}
}
