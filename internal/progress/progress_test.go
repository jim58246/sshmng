package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// newTestBar builds a Bar writing to a buffer in TTY mode (bypassing the
// *os.File/term.IsTerminal detection that would silence a bytes.Buffer).
func newTestBar(buf *bytes.Buffer, label string, total int64) *Bar {
	b := &Bar{w: buf, label: label, total: total, tty: true, width: 80, start: time.Now()}
	return b
}

func TestBarSilentWhenNotTTY(t *testing.T) {
	var buf bytes.Buffer
	b := &Bar{w: &buf, label: "x", total: 100, tty: false}
	b.SetBytes(50)
	b.Finish()
	if buf.Len() != 0 {
		t.Errorf("silent bar wrote %q, want empty", buf.String())
	}
}

func TestBarSetBytesRendersAndFinishErases(t *testing.T) {
	var buf bytes.Buffer
	b := newTestBar(&buf, "web-01", 100)
	b.SetBytes(50)
	out := buf.String()
	if !strings.Contains(out, "50%") {
		t.Errorf("after SetBytes(50), output missing 50%%: %q", out)
	}
	b.Finish()
	// Finish should overwrite with blank (carriage return + spaces).
	if !strings.HasPrefix(buf.String(), "\r") {
		t.Errorf("expected line to start with \\r: %q", buf.String())
	}
}

func TestBarThrottleSkipsRedrawWithinInterval(t *testing.T) {
	var buf bytes.Buffer
	b := newTestBar(&buf, "srv", 1000)
	b.SetBytes(10) // first draw always renders
	firstLen := buf.Len()
	b.SetBytes(20) // within throttle window → skipped
	if buf.Len() != firstLen {
		t.Errorf("throttled redraw wrote extra bytes (len %d → %d)", firstLen, buf.Len())
	}
}

func TestBarUnknownSizeNoCrash(t *testing.T) {
	var buf bytes.Buffer
	b := newTestBar(&buf, "srv", -1)
	b.SetBytes(123456)
	b.Finish()
	if !strings.Contains(buf.String(), "120") { // 123456 B ≈ 120 KB
		t.Errorf("unknown-size render unexpected: %q", buf.String())
	}
}

// TestBarFinishClearsFullRenderedLine verifies that, with a long label on a
// narrow terminal, the rendered line is truncated to fit within width (the
// 刷屏 fix) AND Finish() erases the full rendered line (no trailing residue).
func TestBarFinishClearsFullRenderedLine(t *testing.T) {
	var buf bytes.Buffer
	b := &Bar{
		w:     &buf,
		label: "very-long-server-name-host:/some/very/long/remote/path/to/file.dat",
		total: 100, tty: true, width: 20, start: time.Now(),
	}
	b.SetBytes(50) // first draw always renders (lastDraw zero)
	rendered := buf.String()
	renderedLine := strings.TrimPrefix(rendered, "\r")
	// The fix: the line MUST fit within the terminal width (else it wraps and
	// \r-refresh floods the screen).
	if dw := displayWidth(renderedLine); dw > b.width {
		t.Fatalf("rendered line display width %d exceeds width %d (wraps → 刷屏): %q", dw, b.width, renderedLine)
	}
	wantClear := displayWidth(renderedLine)

	b.Finish()
	// Finish appended "\r" + spaces + "\r".
	finishOut := buf.String()[len(rendered):]
	if !strings.HasPrefix(finishOut, "\r") || !strings.HasSuffix(finishOut, "\r") {
		t.Fatalf("Finish output malformed: %q", finishOut)
	}
	spaces := finishOut[1 : len(finishOut)-1]
	if len(spaces) < wantClear {
		t.Errorf("Finish cleared %d spaces, but rendered line was %d cols (residue left)", len(spaces), wantClear)
	}
}
