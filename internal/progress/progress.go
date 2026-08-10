package progress

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jim58246/sshmng/internal/termutil"
	"golang.org/x/term"
)

const throttle = 100 * time.Millisecond

// Bar is a TTY-aware single-line progress bar written to w (expected os.Stderr).
// When w is not a terminal, all methods are no-ops — callers need not check TTY.
type Bar struct {
	w          io.Writer
	tty        bool
	label      string
	total      int64
	current    int64
	filesTotal int
	filesDone  int
	status     string
	width      int
	start      time.Time
	lastDraw   time.Time
	now        func() time.Time // injectable for tests; nil => time.Now
	finished   bool
	mu         sync.Mutex
	vtRestored bool
}

// NewBar creates a progress bar. total<0 = unknown size (indeterminate mode:
// shows current bytes + rate only, no gauge/percent/ETA). If w is not a
// terminal (or not an *os.File), returns a silent bar whose methods are no-ops.
func NewBar(w io.Writer, label string, total int64) *Bar {
	b := &Bar{w: w, label: label, total: total, width: 80, start: time.Now(), now: time.Now}
	if f, ok := w.(*os.File); ok {
		if term.IsTerminal(int(f.Fd())) {
			b.tty = true
			if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
				b.width = w
			}
		}
	}
	return b
}

// SetFiles enables the file-count dimension (directory transfers). totalFiles<=0 disables.
func (b *Bar) SetFiles(totalFiles int) *Bar {
	b.mu.Lock()
	b.filesTotal = totalFiles
	b.mu.Unlock()
	return b
}

// SetBytes sets the absolute transferred-byte count, redrawing (throttled).
func (b *Bar) SetBytes(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.tty || b.finished {
		return
	}
	b.current = n
	b.maybeRedraw()
}

// SetFilesDone sets the absolute completed-file count.
func (b *Bar) SetFilesDone(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.tty || b.finished {
		return
	}
	b.filesDone = n
	b.maybeRedraw()
}

// SetStatus sets the trailing status tag (relay destination status).
func (b *Bar) SetStatus(tags string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.tty || b.finished {
		return
	}
	b.status = tags
	b.maybeRedraw()
}

// maybeRedraw draws the line if the throttle interval has elapsed (or it's the
// first draw, or 100%). Caller must hold b.mu.
func (b *Bar) maybeRedraw() {
	// b.now is injectable for tests; nil => time.Now (the field's documented contract).
	now := time.Now()
	if b.now != nil {
		now = b.now()
	}
	if !b.lastDraw.IsZero() && now.Sub(b.lastDraw) < throttle {
		// Still redraw at 100% completion so the final state is accurate.
		if !(b.total > 0 && b.current >= b.total) {
			return
		}
	}
	b.lastDraw = now
	b.ensureVT()
	line := renderBarLine(barState{
		label:      b.label,
		total:      b.total,
		current:    b.current,
		filesTotal: b.filesTotal,
		filesDone:  b.filesDone,
		status:     b.status,
		width:      b.width,
		elapsed:    now.Sub(b.start),
	})
	// \r returns to column 0; pad with spaces to clear any prior longer line; no newline.
	io.WriteString(b.w, "\r"+line)
}

// ensureVT enables Windows console VT processing once (idempotent). On Unix no-op.
func (b *Bar) ensureVT() {
	if !b.tty || b.vtRestored {
		return
	}
	if _, err := termutil.EnableVTOutput(); err == nil {
		b.vtRestored = true
		// Note: we leave VT enabled for the process lifetime rather than
		// restoring after every redraw. Restoring per-draw would thrash; the
		// flags only affect stdout rendering and are harmless to leave on.
	}
}

// Finish erases the progress line so the final summary (on stdout) is clean.
// Idempotent; safe to defer.
func (b *Bar) Finish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.tty || b.finished {
		b.finished = true
		return
	}
	b.finished = true
	// Clear the whole line: \r + spaces + \r.
	io.WriteString(b.w, "\r"+strings.Repeat(" ", b.width)+"\r")
}
