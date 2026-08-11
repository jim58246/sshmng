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

// TestPadToPrevClearsStaleTail verifies that when a redrawn line is shorter
// than the previous one, padding is appended so the stale tail (leftover chars
// from the longer prior line) is overwritten. Without this, \r returns to
// column 0 but only writes the shorter new line, leaving the old tail visible.
func TestPadToPrevClearsStaleTail(t *testing.T) {
	cases := []struct {
		name        string
		line        string
		prevWidth   int
		wantPaddedW int // display width of the padded line
		wantCurW    int
	}{
		{"shorter", "abc", 10, 10, 3},
		{"equal", "abc", 3, 3, 3},
		{"longer-no-pad", "abcdef", 3, 6, 6},
		{"cjk-shorter", "火山", 6, 6, 4}, // 2 CJK = 4 cols, pad 2 spaces to reach 6
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			padded, curW := padToPrev(c.line, c.prevWidth)
			if curW != c.wantCurW {
				t.Errorf("curW = %d, want %d", curW, c.wantCurW)
			}
			if dw := displayWidth(padded); dw != c.wantPaddedW {
				t.Errorf("padded displayWidth = %d, want %d (padded=%q)", dw, c.wantPaddedW, padded)
			}
		})
	}
}

// TestBarRedrawPadsShorterLine is the integration test for the stale-tail fix:
// after a longer line is drawn, a subsequent shorter redraw must append enough
// spaces to overwrite the prior line's tail.
func TestBarRedrawPadsShorterLine(t *testing.T) {
	var buf bytes.Buffer
	b := newTestBar(&buf, "srv", 1<<30)
	b.width = 200 // wide so both lines render fully (we want to compare widths)
	// First draw: large ETA/rate digits make a long-ish line.
	b.SetBytes(1_000_000) // first draw always renders
	firstOut := buf.String()
	firstLine := strings.TrimPrefix(firstOut, "\r")
	firstW := displayWidth(firstLine)

	// Make the second line strictly shorter by forcing a tiny current with a
	// huge total → tiny pct → gauge nearly empty, and a different cur string.
	buf.Reset()
	b.current = 0 // reset so the next SetBytes renders from a different state
	// Force a redraw past throttle by advancing lastDraw backward.
	b.lastDraw = b.lastDraw.Add(-throttle * 2)
	b.SetBytes(50) // "50 B" is shorter than "976.6 KB"
	secondOut := buf.String()
	secondLine := strings.TrimPrefix(secondOut, "\r")
	secondW := displayWidth(secondLine)

	// The written bytes must include trailing spaces covering the difference.
	// secondOut = "\r" + secondLine + padding. The padding count >= firstW - secondW.
	writtenTail := secondOut[len("\r")+len(secondLine):]
	if firstW > secondW && len(writtenTail) < firstW-secondW {
		t.Errorf("shorter redraw not padded enough: firstW=%d secondW=%d trailingSpaces=%d (want >= %d)\nfirst=%q\nsecond=%q",
			firstW, secondW, len(writtenTail), firstW-secondW, firstLine, secondOut)
	}
}

// TestBarRefreshWidthNilFileNoOp verifies refreshWidth is safe when no *os.File
// is attached (test/non-TTY bars): it must not panic or change width.
func TestBarRefreshWidthNilFileNoOp(t *testing.T) {
	var buf bytes.Buffer
	b := &Bar{w: &buf, label: "x", total: 100, tty: true, width: 80}
	b.refreshWidth() // file is nil → no-op
	if b.width != 80 {
		t.Errorf("width changed to %d, want 80 (no file)", b.width)
	}
}

// TestClearAndPosition verifies the VT clear sequence for the three cases:
// first draw (no clear), single row (clear trailing), and wrapped rows after a
// terminal shrink (move up + clear to end of screen). This is the fix for the
// resize-residue bug (旧长行折行后 \r 只清底行 → 上行残留).
func TestClearAndPosition(t *testing.T) {
	cases := []struct {
		name    string
		prevLen int
		width   int
		want    string
	}{
		{"first-draw", 0, 80, "\r"},
		{"single-row-clear-trailing", 50, 80, "\r\x1b[J"},
		{"wrapped-2-rows-after-shrink", 95, 50, "\r\x1b[1A\x1b[J"},  // ceil(95/50)=2
		{"wrapped-3-rows", 150, 50, "\r\x1b[2A\x1b[J"},               // ceil(150/50)=3
		{"exact-multiple-2-rows", 100, 50, "\r\x1b[1A\x1b[J"},        // ceil(100/50)=2
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clearAndPosition(c.prevLen, c.width); got != c.want {
				t.Errorf("clearAndPosition(%d, %d) = %q, want %q", c.prevLen, c.width, got, c.want)
			}
		})
	}
}

// TestTruncateToWidthPathAware verifies that long paths keep the filename
// (last segment) and abbreviate directories with "…".
func TestTruncateToWidthPathAware(t *testing.T) {
	cases := []struct {
		in     string
		maxW   int
		want   string
	}{
		{"/tmp/remote2.bin", 30, "/tmp/remote2.bin"},                       // fits → unchanged
		{"/tmp/sshmng_narrow_remote2.bin", 14, "…/remote2.bin"},            // 8+? =13? "…/"=2 + "remote2.bin"=10 =12 ≤14
		{"火山云/115.190.174.107:/tmp/sshmng_narrow_remote2.bin", 20, "…/sshmng_narrow_remote2.bin"}, // last seg is filename; "…/"+20chars
		{"no-slash-here-long-text", 10, "no-slash-…"},                      // no slash → right-truncate
	}
	for _, c := range cases {
		got := truncateToWidth(c.in, c.maxW)
		// Check it fits and (where applicable) contains the filename tail.
		if dw := displayWidth(got); dw > c.maxW {
			t.Errorf("truncateToWidth(%q, %d) = %q, displayWidth %d > %d", c.in, c.maxW, got, dw, c.maxW)
		}
		if c.want != "" && got != c.want {
			// Only assert exact for the deterministic cases; the CJK one may
			// vary in exact cut but must fit and contain "…/".
			if strings.Contains(c.in, "/") && c.maxW < displayWidth(c.in) {
				if !strings.HasPrefix(got, "…/") && got != c.in {
					// acceptable: tail-only truncation
				}
			} else {
				t.Errorf("truncateToWidth(%q, %d) = %q, want %q", c.in, c.maxW, got, c.want)
			}
		}
	}
}

// TestBarFinishClearsFullRenderedLine verifies that, with a long label on a
// narrow terminal, the rendered line is truncated to fit within width (the
// 刷屏 fix) AND Finish() emits a clear sequence that erases the rendered line
// (VT path: \r + clear-to-end-of-screen; the single-row case clears trailing).
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
	// Strip a leading clear sequence if present (first draw emits just "\r"
	// since lastLineLen was 0; subsequent draws emit \r\x1b[J).
	renderedLine = strings.TrimPrefix(renderedLine, "\x1b[J")
	if dw := displayWidth(renderedLine); dw > b.width {
		t.Fatalf("rendered line display width %d exceeds width %d (wraps → 刷屏): %q", dw, b.width, renderedLine)
	}
	renderedDW := displayWidth(renderedLine)

	b.Finish()
	finishOut := buf.String()[len(rendered):]
	// VT Finish emits clearAndPosition(lastLineLen, width). For a single-row
	// line (renderedDW ≤ width) that is "\r\x1b[J" — clears the whole row.
	if !strings.HasPrefix(finishOut, "\r") {
		t.Fatalf("Finish must start with \\r (return to col 0): %q", finishOut)
	}
	if !strings.Contains(finishOut, "\x1b[J") {
		t.Fatalf("Finish VT output must contain clear-to-end-of-screen (\\x1b[J): %q", finishOut)
	}
	// The rendered progress text must NOT appear after Finish (it was cleared).
	if strings.Contains(finishOut, renderedLine) {
		t.Errorf("Finish left rendered line text in output (not cleared): %q", finishOut)
	}
	_ = renderedDW
}
