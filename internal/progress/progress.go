// Package progress formats transfer sizes and measures transfer speed, for
// the progress the CLI and the GUI show.
package progress

import (
	"fmt"
	"math"
	"time"
)

// Size is n bytes in binary units: "512 B", "1.5 MiB".
func Size(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func Rate(bytesPerSec float64) string { return Size(int64(bytesPerSec)) + "/s" }

// Duration is a rough duration: "40 s", "3 min", "1 h 5 min".
func Duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", max(int(d.Seconds()), 1))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()+0.5))
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

// Meter measures a transfer's speed from samples of the bytes done,
// smoothed over a few seconds so the figure doesn't jump with every block.
// The first sample is where it starts, it ignores the time before it.
type Meter struct {
	start, lastT time.Time
	startN       int64
	lastN        int64
	rate         float64 // bytes per second, 0 until measured
}

func (m *Meter) Sample(n int64, now time.Time) {
	if m.start.IsZero() {
		m.start, m.startN = now, n
		m.lastT, m.lastN = now, n
		return
	}
	dt := now.Sub(m.lastT).Seconds()
	if dt < 0.5 {
		return
	}
	rate := float64(n-m.lastN) / dt
	if m.rate == 0 {
		m.rate = rate
	} else {
		m.rate += (1 - math.Exp(-dt/3)) * (rate - m.rate)
	}
	m.lastT, m.lastN = now, n
}

// Speed is the smoothed rate and the time left at it to reach total
// (unknown when 0): "2.0 MiB/s, 46 s left", "" until measured.
func (m *Meter) Speed(n, total int64) string {
	if m.rate <= 0 {
		return ""
	}
	s := Rate(m.rate)
	if left := total - n; total > 0 && left > 0 {
		s += ", " + Duration(time.Duration(float64(left)/m.rate*float64(time.Second))) + " left"
	}
	return s
}

// Summary is what was done since the first sample: "48.0 MiB in 12 s,
// 4.0 MiB/s", "" before any sample.
func (m *Meter) Summary(n int64, now time.Time) string {
	if m.start.IsZero() {
		return ""
	}
	d := now.Sub(m.start)
	n -= m.startN
	return fmt.Sprintf("%s in %s, %s", Size(n), Duration(d), Rate(float64(n)/max(d.Seconds(), 0.001)))
}
