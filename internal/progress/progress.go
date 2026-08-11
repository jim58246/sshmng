package progress

import (
	"fmt"
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
	w           io.Writer
	file        *os.File       // the *os.File backing w, if any (for term.GetSize on resize)
	tty         bool
	label       string
	total       int64
	current     int64
	filesTotal  int
	filesDone   int
	status      string
	width       int
	lastLineLen int // display width of the last rendered line, for padding + Finish
	start       time.Time
	lastDraw   time.Time
	now        func() time.Time // injectable for tests; nil => time.Now
	finished   bool
	mu         sync.Mutex
	vtEnabled  bool // VT processing enabled on b.file (stderr); false => \r+pad fallback
}

// NewBar creates a progress bar. total<0 = unknown size (indeterminate mode:
// shows current bytes + rate only, no gauge/percent/ETA). If w is not a
// terminal (or not an *os.File), returns a silent bar whose methods are no-ops.
func NewBar(w io.Writer, label string, total int64) *Bar {
	b := &Bar{w: w, label: label, total: total, width: 80, start: time.Now(), now: time.Now}
	if f, ok := w.(*os.File); ok {
		if term.IsTerminal(int(f.Fd())) {
			b.tty = true
			b.file = f
			b.refreshWidth()
		}
	}
	return b
}

// refreshWidth re-reads the terminal width from the attached *os.File. Called
// on every redraw so the bar adapts to window resize in real time (the width
// captured at NewBar goes stale when the user resizes the terminal mid-transfer).
// No-op when no file is attached (test/non-TTY bars).
func (b *Bar) refreshWidth() {
	if b.file == nil {
		return
	}
	if w, _, err := term.GetSize(int(b.file.Fd())); err == nil && w > 0 {
		b.width = w
	}
}

// clearAndPosition returns the VT control sequence to erase the previous
// rendered line (which may wrap across multiple rows after a terminal shrink)
// and position the cursor at column 0 of the topmost row it occupied, ready
// for the new line to be written. prevLen=0 (first draw) returns just "\r".
//
// Why this is needed: \r alone returns to column 0 of the CURRENT (bottom)
// row. When the previous line wrapped (e.g. the terminal was shrunk below the
// prior line's width), the old content occupies several rows and \r only
// touches the bottom one — the upper rows stay as residue. Moving the cursor
// up to the top of the old content and clearing to end-of-screen erases all of
// it. \x1b[J from column 0 also clears the single-row case (trailing chars),
// replacing space-padding.
//
//	\r            column 0 of current (bottom) row
//	\x1b[<N>A     cursor up N rows (to top of old wrapped content)
//	\x1b[J        erase cursor → end of screen
func clearAndPosition(prevLen, width int) string {
	if prevLen <= 0 {
		return "\r"
	}
	w := width
	if w <= 0 {
		w = defaultBarWidth
	}
	rows := (prevLen + w - 1) / w // ceil(prevLen / w), ≥1
	if rows <= 1 {
		return "\r\x1b[J"
	}
	return "\r" + fmt.Sprintf("\x1b[%dA", rows-1) + "\x1b[J"
}

// padToPrev is the non-VT fallback: appends spaces so the padded line's display
// width reaches prevWidth, overwriting stale tail chars from a prior longer line.
// Used only when VT could not be enabled (rare); it cannot clear wrapped rows.
func padToPrev(line string, prevWidth int) (padded string, curWidth int) {
	curWidth = displayWidth(line)
	if prevWidth > curWidth {
		return line + strings.Repeat(" ", prevWidth-curWidth), curWidth
	}
	return line, curWidth
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
	// Re-read terminal width so the bar adapts to window resize mid-transfer.
	b.refreshWidth()
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
	if b.vtEnabled {
		// VT clear: erase the previous line's full extent (incl. wrapped rows
		// after a shrink) and position at col 0, then write the new line.
		io.WriteString(b.w, clearAndPosition(b.lastLineLen, b.width)+line)
	} else {
		// Fallback (no VT): \r + space-pad to previous width.
		padded, _ := padToPrev(line, b.lastLineLen)
		io.WriteString(b.w, "\r"+padded)
	}
	b.lastLineLen = displayWidth(line)
}

// ensureVT enables Windows console VT processing on b.file (stderr) once
// (idempotent). On Unix no-op. If it fails (very old console / non-console
// handle), vtEnabled stays false and redraw/Finish fall back to \r + space-pad.
func (b *Bar) ensureVT() {
	if !b.tty || b.vtEnabled {
		return
	}
	if _, err := termutil.EnableVTFile(b.file); err == nil {
		b.vtEnabled = true
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
	if b.vtEnabled {
		// Clear the full previous extent (incl. wrapped rows) and leave cursor
		// at col 0 of the top row.
		io.WriteString(b.w, clearAndPosition(b.lastLineLen, b.width))
	} else {
		io.WriteString(b.w, "\r"+strings.Repeat(" ", max(b.width, b.lastLineLen))+"\r")
	}
}
