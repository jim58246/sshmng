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
	total      int64 // <0 = unknown size
	current    int64
	filesTotal int // 0 = no files dimension
	filesDone  int
	status     string        // relay dest-status tag, e.g. "2✓ 1✗ 1⏳"; "" = none
	width      int           // terminal columns; <=0 => default 80
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
