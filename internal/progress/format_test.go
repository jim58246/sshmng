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
