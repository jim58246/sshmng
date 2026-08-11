package progress

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
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
const barGaugeWidth = 20  // max width of the █░ gauge
const labelEllipsis = "…" // U+2026, single display column

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

// cellWidth returns the display column width of a rune. CJK and other East
// Asian wide chars take 2 columns; everything else (ASCII, the █░ block chars
// in U+2500–U+259F, …, ✓/✗/⏳) takes 1. Conservative: any rune in the wide
// ranges counts as 2, so the line may truncate slightly early rather than wrap.
func cellWidth(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80, // CJK radicals/symbols/unified + Yi + Hangul syllables + CJK compat + fullwidth + emoji + ext B+
		r == 0x2329, r == 0x232A:
		return 2
	default:
		return 1
	}
}

// displayWidth returns the display column width of s (sum of cellWidth per
// rune). Use this — not len(s) or utf8.RuneCountInString(s) — when checking
// whether a line fits a terminal width: CJK server names take 2 columns each,
// so byte/rune length underestimates and the line wraps (wrapping breaks
// \r-based single-line refresh → 刷屏 on Windows console).
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += cellWidth(r)
	}
	return w
}

// truncateToWidth truncates s (by rune) so its display width is <= maxW,
// appending "…" if truncation occurred. maxW must be >= 1 (the ellipsis width).
func truncateToWidth(s string, maxW int) string {
	if maxW < 1 {
		return ""
	}
	if displayWidth(s) <= maxW {
		return s
	}
	// Reserve 1 column for the ellipsis, fill with runes that fit.
	limit := maxW - 1
	out := make([]rune, 0, utf8.RuneCountInString(s))
	w := 0
	for _, r := range s {
		rw := cellWidth(r)
		if w+rw > limit {
			break
		}
		out = append(out, r)
		w += rw
	}
	return string(out) + labelEllipsis
}

// gaugeString builds the █░ gauge of width gw showing pct fill. pct is
// assumed clamped to [0, 100], so filled is always in [0, gw].
func gaugeString(gw, pct int) string {
	if gw <= 0 {
		return ""
	}
	filled := gw * pct / 100
	return strings.Repeat("█", filled) + strings.Repeat("░", gw-filled)
}

// joinNonEmpty joins non-empty parts with sep, prefixing sep only between
// actual parts (no leading/trailing sep).
func joinNonEmpty(parts []string, sep string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

// renderBarLine produces one progress line (no leading \r, no trailing newline)
// that fits within s.width display columns. When the full layout would exceed
// the width, non-essential elements are dropped (tags → ETA → rate → pct), the
// gauge shrinks, and finally the label is truncated with …. This is essential:
// a line longer than the terminal width wraps, and \r then returns to column 0
// of the wrapped (lower) line — so each redraw advances a line and floods the
// screen (刷屏), instead of overwriting the single line.
//
// Layout (known size):  <label>  <gauge>  <pct>%  <cur>/<total>  <rate>  ETA <s>  [tags]
// Layout (unknown):     <label>  <cur>  <rate>  [tags]
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
		tags = fmt.Sprintf("[%d/%d files]", s.filesDone, s.filesTotal)
	}
	if s.status != "" {
		if tags != "" {
			tags += " "
		}
		tags += fmt.Sprintf("[%s]", s.status)
	}

	// Unknown size: label + cur + rate (+ tags).
	if s.total < 0 {
		// Drop order: tags → rate. cur is always kept.
		for _, extra := range [][]string{{rateStr, tags}, {rateStr}, {}} {
			extraStr := ""
			if len(extra) > 0 {
				extraStr = "  " + joinNonEmpty(extra, "  ")
			}
			line := s.label + "  " + curStr + extraStr
			if displayWidth(line) <= width {
				return line
			}
		}
		// Still too wide: truncate label, keep cur.
		return fitLabelAndMeta(s.label, curStr, width)
	}

	// Known size.
	pct := 0
	if s.total > 0 {
		pct = int(float64(s.current) / float64(s.total) * 100)
	}
	if pct > 100 {
		pct = 100
	}
	totalStr := humanizeBytes(s.total)
	etaStr := "—"
	if rate > 0 && s.current < s.total {
		remaining := time.Duration(float64(s.total-s.current)/rate) * time.Second
		etaStr = fmt.Sprintf("%ds", int(remaining.Seconds()))
	}
	progStr := curStr + "/" + totalStr
	pctStr := fmt.Sprintf("%d%%", pct)

	// Drop order (least essential first): tags → ETA → rate → pct.
	// prog (cur/total) and label are kept longest.
	extraSets := [][]string{
		{pctStr, progStr, rateStr, "ETA " + etaStr, tags},
		{pctStr, progStr, rateStr, "ETA " + etaStr},
		{pctStr, progStr, rateStr},
		{pctStr, progStr},
		{progStr},
	}

	// For each extras set, try gauge widths from barGaugeWidth down to 0 with
	// the FULL (untruncated) label. First combination that fits wins — this
	// keeps the label intact and the gauge as large as possible.
	for _, extras := range extraSets {
		meta := joinNonEmpty(extras, "  ")
		metaW := displayWidth(meta)
		labelW := displayWidth(s.label)
		for gw := barGaugeWidth; gw >= 0; gw-- {
			gauge := gaugeString(gw, pct)
			// Layout: "label  gauge  meta" with 2-space separators only between
			// non-empty parts. Compute total display width to test fit.
			var parts []string
			parts = append(parts, s.label)
			if gauge != "" {
				parts = append(parts, gauge)
			}
			nonEmpty := len(parts)
			if meta != "" {
				nonEmpty++
			}
			sepW := 0
			if nonEmpty > 1 {
				sepW = (nonEmpty - 1) * 2
			}
			total := labelW + displayWidth(gauge) + metaW + sepW
			if total <= width {
				all := parts
				if meta != "" {
					all = append(all, meta)
				}
				return joinNonEmpty(all, "  ")
			}
		}
	}

	// Nothing fits with the full label even at gauge=0 + sparsest extras.
	// Truncate the label; use the sparsest extras (prog only) and no gauge.
	sparsest := joinNonEmpty([]string{progStr}, "  ")
	return fitLabelAndMeta(s.label, sparsest, width)
}

// fitLabelAndMeta builds "label  meta" with the label truncated so the whole
// line fits within width. If even meta alone exceeds width, meta is kept and
// the label is dropped to an empty prefix (rare; extremely narrow terminals).
func fitLabelAndMeta(label, meta string, width int) string {
	metaW := displayWidth(meta)
	// "label  meta" needs labelW + 2 + metaW <= width → labelW <= width-2-metaW
	avail := width - 2 - metaW
	if avail < 1 {
		// Not enough room for a label + separator + meta. Just return meta
		// (truncated to width as a last resort).
		if metaW <= width {
			return meta
		}
		return truncateToWidth(meta, width)
	}
	return truncateToWidth(label, avail) + "  " + meta
}
