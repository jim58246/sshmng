package progress

import (
	"strings"
	"testing"
	"time"
)

func TestHumanizeBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{980, "980 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{12_300_000, "11.7 MB"},   // 11730.19 KB → 11.7 MB
		{25 * 1 << 20, "25.0 MB"}, // 25 MiB
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

func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"abc", 3},
		{"", 0},
		{"火山云", 6},        // 3 CJK chars × 2 cols
		{"a中b", 4},         // 1 + 2 + 1
		{"░", 1},           // block shade U+2591 — single width
		{"█", 1},           // block U+2588 — single width
		{"✓", 1},           // check mark — single width (common terminals)
		{"2✓ 1✗ 1⏳", 8},    // 8 single-width chars (digits, spaces, symbols)
	}
	for _, c := range cases {
		if got := displayWidth(c.in); got != c.want {
			t.Errorf("displayWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestRenderBarLineFitsNarrowWidth is the regression test for the Windows
// 刷屏 bug: when the rendered line exceeds the terminal width it wraps, and
// \r-based single-line refresh breaks (each redraw lands on a new line). The
// line MUST fit within the given width, accounting for CJK double-width chars.
func TestRenderBarLineFitsNarrowWidth(t *testing.T) {
	cases := []struct {
		name   string
		width  int
		label  string
		total  int64
		cur    int64
		status string
	}{
		{"narrow-40-cjk-label", 40, "火山云/115.190.174.107:/tmp/sshmng_narrow_remote2.bin", 5 << 20, 2_500_000, ""},
		{"narrow-40-with-status", 40, "web-01", 5 << 20, 2_500_000, "2✓ 1✗ 1⏳"},
		{"narrow-30-very-long-label", 30, "this-is-a-very-long-server-name.example.com:/some/deep/path/to/file.bin", 1 << 30, 500_000_000, ""},
		{"default-80-cjk-label", 80, "火山云/115.190.174.107:/tmp/sshmng_narrow_remote2.bin", 5 << 20, 2_500_000, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := barState{label: c.label, total: c.total, current: c.cur,
				width: c.width, status: c.status, elapsed: 3 * time.Second}
			line := renderBarLine(s)
			dw := displayWidth(line)
			if dw > c.width {
				t.Errorf("displayWidth = %d, exceeds width %d; line=%q\n(wraps → \\r-refresh breaks → 刷屏)", dw, c.width, line)
			}
		})
	}
}

// TestRenderBarLineDropsElementsGracefully verifies that as width shrinks,
// non-essential elements are dropped (ETA, then rate) before truncating the
// label, and the label is always present (truncated with … if needed).
func TestRenderBarLineDropsElementsGracefully(t *testing.T) {
	label := "web-01"
	// At width 80 the full layout includes ETA.
	wide := renderBarLine(barState{label: label, total: 100, current: 50, width: 80, elapsed: 5 * time.Second})
	if !strings.Contains(wide, "ETA") {
		t.Errorf("wide layout should include ETA: %q", wide)
	}
	// At width 20, ETA and rate should be dropped to fit.
	narrow := renderBarLine(barState{label: label, total: 100, current: 50, width: 20, elapsed: 5 * time.Second})
	if strings.Contains(narrow, "ETA") {
		t.Errorf("narrow layout should drop ETA: %q", narrow)
	}
	if !strings.Contains(narrow, label) {
		t.Errorf("label must always be present (truncated ok): %q", narrow)
	}
	if dw := displayWidth(narrow); dw > 20 {
		t.Errorf("narrow displayWidth %d > 20: %q", dw, narrow)
	}
}
