package web

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
)

// plural is the template helper's Go-side twin.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// clampPct bounds a percentage to 0..100 for the inline load bar.
func clampPct(v float64) int {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return int(v + 0.5)
	}
}

// dmiValue blanks placeholders and unresolved JEDEC ids so the template's "–" shows.
func dmiValue(v string) string {
	t := strings.TrimSpace(v)
	if dmiPlaceholders[strings.ToLower(t)] || strings.HasPrefix(strings.ToLower(t), "unknown - [") {
		return ""
	}
	return t
}

// shortCPUModel trims SMBIOS boilerplate: "AMD Ryzen 7 6800H with Radeon Graphics" becomes "Ryzen 7 6800H".
func shortCPUModel(name string) string {
	out := strings.Join(strings.Fields(cpuModelNoise.Replace(name)), " ")
	for _, cut := range cpuModelCut {
		if i := strings.Index(out, cut); i > 0 {
			out = out[:i]
		}
	}
	for _, v := range cpuModelVendors {
		if rest, ok := strings.CutPrefix(out, v); ok {
			out = rest
			break
		}
	}
	return strings.TrimSpace(out)
}

// humanDuration renders an uptime as "2d 4h3m" / "5h 12m3s" / "3m 7s".
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh%dm", days, hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh %dm%ds", hours, mins, secs)
	case mins > 0 && secs == 0:
		return fmt.Sprintf("%dm", mins)
	default:
		return fmt.Sprintf("%dm %ds", mins, secs)
	}
}

// freqMHz renders hertz as megahertz; the snapshot and metrics carry hertz.
func freqMHz(hz float64) string {
	mhz := hz / 1e6
	if mhz == math.Trunc(mhz) {
		return strconv.FormatInt(int64(mhz), 10)
	}
	return strconv.FormatFloat(mhz, 'f', 1, 64)
}

// pctOf is a whole-percent share, clamped to 0..100.
func pctOf(part, whole uint64) int {
	if whole == 0 {
		return 0
	}
	p := int(float64(part) / float64(whole) * 100)
	switch {
	case p < 0:
		return 0
	case p > 100:
		return 100
	default:
		return p
	}
}

// pctOfF takes float inputs in float: a negative float converted to unsigned wraps on amd64.
func pctOfF(part, whole float64) int {
	if !(part > 0) || !(whole > 0) {
		return 0
	}
	p := int(part / whole * 100)
	if p > 100 {
		return 100
	}
	return p
}

// displaySize renders a MiB count as GiB when it divides evenly.
func displaySize(mib int) string {
	if mib%1024 == 0 && mib != 0 {
		return fmt.Sprintf("%d GiB", mib/1024)
	}
	return fmt.Sprintf("%d MiB", mib)
}

// trimFloat renders a float without a trailing ".0".
func trimFloat(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// formatRate renders a byte-per-second throughput.
func formatRate(bps float64) string {
	if bps < 1 {
		return "0 B/s"
	}
	return humanize.Bytes(uint64(bps)) + "/s"
}

// formatLinkSpeed renders megabits as Gbit/s once it reaches 1000.
func formatLinkSpeed(mbit float64) string {
	if mbit >= 1000 {
		return trimFloat(mbit/1000) + " Gbit/s"
	}
	return trimFloat(mbit) + " Mbit/s"
}

// shortHash keeps the first 12 characters, the length git uses for the same job.
func shortHash(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// shortDuration renders seconds in the largest readable unit; offsets want µs or ms.
func shortDuration(seconds float64) string {
	abs := math.Abs(seconds)
	switch {
	case abs == 0:
		return "0"
	case abs < 1e-6:
		return fmt.Sprintf("%.0f ns", seconds*1e9)
	case abs < 1e-3:
		return fmt.Sprintf("%.1f µs", seconds*1e6)
	case abs < 1:
		return fmt.Sprintf("%.1f ms", seconds*1e3)
	default:
		return fmt.Sprintf("%.2f s", seconds)
	}
}
