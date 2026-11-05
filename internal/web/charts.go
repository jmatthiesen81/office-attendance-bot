package web

import (
	"fmt"
	"html"
	"html/template"
	"math"
	"strings"
)

// bar is one bar of a chart. Title is the tooltip.
type bar struct {
	Label string
	Value float64
	Title string
}

// chartOpts configures barChart.
type chartOpts struct {
	Width, Height int
	// Ref draws a dashed reference line at this value (0 = none).
	Ref      float64
	RefLabel string
	// MaxLabels limits the x-axis labels; others are skipped evenly.
	MaxLabels int
	// Percent formats the y-axis from 0 to 1 as percentages.
	Percent bool
	Title   string // accessible name
}

// barChart renders a single-series bar chart as inline SVG. It needs no
// JavaScript (strict CSP): every column has a full-height hit area with a
// native tooltip, and the page shows the exact numbers in a table as well.
func barChart(bars []bar, o chartOpts) template.HTML {
	if o.Width == 0 {
		o.Width = 720
	}
	if o.Height == 0 {
		o.Height = 220
	}
	if o.MaxLabels == 0 {
		o.MaxLabels = 12
	}
	const left, right, top, bottom = 36.0, 8.0, 14.0, 24.0
	plotW, plotH := float64(o.Width)-left-right, float64(o.Height)-top-bottom

	maxV := o.Ref
	for _, b := range bars {
		maxV = math.Max(maxV, b.Value)
	}
	ticks := niceTicks(maxV, o.Percent)
	top0 := ticks[len(ticks)-1]
	y := func(v float64) float64 { return top + plotH - v/top0*plotH }

	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="%s">`, o.Width, o.Height, html.EscapeString(o.Title))
	for _, t := range ticks {
		label := trimFloat(t)
		if o.Percent {
			label = fmt.Sprintf("%.0f %%", t*100)
		}
		fmt.Fprintf(&sb, `<line class="grid" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, left, left+plotW, y(t), y(t))
		fmt.Fprintf(&sb, `<text class="axis" x="%.1f" y="%.1f" text-anchor="end">%s</text>`, left-6, y(t)+4, label)
	}
	n := len(bars)
	if n > 0 {
		step := plotW / float64(n)
		gap := math.Min(2, step/3)
		w := step - gap
		every := int(math.Ceil(float64(n) / float64(o.MaxLabels)))
		for i, b := range bars {
			x := left + float64(i)*step + gap/2
			h := plotH - (y(b.Value) - top)
			title := html.EscapeString(b.Title)
			fmt.Fprintf(&sb, `<g><title>%s</title>`, title)
			fmt.Fprintf(&sb, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="transparent"/>`, left+float64(i)*step, top, step, plotH)
			if h > 0 {
				r := math.Min(2, w/2)
				fmt.Fprintf(&sb, `<path class="bar" d="%s"/>`, roundedTop(x, y(b.Value), w, h, r))
			}
			sb.WriteString(`</g>`)
			if i%every == 0 {
				fmt.Fprintf(&sb, `<text class="axis" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, x+w/2, top+plotH+16, html.EscapeString(b.Label))
			}
		}
	}
	if o.Ref > 0 {
		fmt.Fprintf(&sb, `<line class="ref" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, left, left+plotW, y(o.Ref), y(o.Ref))
		fmt.Fprintf(&sb, `<text class="ref-label" x="%.1f" y="%.1f" text-anchor="end">%s</text>`, left+plotW, y(o.Ref)-4, html.EscapeString(o.RefLabel))
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String()) //nolint:gosec // built from escaped labels and numbers only
}

// roundedTop is a bar path anchored to the baseline with rounded top corners.
func roundedTop(x, y, w, h, r float64) string {
	r = math.Min(r, h)
	return fmt.Sprintf("M%.1f %.1fV%.1fQ%.1f %.1f %.1f %.1fH%.1fQ%.1f %.1f %.1f %.1fV%.1fZ",
		x, y+h, y+r, x, y, x+r, y, x+w-r, x+w, y, x+w, y+r, y+h)
}

// niceTicks returns 0 and up to four evenly spaced round values covering max.
func niceTicks(max float64, percent bool) []float64 {
	if percent {
		return []float64{0, 0.25, 0.5, 0.75, 1}
	}
	if max <= 0 {
		max = 1
	}
	raw := max / 4
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	step := mag
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*mag >= raw {
			step = m * mag
			break
		}
	}
	if step < 1 {
		step = 1 // people are whole numbers
	}
	var out []float64
	for v := 0.0; ; v += step {
		out = append(out, v)
		if v >= max {
			break
		}
	}
	return out
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	return strings.TrimSuffix(strings.TrimSuffix(s, "0"), ".")
}
