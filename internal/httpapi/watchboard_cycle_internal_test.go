package httpapi

// The tile's cycle-reporting helpers (watchboard_cycle.go), tested directly
// since modeRow and dashboardData are unexported.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

func TestAgeLabel(t *testing.T) {
	t.Parallel()

	t.Run("scanned", func(t *testing.T) {
		t.Parallel()

		row := modeRow{Series: SeriesView{LastScan: &store.Summary{
			StartedAt: time.Now().Add(-12 * time.Minute),
		}}}

		if got := row.AgeLabel(); got != "12m old" {
			t.Errorf("AgeLabel() = %q, want %q", got, "12m old")
		}
	})

	t.Run("never scanned", func(t *testing.T) {
		t.Parallel()

		row := modeRow{}

		if got := row.AgeLabel(); got != "never scanned" {
			t.Errorf("AgeLabel() = %q, want %q", got, "never scanned")
		}
	})

	t.Run("running", func(t *testing.T) {
		t.Parallel()

		row := modeRow{Series: SeriesView{
			LastScan: &store.Summary{StartedAt: time.Now().Add(-time.Hour)},
			Running:  []scanner.Running{{}},
		}}

		if got := row.AgeLabel(); got != "scanning now" {
			t.Errorf("AgeLabel() = %q, want %q", got, "scanning now")
		}
	})
}

func TestNextLabel(t *testing.T) {
	t.Parallel()

	t.Run("scheduled", func(t *testing.T) {
		t.Parallel()

		row := modeRow{NextRun: time.Now().Add(18 * time.Minute)}

		if got := row.NextLabel(); got != "next in 18m" {
			t.Errorf("NextLabel() = %q, want %q", got, "next in 18m")
		}
	})

	t.Run("not scheduled", func(t *testing.T) {
		t.Parallel()

		row := modeRow{}

		if got := row.NextLabel(); got != "not scheduled" {
			t.Errorf("NextLabel() = %q, want %q", got, "not scheduled")
		}
	})

	t.Run("past due", func(t *testing.T) {
		t.Parallel()

		row := modeRow{NextRun: time.Now().Add(-time.Minute)}

		if got := row.NextLabel(); got != "next scan overdue" {
			t.Errorf("NextLabel() = %q, want %q", got, "next scan overdue")
		}

		if !row.Overdue() {
			t.Error("Overdue() = false for a past-due NextRun")
		}
	})
}

func TestCyclePercent(t *testing.T) {
	t.Parallel()

	// Each case reads the clock itself, just before it builds its row.
	// CyclePercent measures against time.Now, and parallel subtests only
	// start once this function has returned, so a now taken here could be
	// older than 1% of the hour-long interval by the time a case runs on a
	// loaded runner — and "start of interval" then reads 1, not 0.

	t.Run("start of interval", func(t *testing.T) {
		t.Parallel()

		now := time.Now()

		row := modeRow{
			LastRun: now,
			NextRun: now.Add(time.Hour),
			Series:  SeriesView{LastScan: &store.Summary{StartedAt: now}},
		}

		if got := row.CyclePercent(); got != 0 {
			t.Errorf("CyclePercent() = %d, want 0", got)
		}
	})

	t.Run("mid-interval", func(t *testing.T) {
		t.Parallel()

		now := time.Now()

		row := modeRow{
			LastRun: now.Add(-30 * time.Minute),
			NextRun: now.Add(30 * time.Minute),
			Series:  SeriesView{LastScan: &store.Summary{StartedAt: now.Add(-30 * time.Minute)}},
		}

		got := row.CyclePercent()
		if got < 45 || got > 55 {
			t.Errorf("CyclePercent() = %d, want roughly 50", got)
		}
	})

	t.Run("running", func(t *testing.T) {
		t.Parallel()

		now := time.Now()

		row := modeRow{
			LastRun: now.Add(-30 * time.Minute),
			NextRun: now.Add(30 * time.Minute),
			Series:  SeriesView{Running: []scanner.Running{{}}},
		}

		if got := row.CyclePercent(); got != 100 {
			t.Errorf("CyclePercent() = %d, want 100 (running)", got)
		}
	})

	t.Run("overdue", func(t *testing.T) {
		t.Parallel()

		now := time.Now()

		row := modeRow{
			LastRun: now.Add(-2 * time.Hour),
			NextRun: now.Add(-time.Hour),
		}

		if got := row.CyclePercent(); got != 100 {
			t.Errorf("CyclePercent() = %d, want 100 (overdue)", got)
		}
	})

	t.Run("total is age plus time-to-next, not NextRun minus LastRun", func(t *testing.T) {
		t.Parallel()

		now := time.Now()

		// LastRun is 3h in the past (a stale schedule entry), but the shown
		// scan is only 10m old and next in 10m — the bar must read from
		// those two numbers, not from the disagreeing LastRun.
		row := modeRow{
			LastRun: now.Add(-3 * time.Hour),
			NextRun: now.Add(10 * time.Minute),
			Series:  SeriesView{LastScan: &store.Summary{StartedAt: now.Add(-10 * time.Minute)}},
		}

		got := row.CyclePercent()
		if got < 45 || got > 55 {
			t.Errorf("CyclePercent() = %d, want roughly 50 (age and next-in both ~10m)", got)
		}
	})

	t.Run("zero interval does not panic", func(t *testing.T) {
		t.Parallel()

		now := time.Now()

		// NextRun == LastRun, both still in the future: not overdue, but the
		// interval they imply is zero, which must short-circuit to 0 rather
		// than divide by it.
		future := now.Add(time.Hour)
		row := modeRow{
			LastRun: future,
			NextRun: future,
			Series:  SeriesView{LastScan: &store.Summary{StartedAt: future}},
		}

		if got := row.CyclePercent(); got != 0 {
			t.Errorf("CyclePercent() = %d, want 0 for a zero-length interval", got)
		}
	})
}

func TestAttachScheduleRunsBeforeShapeWatchboard(t *testing.T) {
	t.Parallel()

	next := time.Now().Add(18 * time.Minute)
	last := time.Now().Add(-12 * time.Minute)

	data := dashboardData{
		Targets: []targetRow{
			newTargetRow(TargetView{
				Name: "site",
				Series: []SeriesView{
					{Mode: model.ConsentAccept},
					{Mode: model.ConsentReject},
				},
			}),
		},
		Jobs: []scheduleRow{
			{Target: "site", Mode: model.ConsentAccept, NextRun: next, LastRun: last},
		},
	}

	data.attachSchedule()
	data.shapeWatchboard(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	var found bool
	for _, g := range data.Groups {
		for _, tg := range g.Targets {
			for _, row := range tg.Rows {
				if row.Series.Mode == model.ConsentAccept {
					found = true
					if row.NextRun != next || row.LastRun != last {
						t.Errorf("group's copy of the accept row has NextRun=%v LastRun=%v, want %v/%v", row.NextRun, row.LastRun, next, last)
					}
				}
				if row.Series.Mode == model.ConsentReject && !row.NextRun.IsZero() {
					t.Errorf("reject row got a schedule entry it was not given: %v", row.NextRun)
				}
			}
		}
	}

	if !found {
		t.Fatal("accept row not found in the shaped groups")
	}
}

func TestModeColumns(t *testing.T) {
	t.Parallel()

	t.Run("folded row contributes both modes", func(t *testing.T) {
		t.Parallel()

		g := envGroup{Targets: []watchTarget{{targetRow: targetRow{Rows: []modeRow{
			{Label: "none / reject", Folded: true},
			{Label: "accept"},
		}}}}}

		got := g.ModeColumns()
		want := []string{"none", "reject", "accept"}

		if len(got) != len(want) {
			t.Fatalf("ModeColumns() = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("ModeColumns()[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("one mode", func(t *testing.T) {
		t.Parallel()

		g := envGroup{Targets: []watchTarget{{targetRow: targetRow{Rows: []modeRow{
			{Label: "accept"},
		}}}}}

		got := g.ModeColumns()
		if len(got) != 1 || got[0] != "accept" {
			t.Errorf("ModeColumns() = %v, want [accept]", got)
		}
	})
}

// The cycle bar's width must never be an inline "style" attribute: the
// page's Content-Security-Policy sets style-src with no unsafe-inline
// (server.go), which drops inline style attributes silently rather than
// failing loudly — every bar would render at its CSS default width
// (previously 100%, indistinguishable from a series that is actually due)
// regardless of its real percentage. cycle.js sets the width from a
// data-percent attribute instead, through the CSSOM, which style-src does
// not govern.
func TestCycleBarHasNoInlineStyleAttribute(t *testing.T) {
	t.Parallel()

	ui, err := newUIRenderer()
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	row := modeRow{
		Label: "accept",
		Series: SeriesView{
			Mode:     model.ConsentAccept,
			LastScan: &store.Summary{StartedAt: now.Add(-30 * time.Minute)},
		},
		NextRun: now.Add(30 * time.Minute),
	}

	if !row.Scheduled() {
		t.Fatal("test row must be scheduled, or the bar never renders at all")
	}

	target := targetRow{
		TargetView: TargetView{Name: "site", URL: "https://example.com"},
		Rows:       []modeRow{row},
		Severity:   diff.SeverityInfo,
	}

	p := page{
		Data: dashboardData{
			Targets: []targetRow{target},
			Groups:  []envGroup{{Label: "env", Targets: []watchTarget{{targetRow: target}}}},
		},
	}

	var buf bytes.Buffer
	if err := ui.tmpl.ExecuteTemplate(&buf, "dashboard.html", p); err != nil {
		t.Fatal(err)
	}

	html := buf.String()

	if strings.Contains(html, `style="width`) {
		t.Errorf("dashboard.html has an inline width style, which the page's CSP silently drops\n%s", html)
	}

	want := `data-percent="` + strconv.Itoa(row.CyclePercent()) + `"`
	if !strings.Contains(html, want) {
		t.Errorf("dashboard.html does not carry %s\n%s", want, html)
	}
}
