package pty

// windowWatcher manages terminal resize notifications.
// ch receives a value each time the terminal size changes.
// stop terminates the underlying watcher (goroutine / signal handler).
type windowWatcher struct {
	ch   chan struct{}
	stop func()
}
