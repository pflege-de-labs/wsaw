package report

import "fmt"

// FormatBytes is the human units every size wsaw shows uses — the web
// interface's template func, the storage dashboard's inline SVG (Story 5.32,
// AC9), the PDF report (Story 5.39, AC13) and the CLI's own reports all call
// it, so no two can disagree. The units are binary and say so, as the
// configuration reference's are.
func FormatBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0

	// Capped at TiB: anything larger is still shown, as thousands of TiB.
	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
