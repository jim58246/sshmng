package pty

import (
	"os"
	"time"

	"golang.org/x/term"
)

// winChangePollInterval is how often Windows polls the terminal size.
// SIGWINCH doesn't exist on Windows, so we poll instead.
const winChangePollInterval = 200 * time.Millisecond

func (p *PtyConn) notifyWindowChange() *windowWatcher {
	done := make(chan struct{})
	ww := &windowWatcher{
		ch: make(chan struct{}, 1),
		stop: func() {
			close(done)
		},
	}

	go func() {
		fd := int(os.Stdout.Fd())
		prevW, prevH := 0, 0

		ticker := time.NewTicker(winChangePollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				w, h, err := term.GetSize(fd)
				if err != nil {
					continue
				}
				if w != prevW || h != prevH {
					prevW, prevH = w, h
					select {
					case ww.ch <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	return ww
}
