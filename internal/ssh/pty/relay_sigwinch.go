//go:build !windows

package pty

import (
	"os"
	"os/signal"
	"syscall"
)

func (p *PtyConn) notifyWindowChange() *windowWatcher {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)

	ww := &windowWatcher{
		ch: make(chan struct{}, 1),
		stop: func() {
			signal.Stop(sigCh)
		},
	}

	go func() {
		for range sigCh {
			select {
			case ww.ch <- struct{}{}:
			default:
			}
		}
	}()

	return ww
}
