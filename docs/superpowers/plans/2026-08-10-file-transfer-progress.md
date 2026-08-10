# `sshmng file` Transfer Progress Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add TTY-aware single-line progress bars to all five `sshmng file` subcommands (upload/download/upload-dir/download-dir/relay), auto-silent on non-TTY, with no speed regression.

**Architecture:** Progress is a pure CLI-layer concern. A new `internal/progress` package renders bars to stderr and provides counting reader/writer wrappers; the transfer layer only gains optional `OnProgress` callbacks (nil = current silent behavior). Windows VT flags are extracted into a shared `internal/termutil` package used by both `pty.Relay` and `progress.Bar`. MCP paths are untouched.

**Tech Stack:** Go 1.25.12, `golang.org/x/term` (TTY detect + size), `golang.org/x/sys/windows` (VT flags, Windows-only). No new dependencies.

## Global Constraints

- **No new dependencies.** Use existing `golang.org/x/term` and `golang.org/x/sys` only.
- **Progress → stderr; final summary → stdout** (the `out io.Writer` already threaded through `file_cmd.go`).
- **Non-TTY → silent.** All progress methods no-op when stderr is not a terminal (scripts/CI/tests unaffected).
- **No speed regression.** Progress is decoration on the transfer link; worst case = current speed + negligible bookkeeping. The UploadSized pipelining assumption is validated by a benchmark (Task 4).
- **MCP path unchanged.** `Conn` interface, `Session` methods, and MCP tool handlers are not modified. New `OnProgress` field is zero-value-nil (source-compatible with existing fakes). `RelayTransferWithProgress` is a new method; `RelayTransfer` stays as-is.
- **Bilingual docs:** CLAUDE.md requires English/zh-CN doc mirroring. This plan changes no CLI signatures or usage text, so no doc edits are required. If any usage text is touched, mirror to `docs/zh-CN/` and `README.zh-CN.md`.
- **Commit style:** end commit messages with `Co-Authored-By: Claude <noreply@anthropic.com>`. Push direct to `main` (per user preference — no PRs).

---

## File Structure

| File | Responsibility | Action |
|------|---------------|--------|
| `internal/termutil/termutil_windows.go` | Windows VT flag enable/restore | Create (extracted from `pty/relay_console_windows.go`) |
| `internal/termutil/termutil_other.go` | Unix no-op VT stub | Create (extracted from `pty/relay_console.go`) |
| `internal/ssh/pty/relay_console_windows.go` | (deleted) | Remove |
| `internal/ssh/pty/relay_console.go` | (deleted) | Remove |
| `internal/ssh/pty/relay.go` | Call `termutil.EnableVTOutput` instead of local fn | Modify (2 lines) |
| `internal/progress/progress.go` | `Bar` renderer: TTY detect, throttle, render, Finish | Create |
| `internal/progress/counting.go` | `CountingReader` / `CountingWriter` | Create |
| `internal/progress/format.go` | Pure helpers: `humanizeBytes`, `renderBarLine` | Create |
| `internal/progress/progress_test.go` | Bar behavior tests (TTY silence, throttle, finish) | Create |
| `internal/progress/format_test.go` | Pure helper tests | Create |
| `internal/progress/counting_test.go` | Counting reader/writer tests | Create |
| `internal/ssh/pty/sftp_bench_test.go` | Add `BenchmarkSftpUploadSizedCounting` | Modify |
| `internal/ssh/conn/sftp_dir.go` | Add `OnProgress` field to `DirTransferOptions` | Modify |
| `internal/ssh/pty/sftp_dir.go` | Wire `OnProgress` in `UploadDir`/`DownloadDir` worker pools | Modify |
| `internal/ssh/session/relay.go` | Add `RelayTransferWithProgress` | Modify |
| `internal/cli/file_cmd.go` | Wire progress bars into all 5 subcommands | Modify |

---

### Task 1: Extract Windows VT flags into `internal/termutil`

**Files:**
- Create: `internal/termutil/termutil_windows.go`
- Create: `internal/termutil/termutil_other.go`
- Remove: `internal/ssh/pty/relay_console_windows.go`
- Remove: `internal/ssh/pty/relay_console.go`
- Modify: `internal/ssh/pty/relay.go:40,44`
- Test: existing `internal/ssh/pty/*_test.go` (regression gate — Relay still compiles & tests pass)

**Interfaces:**
- Produces: `termutil.EnableVTOutput() (uint32, error)`, `termutil.RestoreOutputMode(old uint32)` — exported, build-tagged. Windows impl ORs `ENABLE_VIRTUAL_TERMINAL_PROCESSING | DISABLE_NEWLINE_AUTO_RETURN` onto stdout's console mode; Unix impl is no-op returning `(0, nil)` / ignoring arg.

- [ ] **Step 1: Create `internal/termutil/termutil_windows.go`**

Copy the body of `internal/ssh/pty/relay_console_windows.go` verbatim, changing only: `package pty` → `package termutil`, and export the two functions (`enableVTOutput` → `EnableVTOutput`, `restoreOutputMode` → `RestoreOutputMode`). Update internal call sites within the file accordingly. Keep the full comment block (it documents the OpenSSH-derived flag reasoning). Keep `//go:build windows`.

```go
//go:build windows

package termutil

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// EnableVTOutput enables ENABLE_VIRTUAL_TERMINAL_PROCESSING |
// DISABLE_NEWLINE_AUTO_RETURN on the Windows console output handle (stdout),
// returning the previous mode for RestoreOutputMode. ... (keep full comment from source)
func EnableVTOutput() (uint32, error) {
	h := windows.Handle(os.Stdout.Fd())
	var old uint32
	if err := windows.GetConsoleMode(h, &old); err != nil {
		return 0, nil
	}
	mode := old | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
	if err := windows.SetConsoleMode(h, mode); err != nil {
		return 0, fmt.Errorf("set console output mode: %w", err)
	}
	return old, nil
}

// RestoreOutputMode restores the console output mode saved by EnableVTOutput.
// old=0 means no modification was made (non-console handle); skip.
func RestoreOutputMode(old uint32) {
	if old == 0 {
		return
	}
	windows.SetConsoleMode(windows.Handle(os.Stdout.Fd()), old)
}
```

- [ ] **Step 2: Create `internal/termutil/termutil_other.go`**

```go
//go:build !windows

package termutil

// EnableVTOutput is a no-op on Unix: POSIX terminals already use xterm LF
// semantics (down-one-line, column unchanged). Returns 0; RestoreOutputMode(0)
// is likewise a no-op.
func EnableVTOutput() (uint32, error) { return 0, nil }

// RestoreOutputMode is a no-op on Unix.
func RestoreOutputMode(uint32) {}
```

- [ ] **Step 3: Delete the two `pty/relay_console*.go` files**

```bash
git rm internal/ssh/pty/relay_console_windows.go internal/ssh/pty/relay_console.go
```

- [ ] **Step 4: Update `internal/ssh/pty/relay.go` to call `termutil`**

Add `"github.com/jim58246/sshmng/internal/termutil"` to the import block. Change the two call sites (currently lines 40 and 44):

```go
	oldOutMode, err := termutil.EnableVTOutput()
```
```go
	defer termutil.RestoreOutputMode(oldOutMode)
```

- [ ] **Step 5: Build and run existing tests as regression gate**

Run: `go build ./... && go test ./internal/ssh/pty/...`
Expected: build succeeds (no reference to deleted symbols), all existing pty tests pass. The Relay behavior is unchanged — same flags, same functions, just relocated and exported.

- [ ] **Step 6: Verify cross-platform build tags compile**

Run: `GOOS=windows go build ./internal/termutil/ && GOOS=linux go build ./internal/termutil/`
Expected: both compile without error (windows picks `termutil_windows.go`, linux picks `termutil_other.go`).

- [ ] **Step 7: Commit**

```bash
git add internal/termutil/ internal/ssh/pty/relay.go
git rm internal/ssh/pty/relay_console_windows.go internal/ssh/pty/relay_console.go
git commit -m "$(cat <<'EOF'
refactor: extract Windows VT flags into shared internal/termutil

Move enableVTOutput/restoreOutputMode out of pty into internal/termutil
so progress.Bar can reuse the same VT setup. No behavior change to Relay.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Pure progress helpers (`humanizeBytes`, `renderBarLine`) + counting wrappers

**Files:**
- Create: `internal/progress/counting.go`
- Create: `internal/progress/format.go`
- Test: `internal/progress/counting_test.go`
- Test: `internal/progress/format_test.go`

**Interfaces:**
- Produces:
  - `type CountingReader struct { R io.Reader; Fn func(int64) }` with `Read(p []byte) (int, error)`
  - `type CountingWriter struct { W io.Writer; Fn func(int64) }` with `Write(p []byte) (int, error)` — Fn receives **cumulative** bytes transferred so far.
  - `func humanizeBytes(n int64) string` — e.g. `12.3 MB`, `980 B`, `1.0 GB`.
  - `type barState struct` + `func renderBarLine(s barState) string` — pure render of one progress line (no I/O).

- [ ] **Step 1: Write failing tests for CountingReader/CountingWriter**

`internal/progress/counting_test.go`:
```go
package progress

import (
	"bytes"
	"io"
	"testing"
)

func TestCountingReaderCumulative(t *testing.T) {
	var seen []int64
	r := &CountingReader{R: bytes.NewReader([]byte("hello world")), Fn: func(n int64) { seen = append(seen, n) }}
	buf := make([]byte, 5)
	n, err := r.Read(buf)
	if err != nil || n != 5 || string(buf) != "hello" {
		t.Fatalf("Read = (%d, %v, %q), want (5, nil, hello)", n, err, buf)
	}
	n2, _ := r.Read(buf)
	if seen[len(seen)-1] != int64(n+n2) {
		t.Errorf("last cumulative = %d, want %d", seen[len(seen)-1], n+n2)
	}
}

func TestCountingReaderNilFnNoPanic(t *testing.T) {
	r := &CountingReader{R: bytes.NewReader([]byte("x"))}
	if _, err := r.Read(make([]byte, 1)); err != nil {
		t.Fatalf("Read with nil Fn: %v", err)
	}
}

func TestCountingWriterCumulative(t *testing.T) {
	var seen []int64
	var dst bytes.Buffer
	w := &CountingWriter{W: &dst, Fn: func(n int64) { seen = append(seen, n) }}
	n, err := w.Write([]byte("abc"))
	if err != nil || n != 3 || dst.String() != "abc" {
		t.Fatalf("Write = (%d, %v, %q), want (3, nil, abc)", n, err, dst.String())
	}
	w.Write([]byte("de"))
	if seen[len(seen)-1] != 5 {
		t.Errorf("cumulative after 2nd write = %d, want 5", seen[len(seen)-1])
	}
}

func TestCountingWriterPassesUnderlyingWriteError(t *testing.T) {
	w := &CountingWriter{W: &errWriter{}, Fn: func(int64) {}}
	if _, err := w.Write([]byte("x")); err != errSentinel {
		t.Fatalf("expected underlying error to pass through, got %v", err)
	}
}

type errWriter struct{}
var errSentinel = io.ErrShortWrite
func (*errWriter) Write([]byte) (int, error) { return 0, errSentinel }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/progress/...`
Expected: FAIL — package doesn't exist / symbols undefined.

- [ ] **Step 3: Implement `internal/progress/counting.go`**

```go
package progress

import "io"

// CountingReader wraps an io.Reader, invoking Fn with the cumulative byte count
// after each Read. Fn==nil is allowed (no-op). Used to feed Bar.SetBytes during
// uploads without touching the underlying reader's method set (preserves sftp
// pipelining trigger on *io.LimitReader).
type CountingReader struct {
	R  io.Reader
	n  int64
	Fn func(int64)
}

func (c *CountingReader) Read(p []byte) (int, error) {
	n, err := c.R.Read(p)
	if n > 0 {
		c.n += int64(n)
		if c.Fn != nil {
			c.Fn(c.n)
		}
	}
	return n, err
}

// CountingWriter wraps an io.Writer, invoking Fn with the cumulative byte count
// after each Write. Fn==nil is allowed.
type CountingWriter struct {
	W  io.Writer
	n  int64
	Fn func(int64)
}

func (c *CountingWriter) Write(p []byte) (int, error) {
	n, err := c.W.Write(p)
	if n > 0 {
		c.n += int64(n)
		if c.Fn != nil {
			c.Fn(c.n)
		}
	}
	return n, err
}
```

- [ ] **Step 4: Run counting tests to verify they pass**

Run: `go test ./internal/progress/ -run Counting`
Expected: PASS.

- [ ] **Step 5: Write failing tests for `humanizeBytes` and `renderBarLine`**

`internal/progress/format_test.go`:
```go
package progress

import (
	"strings"
	"testing"
	"time"
)

func TestHumanizeBytes(t *testing.T) {
	cases := []struct{ in int64; want string }{
		{0, "0 B"},
		{980, "980 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{12_300_000, "11.7 MB"},   // 11730.19 KB → 11.7 MB
		{25 * 1<<20, "25.0 MB"},   // 25 MiB
		{1 << 30, "1.0 GB"},
	}
	for _, c := range cases {
		if got := humanizeBytes(c.in); got != c.want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderBarLineKnownSize(t *testing.T) {
	s := barState{label: "web-01", total: 100, current: 50, width: 60, elapsed: 5 * time.Second}
	line := renderBarLine(s)
	// 50% => percent string present, both humanized byte counts present.
	if !strings.Contains(line, "50%") {
		t.Errorf("missing 50%%: %q", line)
	}
	if !strings.Contains(line, "web-01") {
		t.Errorf("missing label: %q", line)
	}
}

func TestRenderBarLineUnknownSizeNoPercent(t *testing.T) {
	s := barState{label: "web-01", total: -1, current: 12_000_000, width: 60, elapsed: 3 * time.Second}
	line := renderBarLine(s)
	if strings.Contains(line, "%") {
		t.Errorf("unknown size must not show percent: %q", line)
	}
	if !strings.Contains(line, "11.4 MB") { // 12000000 → 11.4 MB
		t.Errorf("missing current bytes: %q", line)
	}
}

func TestRenderBarLineFilesTag(t *testing.T) {
	s := barState{label: "srv", total: 100, current: 50, width: 80,
		filesTotal: 10, filesDone: 4, elapsed: 2 * time.Second}
	line := renderBarLine(s)
	if !strings.Contains(line, "[4/10 files]") {
		t.Errorf("missing files tag: %q", line)
	}
}

func TestRenderBarLineStatusTag(t *testing.T) {
	s := barState{label: "srv", total: 100, current: 60, width: 80,
		status: "2✓ 1✗ 1⏳", elapsed: 2 * time.Second}
	line := renderBarLine(s)
	if !strings.Contains(line, "[2✓ 1✗ 1⏳]") {
		t.Errorf("missing status tag: %q", line)
	}
}
```

- [ ] **Step 6: Run tests to verify they fail**

Run: `go test ./internal/progress/ -run 'Humanize|Render'`
Expected: FAIL — `humanizeBytes`, `barState`, `renderBarLine` undefined.

- [ ] **Step 7: Implement `internal/progress/format.go`**

```go
package progress

import (
	"fmt"
	"strings"
	"time"
)

// barState is the pure input to renderBarLine. Kept separate from Bar so the
// render function is trivially unit-testable with no I/O or time dependency.
type barState struct {
	label      string
	total      int64        // <0 = unknown size
	current    int64
	filesTotal int          // 0 = no files dimension
	filesDone  int
	status     string       // relay dest-status tag, e.g. "2✓ 1✗ 1⏳"; "" = none
	width      int          // terminal columns; <=0 => default 80
	elapsed    time.Duration // for rate + ETA
}

const defaultBarWidth = 80
const barGaugeWidth = 20 // width of the █░ gauge itself

func humanizeBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	f := float64(n) / 1024
	idx := 0
	for f >= 1024 && idx < len(units)-1 {
		f /= 1024
		idx++
	}
	return fmt.Sprintf("%.1f %s", f, units[idx])
}

// renderBarLine produces one progress line (no leading \r, no trailing newline).
// Layout (known size):  <label>  <gauge>  <pct>  <cur>/<total>  <rate>  ETA <s>  [tags]
// Layout (unknown):     <label>  <cur>  <rate>
func renderBarLine(s barState) string {
	width := s.width
	if width <= 0 {
		width = defaultBarWidth
	}
	curStr := humanizeBytes(s.current)
	rate := float64(0)
	if s.elapsed > 0 {
		rate = float64(s.current) / s.elapsed.Seconds()
	}
	rateStr := humanizeBytes(int64(rate)) + "/s"

	var tags string
	if s.filesTotal > 0 {
		tags += fmt.Sprintf("  [%d/%d files]", s.filesDone, s.filesTotal)
	}
	if s.status != "" {
		tags += fmt.Sprintf("  [%s]", s.status)
	}

	// Unknown size: no gauge, no percent, no ETA.
	if s.total < 0 {
		return fmt.Sprintf("%s  %s  %s%s", s.label, curStr, rateStr, tags)
	}

	pct := 0
	if s.total > 0 {
		pct = int(float64(s.current) / float64(s.total) * 100)
	}
	if pct > 100 {
		pct = 100
	}
	totalStr := humanizeBytes(s.total)

	// Gauge: fill pct% of barGaugeWidth with █, rest with ░.
	filled := barGaugeWidth * pct / 100
	gauge := strings.Repeat("█", filled) + strings.Repeat("░", barGaugeWidth-filled)

	etaStr := "—"
	if rate > 0 && s.current < s.total {
		remaining := time.Duration(float64(s.total-s.current)/rate) * time.Second
		etaStr = fmt.Sprintf("%ds", int(remaining.Seconds()))
	}

	return fmt.Sprintf("%s  %s  %d%%  %s/%s  %s  ETA %s%s",
		s.label, gauge, pct, curStr, totalStr, rateStr, etaStr, tags)
}
```

- [ ] **Step 8: Run all progress tests to verify they pass**

Run: `go test ./internal/progress/...`
Expected: PASS (counting + humanize + render).

- [ ] **Step 9: Commit**

```bash
git add internal/progress/
git commit -m "$(cat <<'EOF'
feat(progress): add counting wrappers and pure render helpers

CountingReader/CountingWriter feed cumulative byte counts to a callback
without altering the underlying reader's method set (preserves sftp
pipelining). humanizeBytes + renderBarLine are pure and fully unit-tested.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: `progress.Bar` — TTY-aware, throttled renderer

**Files:**
- Create: `internal/progress/progress.go`
- Test: `internal/progress/progress_test.go`

**Interfaces:**
- Consumes: `renderBarLine`, `barState` (from Task 2), `termutil.EnableVTOutput`/`RestoreOutputMode` (from Task 1), `term.IsTerminal`/`term.GetSize` from `golang.org/x/term`.
- Produces:
  - `type Bar struct`
  - `func NewBar(w io.Writer, label string, total int64) *Bar` — auto-detects TTY via `*os.File` type assertion + `term.IsTerminal`; non-TTY (or non-*os.File writer) → silent bar.
  - `func (b *Bar) SetFiles(totalFiles int) *Bar`
  - `func (b *Bar) SetBytes(n int64)`
  - `func (b *Bar) SetFilesDone(n int)`
  - `func (b *Bar) SetStatus(tags string)`
  - `func (b *Bar) Finish()` — erase the progress line; idempotent.

- [ ] **Step 1: Write failing tests for Bar**

`internal/progress/progress_test.go`:
```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/progress/ -run Bar`
Expected: FAIL — `Bar` type undefined, `newTestBar` references missing fields.

- [ ] **Step 3: Implement `internal/progress/progress.go`**

```go
package progress

import (
	"io"
	"os"
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
	now := b.now()
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
```

Add `"strings"` to the import block (used in `Finish`).

- [ ] **Step 4: Run Bar tests to verify they pass**

Run: `go test ./internal/progress/ -run Bar`
Expected: PASS.

- [ ] **Step 5: Run the full progress package**

Run: `go test ./internal/progress/...`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/progress/progress.go internal/progress/progress_test.go
git commit -m "$(cat <<'EOF'
feat(progress): add TTY-aware throttled Bar renderer

Bar auto-detects TTY via *os.File + term.IsTerminal; non-TTY (pipes, CI,
tests with buffers) yields a silent bar whose methods are no-op. Redraw is
throttled to ~10fps; Finish erases the line so stdout summaries stay clean.
Windows VT flags are enabled once via shared termutil.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Benchmark — validate UploadSized+CountingReader does not regress pipelining

**Files:**
- Modify: `internal/ssh/pty/sftp_bench_test.go`

**Interfaces:**
- Consumes: `progress.CountingReader` (Task 2), existing `BenchmarkSftpUpload` helpers (`newFakeShellServerWithSftp`, `newDialerWithTempKnownHosts`).
- Produces: `BenchmarkSftpUploadSizedCounting` — a benchmark proving `UploadSized(CountingReader, size)` throughput is within the same order of magnitude as `Upload(*os.File)`.

- [ ] **Step 1: Add the benchmark**

Append to `internal/ssh/pty/sftp_bench_test.go`:
```go
import "github.com/jim58246/sshmng/internal/progress"

// BenchmarkSftpUploadSizedCounting validates the core no-regression claim of
// the progress feature: wrapping *os.File in a CountingReader and uploading via
// UploadSized must stay on sftp's concurrent pipelining path (*io.LimitedReader
// type switch), NOT degrade to serial writeChunkAt.
//
// Expected: throughput within the same order of magnitude as BenchmarkSftpUpload
// (which uses *os.File directly). If this regresses by ~10x, CountingReader
// must forward Stat() (see ctxReaderWithStat) — that is the documented fallback.
func BenchmarkSftpUploadSizedCounting(b *testing.B) {
	srv := newFakeShellServerWithSftp(b)
	d := newDialerWithTempKnownHosts(b)
	client, err := d.Dial(conn.DialOptions{
		Addr:          srv.Addr(),
		User:          "alice",
		Auth:          config.SSHAuth{Password: "wonderland"},
		HostKeyVerify: true,
	})
	if err != nil {
		b.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		b.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	data := bytes.Repeat([]byte("x"), 4<<20) // 4MB
	tmp, err := os.CreateTemp("", "bench-upload-sized")
	if err != nil {
		b.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		b.Fatalf("Write tmp: %v", err)
	}
	if err := tmp.Close(); err != nil {
		b.Fatalf("Close tmp: %v", err)
	}
	fi, _ := os.Stat(tmp.Name)
	size := int64(-1)
	if fi != nil {
		size = fi.Size()
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		f, err := os.Open(tmp.Name())
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		cr := &progress.CountingReader{R: f, Fn: func(int64) {}}
		n, _, err := p.UploadSized(cr, size, "/bench_sized.txt", 60000)
		f.Close()
		if err != nil {
			b.Fatalf("UploadSized: %v (bytes=%d)", err, n)
		}
	}
}
```

- [ ] **Step 2: Run the benchmarks and compare**

Run: `go test -run='^$' -bench='BenchmarkSftpUpload$|BenchmarkSftpUploadSizedCounting$' -benchtime=2s ./internal/ssh/pty/`
Expected: both run; `BenchmarkSftpUploadSizedCounting` throughput (MB/s via `b.SetBytes`) is within the same order of magnitude as `BenchmarkSftpUpload`. Note the actual numbers in the commit message.

- [ ] **Step 3: Decide on fallback based on results**

- If `UploadSized+CountingReader` is within ~2x of `Upload(*os.File)`: assumption confirmed, proceed as-designed.
- If it regresses ~10x (serial path): implement the fallback — make `CountingReader` forward `Stat()` when the underlying reader supports it:
  ```go
  func (c *CountingReader) Stat() (os.FileInfo, error) {
      if s, ok := c.R.(interface{ Stat() (os.FileInfo, error) }); ok {
          return s.Stat()
      }
      return nil, os.ErrNotExist
  }
  ```
  Re-run the bench to confirm recovery, and add a test asserting `CountingReader` wrapping `*os.File` exposes `Stat()`.

- [ ] **Step 4: Commit (include observed numbers)**

```bash
git add internal/ssh/pty/sftp_bench_test.go
git commit -m "$(cat <<'EOF'
test(sftp): bench UploadSized+CountingReader to guard pipelining

Validates that wrapping *os.File in progress.CountingReader and uploading
via UploadSized stays on sftp's concurrent path (*io.LimitedReader switch).
Observed: <fill in Upload MB/s vs UploadSized+Counting MB/s>.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```
(Replace `<fill in ...>` with the actual measured numbers before committing.)

---

### Task 5: Wire progress bars into single-file upload/download CLI

**Files:**
- Modify: `internal/cli/file_cmd.go` — `runFileUpload` (around line 137-154) and `runFileDownload` (around line 194-211).

**Interfaces:**
- Consumes: `progress.NewBar`, `progress.CountingReader`, `progress.CountingWriter` (Tasks 2-3); `ptyConn.UploadSized`, `ptyConn.Stat`, `ptyConn.Download` (existing, confirmed in `internal/ssh/pty/sftp.go`).
- Produces: `runFileUpload`/`runFileDownload` now render a progress bar to stderr during transfer; stdout summary unchanged.

- [ ] **Step 1: Modify `runFileUpload` to use UploadSized + CountingReader + Bar**

In `internal/cli/file_cmd.go`, add imports `"os"` (already present) and `"github.com/jim58246/sshmng/internal/progress"`. Replace the transfer block:

```go
	// existing: f, err := os.Open(local) ... defer f.Close()

	fi, _ := os.Stat(local)
	size := int64(-1)
	if fi != nil {
		size = fi.Size()
	}
	bar := progress.NewBar(os.Stderr, srv.Name+":"+remote, size)
	cr := &progress.CountingReader{R: f, Fn: func(n int64) { bar.SetBytes(n) }}
	n, timedOut, err := ptyConn.UploadSized(cr, size, remote, *timeoutMs)
	bar.Finish()
	if err != nil && !timedOut {
		fmt.Fprintf(out, "Error: upload %s -> %s:%s: %v\n", local, srv.Name, remote, err)
		return 1
	}
	if timedOut {
		fmt.Fprintf(out, "uploaded %s -> %s:%s (timed out, %d bytes transferred)\n", local, srv.Name, remote, n)
		return 1
	}
	fmt.Fprintf(out, "uploaded %s -> %s:%s (%d bytes)\n", local, srv.Name, remote, n)
	return 0
```

- [ ] **Step 2: Modify `runFileDownload` to use Stat + CountingWriter + Bar**

```go
	// existing: f, err := os.Create(local) ... defer f.Close()

	total := int64(-1)
	if fi, statErr := ptyConn.Stat(remote); statErr == nil {
		total = fi.Size()
	}
	bar := progress.NewBar(os.Stderr, srv.Name+":"+remote, total)
	cw := &progress.CountingWriter{W: f, Fn: func(n int64) { bar.SetBytes(n) }}
	n, timedOut, err := ptyConn.Download(remote, cw, *timeoutMs)
	bar.Finish()
	if err != nil && !timedOut {
		fmt.Fprintf(out, "Error: download %s:%s -> %s: %v\n", srv.Name, remote, local, err)
		return 1
	}
	if timedOut {
		fmt.Fprintf(out, "downloaded %s:%s -> %s (timed out, %d bytes transferred)\n", srv.Name, remote, local, n)
		return 1
	}
	fmt.Fprintf(out, "downloaded %s:%s -> %s (%d bytes)\n", srv.Name, remote, local, n)
	return 0
```

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: build succeeds.

- [ ] **Step 4: Manual smoke test (TTY) and non-TTY silence**

Run (TTY, small file): `go run . file upload <your-server> /etc/hosts /tmp/hosts_test` — observe a progress line on stderr, then the summary on stdout.
Run (non-TTY, piped): `go run . file upload <your-server> /etc/hosts /tmp/hosts_test 2>/dev/null; echo "exit=$?"` — no progress noise, summary present.
If no live server is available, at minimum verify `go vet ./internal/cli/...` is clean and the package compiles; defer live validation to integration.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/file_cmd.go
git commit -m "$(cat <<'EOF'
feat(cli): progress bars for file upload/download

Single-file upload now uses UploadSized+CountingReader (stays on sftp
pipelining path); download wraps the local writer and uses remote Stat
for the total. Progress renders to stderr; non-TTY is silent. Summary
on stdout unchanged.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: `OnProgress` callback in `DirTransferOptions` + pty worker wiring

**Files:**
- Modify: `internal/ssh/conn/sftp_dir.go:43-47` (add field)
- Modify: `internal/ssh/pty/sftp_dir.go` — `UploadDir` walk (lines 56-82) + worker pool (lines 93-137); `DownloadDir` walk (lines 286-309) + worker pool (lines 320-364).

**Interfaces:**
- Consumes: existing `DirTransferOptions`, `DirTransferResult`, worker pool structure.
- Produces: `DirTransferOptions.OnProgress func(bytes int64, files int)` — invoked after each successful file transfer (held under the existing `mu`), `nil` = no-op.

- [ ] **Step 1: Add the `OnProgress` field**

In `internal/ssh/conn/sftp_dir.go`, add to `DirTransferOptions`:
```go
type DirTransferOptions struct {
	Conflict    ConflictPolicy
	Concurrency int
	TimeoutMs   int
	// OnProgress, if non-nil, is invoked after each successfully transferred
	// file with the cumulative successful byte count and file count. Called
	// under the worker pool's mutex (serialized) so the callback need not be
	// goroutine-safe. nil = no progress reporting (MCP path).
	OnProgress func(bytes int64, files int)
}
```

- [ ] **Step 2: Wire `OnProgress` into `UploadDir`**

In `internal/ssh/pty/sftp_dir.go` `UploadDir`: accumulate totals during walk and invoke the callback in the worker. The walk callback (line 56) already has `fi os.FileInfo` — add `totalBytes`/`totalFiles` accumulation before appending the task. Then in the worker's success branch (around lines 126-132), after `result.Bytes += int64(n); result.Files++`, add the guarded callback.

Walk accumulation (inside the `filepath.Walk` func, after `tasks = append(tasks, fileTask{localPath, remotePath})`):
```go
			tasks = append(tasks, fileTask{localPath, remotePath})
			// (totals are derived from result counts at call time; no separate
			// accumulation needed — the Bar is configured by the CLI from its
			// own walk, and OnProgress reports result.Bytes/result.Files.)
			return nil
```
Wait — per the spec the Bar's total comes from the CLI's own walk. The pty layer only reports *current* cumulative (already in `result`). So the only change needed in the worker is the callback:

```go
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
				} else {
					result.Bytes += int64(n)
					result.Files++
					if opts.OnProgress != nil {
						opts.OnProgress(result.Bytes, result.Files)
					}
				}
				if timedOut {
					result.TimedOut++
				}
				mu.Unlock()
```

- [ ] **Step 3: Wire `OnProgress` into `DownloadDir` symmetrically**

In `DownloadDir`'s worker success branch (around sftp_dir.go:351-361), add the same guarded callback after `result.Bytes += int64(n); result.Files++`:
```go
					mu.Lock()
					if err != nil {
						errs = append(errs, err)
					} else {
						result.Bytes += int64(n)
						result.Files++
						if opts.OnProgress != nil {
							opts.OnProgress(result.Bytes, result.Files)
						}
					}
					if timedOut {
						result.TimedOut++
					}
					mu.Unlock()
```

- [ ] **Step 4: Run existing sftp_dir tests as regression gate**

Run: `go test ./internal/ssh/pty/ -run SftpDir`
Expected: PASS — `OnProgress` is nil for existing callers (MCP, tests), behavior unchanged. Also `go test ./internal/ssh/session/...` (fakes pass opts through without reading the new field).

- [ ] **Step 5: Commit**

```bash
git add internal/ssh/conn/sftp_dir.go internal/ssh/pty/sftp_dir.go
git commit -m "$(cat <<'EOF'
feat(sftp): OnProgress callback for directory transfers

DirTransferOptions gains an optional OnProgress func(bytes, files) invoked
after each successful file under the worker-pool mutex (serialized, so the
callback need not be goroutine-safe). nil preserves current silent behavior;
MCP path is unchanged.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: Wire progress bars into directory CLI (upload-dir / download-dir)

**Files:**
- Modify: `internal/cli/file_cmd.go` — `runFileUploadDir` (around line 255-265) and `runFileDownloadDir` (around line 307-317).

**Interfaces:**
- Consumes: `DirTransferOptions.OnProgress` (Task 6), `progress.NewBar` (Task 3), local `os.Stat`/remote `ptyConn.Stat` for totals.
- Produces: directory subcommands render a bar with both byte and file dimensions.

- [ ] **Step 1: Add a helper to compute dir totals locally (for upload-dir)**

Add near `printDirResult` in `file_cmd.go`:
```go
// localDirTotals walks localDir (non-recursively into the count via filepath.Walk)
// returning total bytes and file count of regular files (symlinks skipped, matching
// pty.UploadDir's walk). Used to size the progress bar before transfer starts.
func localDirTotals(localDir string) (bytes int64, files int) {
	filepath.Walk(localDir, func(_ string, fi os.FileInfo, err error) error {
		if err != nil || fi == nil {
			return nil
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !fi.IsDir() {
			bytes += fi.Size()
			files++
		}
		return nil
	})
	return
}
```
Add `"path/filepath"` and `"os"` to imports (os already imported; add filepath).

- [ ] **Step 2: Wire bar into `runFileUploadDir`**

After `opts := conn.DirTransferOptions{...}` (line 255-259) and before `ptyConn.UploadDir`, insert:
```go
	totalBytes, totalFiles := localDirTotals(local)
	bar := progress.NewBar(os.Stderr, srv.Name+":"+remote, totalBytes)
	if totalFiles > 0 {
		bar.SetFiles(totalFiles)
	}
	opts.OnProgress = func(bytes int64, files int) {
		bar.SetBytes(bytes)
		bar.SetFilesDone(files)
	}
	res, err := ptyConn.UploadDir(local, remote, opts)
	bar.Finish()
	printDirResult(out, "uploaded", local, srv.Name, remote, res, err)
```
(Replace the existing `res, err := ...; printDirResult(...)` lines.)

- [ ] **Step 3: Wire bar into `runFileDownloadDir`**

For download-dir the totals come from the remote. Add a helper that walks the remote via sftp to sum sizes, OR reuse the pty walk. The simplest correct approach: call a remote-walk total. But ptyConn has no public "walk totals" method. Add a small method on `*pty.PtyConn`:

In `internal/ssh/pty/sftp_dir.go`, add:
```go
// RemoteDirTotals sums the byte size and regular-file count of remoteDir via
// sftp Walk (symlinks skipped, matching DownloadDir). Used to size the progress
// bar before a download-dir transfer. Errors are tolerated (returns counts seen).
func (p *PtyConn) RemoteDirTotals(remoteDir string) (bytes int64, files int) {
	p.mu.Lock()
	sftpClient := p.sftpClient
	p.mu.Unlock()
	if sftpClient == nil {
		return 0, 0
	}
	walker := sftpClient.Walk(remoteDir)
	for walker.Step() {
		if walker.Err() != nil {
			continue
		}
		fi := walker.Stat()
		if fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if !fi.IsDir() {
			bytes += fi.Size()
			files++
		}
	}
	return
}
```

Then in `runFileDownloadDir`, after the sftp-availability check and before `opts :=`:
```go
	totalBytes, totalFiles := ptyConn.RemoteDirTotals(remote)
	bar := progress.NewBar(os.Stderr, srv.Name+":"+remote, totalBytes)
	if totalFiles > 0 {
		bar.SetFiles(totalFiles)
	}
	opts.OnProgress = func(bytes int64, files int) {
		bar.SetBytes(bytes)
		bar.SetFilesDone(files)
	}
	res, err := ptyConn.DownloadDir(remote, local, opts)
	bar.Finish()
	printDirResult(out, "downloaded", local, srv.Name, remote, res, err)
```
(Replace the existing `res, err := ...; printDirResult(...)` lines.)

- [ ] **Step 4: Build and run regression tests**

Run: `go build ./... && go test ./internal/ssh/pty/ ./internal/cli/`
Expected: build succeeds, tests pass (the new pty method is additive; CLI has no existing unit tests to break, but pty sftp_dir tests must still pass).

- [ ] **Step 5: Commit**

```bash
git add internal/cli/file_cmd.go internal/ssh/pty/sftp_dir.go
git commit -m "$(cat <<'EOF'
feat(cli): progress bars for upload-dir/download-dir

Directory transfers show a bar with byte + file dimensions. Upload-dir
totals come from a local walk; download-dir totals from a new
PtyConn.RemoteDirTotals (remote sftp walk). OnProgress feeds the bar;
non-TTY stays silent. Summary on stdout unchanged.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: `RelayTransferWithProgress` on `session.Manager`

**Files:**
- Modify: `internal/ssh/session/relay.go` — add new method after `RelayTransfer` (after line 289).

**Interfaces:**
- Consumes: existing `RelayTransfer` internals (`fanWriter`, `liveEntry`, upload/download goroutines), `progress.CountingWriter` (Task 2).
- Produces: `func (m *Manager) RelayTransferWithProgress(srcSid, srcPath string, dstSids []string, dstPath string, timeoutMs int, onDownload func(bytes int64), onDestDone func(dstSid string, ok bool, bytes int64, err error)) (*RelayResult, error)`.

- [ ] **Step 1: Write a failing test for `RelayTransferWithProgress`**

Add to `internal/ssh/session/relay_test.go` (mirror the existing relay test setup; if `TestRelayTransfer*` fixtures exist, reuse them). The test asserts `onDownload` is invoked with increasing byte counts and `onDestDone` fires once per destination:
```go
func TestRelayTransferWithProgressCallbacks(t *testing.T) {
	// Reuse the same fake-conn + manager setup pattern as the existing
	// TestRelayTransfer* test in this file (see its setup for fakeConn fields:
	// sftpEnabled=true, downloadResult data, upload success).
	// Build a 1:2 relay: src -> [dst1, dst2].
	// Assert: onDownload received at least one nonzero byte count;
	//         onDestDone called exactly len(dstSids) times with ok=true.
	// (Adapt the concrete fake setup from the nearest existing relay test —
	// copy its manager/session construction verbatim, then swap the call to
	// RelayTransferWithProgress with the two callbacks.)
}
```
Implement the body by copying the nearest existing `TestRelayTransfer*` test's setup and swapping in the new method + callback capture slices. If no such test exists in `relay_test.go` beyond the fanWriter unit tests, instead build a minimal in-memory fake using `stubConn` (already in `tools_relay_test.go` / `session_test.go`) registered into a `Manager`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ssh/session/ -run RelayTransferWithProgress`
Expected: FAIL — method undefined.

- [ ] **Step 3: Implement `RelayTransferWithProgress`**

The cleanest implementation: wrap the download-side `fanWriter` in a `CountingWriter` so `onDownload` fires, and add an `onDestDone` call at the end of each upload goroutine. Add to `relay.go`:

```go
// RelayTransferWithProgress is RelayTransfer with progress callbacks. It does
// NOT change the barrier fan-out model: live destinations advance in lockstep,
// so onDownload reflects the shared download pace. onDestDone fires once per
// destination at upload completion (low frequency). Either callback may be nil.
//
// Existing RelayTransfer is unchanged; MCP continues to use it.
func (m *Manager) RelayTransferWithProgress(
	srcSid, srcPath string, dstSids []string, dstPath string, timeoutMs int,
	onDownload func(bytes int64),
	onDestDone func(dstSid string, ok bool, bytes int64, err error),
) (*RelayResult, error) {
	srcSess, err := m.Get(srcSid)
	if err != nil {
		return nil, err
	}
	if len(dstSids) == 0 {
		return nil, errors.New("no relay destinations")
	}

	// --- pre-flight (same as RelayTransfer) ---
	if !srcSess.SftpAvailable() {
		dests := make([]RelayDest, len(dstSids))
		for i, sid := range dstSids {
			dests[i] = RelayDest{DstSid: sid, Err: errors.New("source sftp unavailable")}
			if onDestDone != nil {
				onDestDone(sid, false, 0, errors.New("source sftp unavailable"))
			}
		}
		return &RelayResult{SrcServer: srcSess.ServerName(), Destinations: dests, Err: conn.ErrSftpUnavailable}, nil
	}

	dests := make([]RelayDest, len(dstSids))
	type liveEntry struct {
		idx  int
		sid  string
		sess *Session
		pr   *io.PipeReader
		pw   *io.PipeWriter
	}
	var live []*liveEntry

	for i, sid := range dstSids {
		dests[i] = RelayDest{DstSid: sid}
		if sid == srcSid {
			dests[i].Err = errors.New("cannot relay a session to itself")
			if onDestDone != nil {
				onDestDone(sid, false, 0, errors.New("cannot relay a session to itself"))
			}
			continue
		}
		ds, gerr := m.Get(sid)
		if gerr != nil {
			dests[i].Err = gerr
			if onDestDone != nil {
				onDestDone(sid, false, 0, gerr)
			}
			continue
		}
		dests[i].DstServer = ds.ServerName()
		if !ds.SftpAvailable() {
			dests[i].Err = conn.ErrSftpUnavailable
			if onDestDone != nil {
				onDestDone(sid, false, 0, conn.ErrSftpUnavailable)
			}
			continue
		}
		if ds.State() != StateIdle {
			dests[i].Err = errors.New("session busy")
			if onDestDone != nil {
				onDestDone(sid, false, 0, errors.New("session busy"))
			}
			continue
		}
		pr, pw := io.Pipe()
		live = append(live, &liveEntry{idx: i, sid: sid, sess: ds, pr: pr, pw: pw})
	}

	if len(live) == 0 {
		return &RelayResult{SrcServer: srcSess.ServerName(), Destinations: dests, Err: errors.New("no live relay destinations (all failed pre-flight)"}, nil
	}

	var size int64 = -1
	useSized := false
	if fi, statErr := srcSess.Stat(srcPath); statErr == nil {
		if !fi.Mode().IsRegular() {
			for _, e := range live {
				e.pw.CloseWithError(errors.New("source not a regular file"))
				dests[e.idx].Err = errors.New("source not a regular file")
				if onDestDone != nil {
					onDestDone(e.sid, false, 0, errors.New("source not a regular file"))
				}
			}
			return &RelayResult{SrcServer: srcSess.ServerName(), Destinations: dests, Err: errors.New("source not a regular file")}, nil
		}
		size = fi.Size()
		useSized = true
	}

	pws := make([]*io.PipeWriter, len(live))
	for i, e := range live {
		pws[i] = e.pw
	}
	fw := newFanWriter(pws)

	// Wrap fanWriter so each downloaded chunk reports cumulative bytes.
	var dlSink io.Writer = fw
	if onDownload != nil {
		dlSink = &progress.CountingWriter{W: fw, Fn: onDownload}
	}

	var dlN int
	var dlTimedOut bool
	var dlErr error
	dlDone := make(chan struct{})
	go func() {
		defer close(dlDone)
		dlN, dlTimedOut, dlErr = srcSess.Download(srcPath, dlSink, timeoutMs)
		for _, e := range live {
			e.pw.CloseWithError(dlErr)
		}
	}()

	var upWg sync.WaitGroup
	for _, e := range live {
		upWg.Add(1)
		go func(e *liveEntry) {
			defer upWg.Done()
			var n int
			var tOut bool
			var uerr error
			if useSized {
				n, tOut, uerr = e.sess.UploadSized(e.pr, size, dstPath, timeoutMs)
			} else {
				n, tOut, uerr = e.sess.Upload(e.pr, dstPath, timeoutMs)
			}
			e.pr.CloseWithError(uerr)
			dests[e.idx].OK = uerr == nil && !tOut
			dests[e.idx].Bytes = n
			dests[e.idx].TimedOut = tOut
			dests[e.idx].Err = uerr
			if onDestDone != nil {
				onDestDone(e.sid, dests[e.idx].OK, int64(n), uerr)
			}
		}(e)
	}

	<-dlDone
	upWg.Wait()

	res := &RelayResult{
		DownloadedBytes: dlN,
		TimedOut:        dlTimedOut,
		SrcServer:       srcSess.ServerName(),
		Destinations:    dests,
	}
	for i := range dests {
		if dests[i].TimedOut {
			res.TimedOut = true
		}
	}
	allOK := dlErr == nil && !dlTimedOut
	for i := range dests {
		if !dests[i].OK {
			allOK = false
		}
	}
	if allOK && useSized {
		if dlN != int(size) {
			allOK = false
		}
		for i := range dests {
			if dests[i].Bytes != int(size) {
				dests[i].OK = false
				allOK = false
			}
		}
	}
	if !allOK {
		root := dlErr
		if root == nil {
			root = errors.New("one or more relay destinations failed")
		}
		res.Err = root
	}
	return res, nil
}
```

Add `"github.com/jim58246/sshmng/internal/progress"` to relay.go imports (or to session package — confirm import path; the relay.go file is in `package session`, so import `github.com/jim58246/sshmng/internal/progress`). Verify no import cycle: `progress` imports `termutil` only; `session` importing `progress` is a new edge — check `go build` in Step 4 catches any cycle.

- [ ] **Step 4: Build and run tests**

Run: `go build ./... && go test ./internal/ssh/session/ -run Relay`
Expected: build succeeds (no import cycle), new test passes, existing relay tests still pass.

- [ ] **Step 5: Commit**

```bash
git add internal/ssh/session/relay.go internal/ssh/session/relay_test.go
git commit -m "$(cat <<'EOF'
feat(relay): RelayTransferWithProgress with download + per-dest callbacks

New method wraps fanWriter in a CountingWriter for cumulative download
bytes and fires onDestDone once per destination at upload completion.
Barrier fan-out model unchanged (live dests advance in lockstep).
RelayTransfer and MCP path untouched.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 9: Wire progress bar into relay CLI

**Files:**
- Modify: `internal/cli/file_cmd.go` — `runFileRelay` (around line 416-446).

**Interfaces:**
- Consumes: `Manager.RelayTransferWithProgress` (Task 8), `progress.NewBar`, `progress.CountingWriter` (Tasks 2-3), source `Stat` for total (via the manager method's internal Stat — but CLI needs the total to size the bar; obtain it from a pre-flight `ptyConn.Stat` on the source, or pass `total=-1` and rely on `RelayTransferWithProgress`'s internal Stat). 

**Design decision:** The CLI does not have the source `*pty.PtyConn` directly (relay uses `loginRelaySession` which registers sessions into the manager and discards the ptyConn). The simplest correct path: get the source session via `manager.Get(srcSid)` and call its `Stat(srcPath)` to size the bar. `Session.Stat` exists (`session.go:390`).

- [ ] **Step 1: Modify `runFileRelay` to build a bar and call `RelayTransferWithProgress`**

Replace the `res, err := manager.RelayTransfer(...)` block (lines 416-421) and the per-dest printing with a version that sets up a bar first. Insert before the manager call:

```go
	// Size the bar from the source file via the source session's Stat.
	total := int64(-1)
	if srcSess, gerr := manager.Get(srcSid); gerr == nil {
		if fi, statErr := srcSess.Stat(srcPath); statErr == nil {
			total = fi.Size()
		}
	}
	bar := progress.NewBar(os.Stderr, srcServerName, total)

	// Track dest completion for the status tag: ✓ done, ✗ failed, ⏳ in-flight.
	type dstStat struct{ done, ok bool }
	dstStates := make(map[string]*dstStat, len(*to))
	for _, n := range *to {
		dstStates[n] = &dstStat{}
	}
	sidToName := make(map[string]string)
	// (sidToName filled from onDestDone's sid; but CLI only has names. Map by
	//  resolving sid->name: the manager.Get(sid).ServerName() gives it. For the
	//  status tag we instead count by outcome directly.)
	var doneOK, doneFail, inFlight int
	for range *to {
		inFlight++
	}
	updateStatus := func() {
		bar.SetStatus(fmt.Sprintf("%d✓ %d✗ %d⏳", doneOK, doneFail, inFlight))
	}
	updateStatus()

	res, err := manager.RelayTransferWithProgress(srcSid, srcPath, dstSids, dstPath, *timeoutMs,
		func(bytes int64) { bar.SetBytes(bytes) },
		func(dstSid string, ok bool, _ int64, _ error) {
			inFlight--
			if ok {
				doneOK++
			} else {
				doneFail++
			}
			updateStatus()
		},
	)
	bar.Finish()
```

Keep the existing per-dest summary printing (`for _, d := range res.Destinations { ... }`) and the final `relay ... -> N destinations` line unchanged — they write to `out` (stdout). Add `"fmt"` (already imported) — `fmt.Sprintf` is used in `updateStatus`.

Note: `dstStates` map above is unused in the final counting approach — remove it and the `sidToName` stub to keep the code clean (the three counters suffice). Final code keeps only `doneOK/doneFail/inFlight` + `updateStatus`.

- [ ] **Step 2: Build and run**

Run: `go build ./... && go vet ./internal/cli/...`
Expected: clean build, no vet warnings.

- [ ] **Step 3: Manual smoke test (if a relay config is available)**

Run: `go run . file relay <src> <src-path> <dst-path> --to <dst1> --timeout 30000` — observe a single progress line with `[N✓ N✗ N⏳]` status updating, then the per-dest summary. If no live servers, ensure `go vet` clean and defer live validation.

- [ ] **Step 4: Run full test suite**

Run: `go test ./...`
Expected: all PASS (no behavioral change to tested paths; CLI has no unit tests but nothing regresses).

- [ ] **Step 5: Commit**

```bash
git add internal/cli/file_cmd.go
git commit -m "$(cat <<'EOF'
feat(cli): progress bar for file relay

Relay shows a single main bar (downloaded bytes / source size) plus a
dest-status tag [N✓ N✗ N⏳]. Reflects the barrier model honestly: live
destinations advance in lockstep, so one bar represents all. Per-dest
summary on stdout unchanged.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

## Self-Review

**1. Spec coverage:**
- `internal/termutil` shared VT → Task 1 ✓
- `internal/progress` Bar + counting + format → Tasks 2, 3 ✓
- Single-file upload (`UploadSized`+CountingReader) / download (Stat+CountingWriter) → Task 5 ✓
- UploadSized pipelining validation (bench + fallback) → Task 4 ✓
- `DirTransferOptions.OnProgress` + walk totals + held-lock callback → Task 6 ✓ (callback placed under existing `mu`, matching spec's "持锁回调 = bar 免锁")
- Directory CLI bar with byte+file dimensions → Task 7 ✓
- `RelayTransferWithProgress` + CountingWriter on fanWriter + onDestDone → Task 8 ✓
- Relay CLI single bar + status tag → Task 9 ✓
- Progress→stderr / summary→stdout → enforced in every CLI task (Bar uses `os.Stderr`, `out` stays stdout) ✓
- Non-TTY silent → `NewBar` detection in Task 3, tested ✓
- MCP unchanged → `OnProgress` nil-default (Task 6), `RelayTransfer` untouched (Task 8) ✓
- No new deps → only `golang.org/x/term`/`x/sys` (existing) ✓

**2. Placeholder scan:** Task 8 Step 1 references "adapt the nearest existing relay test" — this is the one place requiring the implementer to locate existing fixtures. This is acceptable guidance (the file is small and the fixtures are named), not a placeholder. All other steps contain full code. Task 4 Step 3 has a conditional fallback with full code. No TBD/TODO.

**3. Type consistency:**
- `CountingReader{R, Fn}` / `CountingWriter{W, Fn}` — consistent across Tasks 2, 4, 5, 8.
- `NewBar(w, label, total)`, `SetBytes(int64)`, `SetFiles(int)`, `SetFilesDone(int)`, `SetStatus(string)`, `Finish()` — consistent across Tasks 3, 5, 7, 9.
- `OnProgress func(bytes int64, files int)` — consistent in Task 6 (definition) and Task 7 (usage).
- `RelayTransferWithProgress(... onDownload func(int64), onDestDone func(string, bool, int64, error))` — consistent in Task 8 (definition) and Task 9 (usage). Note Task 9 passes `_ int64, _ error` for onDestDone's unused params — signatures match.
- `RelayTransferWithProgress` returns `*RelayResult` (pointer) in Task 8; Task 9 uses `res, err :=` then `res.Destinations` — pointer dereference works. The existing `RelayTransfer` returns value `RelayResult`; the new method returns pointer. Task 9's `res.Destinations` works either way. Consistent.

No issues found.

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-10-file-transfer-progress.md`. Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
