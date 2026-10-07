package progress

import (
	"testing"
	"time"
)

func TestFormat(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 48 << 20: "48.0 MiB", 3 << 40: "3.0 TiB"} {
		if got := Size(n); got != want {
			t.Errorf("Size(%d) = %q, want %q", n, got, want)
		}
	}
	for d, want := range map[time.Duration]string{200 * time.Millisecond: "1 s", 40 * time.Second: "40 s", 90 * time.Second: "2 min", 3700 * time.Second: "1 h 1 min"} {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestMeter(t *testing.T) {
	var m Meter
	now := time.Unix(1_000_000, 0)
	if m.Speed(0, 100) != "" || m.Summary(0, now) != "" {
		t.Error("measured before any sample")
	}
	m.Sample(0, now)
	m.Sample(1<<20, now.Add(200*time.Millisecond)) // too soon to count
	if m.Speed(1<<20, 100<<20) != "" {
		t.Error("measured after 200 ms")
	}
	for i := 1; i <= 4; i++ {
		m.Sample(int64(i)*2<<20, now.Add(time.Duration(i)*time.Second))
	}
	if got := m.Speed(8<<20, 100<<20); got != "2.0 MiB/s, 46 s left" {
		t.Errorf("steady 2 MiB/s: %q", got)
	}
	// A stall slows it down gradually rather than at once.
	m.Sample(8<<20, now.Add(5*time.Second))
	if r := m.rate / (1 << 20); r <= 1 || r >= 2 {
		t.Errorf("after a stalled second: %.2f MiB/s", r)
	}
	if got := m.Speed(8<<20, 0); got == "" || got[len(got)-4:] == "left" {
		t.Errorf("unknown total: %q", got)
	}
	if got := m.Summary(8<<20, now.Add(5*time.Second)); got != "8.0 MiB in 5 s, 1.6 MiB/s" {
		t.Errorf("summary %q", got)
	}
}
