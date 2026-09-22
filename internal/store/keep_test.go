package store

import (
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// berlin is the zone the period tests cut days in. It is a real zone with a
// real DST transition, because a policy that only works in UTC would fail on
// exactly the two nights a year an operator is least able to reason about it.
func berlin(t *testing.T) *time.Location {
	t.Helper()

	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("Europe/Berlin is not available on this machine: %v", err)
	}

	return loc
}

// cand builds a candidate at a wall-clock time in a zone.
func cand(id string, at time.Time, term model.TerminationReason) candidate {
	return candidate{ScanID: id, StartedAt: at, Termination: term}
}

func at(loc *time.Location, y int, mo time.Month, d, h int) time.Time {
	return time.Date(y, mo, d, h, 0, 0, 0, loc)
}

// keptIDs lists what a selection kept, newest first.
func keptIDs(decisions []Decision) []string {
	var out []string

	for _, d := range decisions {
		if d.Keep {
			out = append(out, d.ScanID)
		}
	}

	return out
}

func ruleFor(decisions []Decision, id string) string {
	for _, d := range decisions {
		if d.ScanID == id {
			return d.Rule
		}
	}

	return ""
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}

	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}

	return true
}

func TestSelectKeepBuckets(t *testing.T) {
	t.Parallel()

	loc := berlin(t)
	now := at(loc, 2026, time.March, 20, 12)

	tests := []struct {
		name  string
		cands []candidate
		keep  Keep
		want  []string
	}{
		{
			name: "daily keeps one result per day, newest days first",
			cands: []candidate{
				cand("d3-late", at(loc, 2026, time.March, 20, 9), model.TermIdle),
				cand("d3-early", at(loc, 2026, time.March, 20, 6), model.TermIdle),
				cand("d2", at(loc, 2026, time.March, 19, 6), model.TermIdle),
				cand("d1", at(loc, 2026, time.March, 18, 6), model.TermIdle),
			},
			keep: Keep{Daily: 2, Location: loc},
			want: []string{"d3-late", "d2"},
		},
		{
			name: "a day whose newest scan is truncated keeps the day's clean scan",
			cands: []candidate{
				cand("timeout", at(loc, 2026, time.March, 20, 23), model.TermTimeout),
				cand("clean", at(loc, 2026, time.March, 20, 6), model.TermIdle),
			},
			keep: Keep{Daily: 1, Location: loc},
			want: []string{"clean"},
		},
		{
			name: "a day of only truncated scans keeps the newest of them",
			cands: []candidate{
				cand("cap", at(loc, 2026, time.March, 20, 23), model.TermRequestCap),
				cand("older", at(loc, 2026, time.March, 20, 6), model.TermByteCap),
			},
			keep: Keep{Daily: 1, Location: loc},
			want: []string{"cap"},
		},
		{
			name: "a day of only failures keeps the newest failure",
			cands: []candidate{
				cand("err", at(loc, 2026, time.March, 20, 23), model.TermError),
				cand("skipped", at(loc, 2026, time.March, 20, 6), model.TermSkipped),
			},
			keep: Keep{Daily: 1, Location: loc},
			want: []string{"err"},
		},
		{
			name: "a truncated scan beats a failed one in the same day",
			cands: []candidate{
				cand("err", at(loc, 2026, time.March, 20, 23), model.TermError),
				cand("timeout", at(loc, 2026, time.March, 20, 6), model.TermTimeout),
			},
			keep: Keep{Daily: 1, Location: loc},
			want: []string{"timeout"},
		},
		{
			name: "hourly cuts on the hour in the configured zone",
			cands: []candidate{
				cand("h12", at(loc, 2026, time.March, 20, 12), model.TermIdle),
				cand("h11", at(loc, 2026, time.March, 20, 11), model.TermIdle),
				cand("h10", at(loc, 2026, time.March, 20, 10), model.TermIdle),
			},
			keep: Keep{Hourly: 2, Location: loc},
			want: []string{"h12", "h11"},
		},
		{
			name: "monthly and yearly count only periods that hold a scan",
			cands: []candidate{
				cand("mar", at(loc, 2026, time.March, 20, 6), model.TermIdle),
				cand("jan", at(loc, 2026, time.January, 4, 6), model.TermIdle),
				cand("dec", at(loc, 2025, time.December, 4, 6), model.TermIdle),
			},
			keep: Keep{Monthly: 2, Location: loc},
			want: []string{"mar", "jan"},
		},
		{
			name: "rules combine by union and never cancel one another",
			cands: []candidate{
				cand("now", at(loc, 2026, time.March, 20, 11), model.TermIdle),
				cand("yesterday", at(loc, 2026, time.March, 19, 11), model.TermIdle),
				cand("last-year", at(loc, 2025, time.March, 19, 11), model.TermIdle),
			},
			keep: Keep{Last: 1, Yearly: 2, Location: loc},
			want: []string{"now", "last-year"},
		},
		{
			name: "last keeps a truncated scan it covers",
			cands: []candidate{
				cand("timeout", at(loc, 2026, time.March, 20, 11), model.TermTimeout),
				cand("clean", at(loc, 2026, time.March, 20, 6), model.TermIdle),
			},
			keep: Keep{Last: 1, Location: loc},
			want: []string{"timeout"},
		},
		{
			name: "within keeps everything in the window, failures included",
			cands: []candidate{
				cand("err", at(loc, 2026, time.March, 20, 11), model.TermError),
				cand("clean", at(loc, 2026, time.March, 20, 10), model.TermIdle),
				cand("old", at(loc, 2026, time.March, 18, 10), model.TermIdle),
			},
			keep: Keep{Within: 6 * time.Hour, Location: loc},
			want: []string{"err", "clean"},
		},
		{
			name: "a policy of counts alone never empties a series",
			cands: []candidate{
				cand("ancient", at(loc, 2019, time.March, 20, 11), model.TermIdle),
			},
			keep: Keep{Hourly: 2, Location: loc},
			want: []string{"ancient"},
		},
		{
			name: "an explicit age limit may empty a series",
			cands: []candidate{
				cand("ancient", at(loc, 2019, time.March, 20, 11), model.TermIdle),
			},
			keep: Keep{Within: 24 * time.Hour, Location: loc},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			policy := tc.keep

			got := keptIDs(selectKeep(tc.cands, now, Retention{Keep: &policy}))
			if !equalIDs(got, tc.want) {
				t.Errorf("kept %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSelectKeepPeriodsAtBoundaries covers the two places a period is easy to
// cut wrong: the night a zone changes offset, and the turn of a year, where
// an ISO week belongs to the year that holds most of it.
func TestSelectKeepPeriodsAtBoundaries(t *testing.T) {
	t.Parallel()

	loc := berlin(t)

	t.Run("the spring-forward night is one day, not two", func(t *testing.T) {
		t.Parallel()

		// 2026-03-29 is the night Europe/Berlin loses an hour at 02:00.
		cands := []candidate{
			cand("after", at(loc, 2026, time.March, 29, 12), model.TermIdle),
			cand("before", at(loc, 2026, time.March, 29, 1), model.TermIdle),
		}

		keep := Keep{Daily: 1, Location: loc}

		got := keptIDs(selectKeep(cands, at(loc, 2026, time.March, 30, 0), Retention{Keep: &keep}))
		if !equalIDs(got, []string{"after"}) {
			t.Errorf("kept %v, want the day's newest scan only", got)
		}
	})

	t.Run("an ISO week spanning new year is one week", func(t *testing.T) {
		t.Parallel()

		// 2025-12-29 (Monday) through 2026-01-04 are ISO week 2026-W01.
		cands := []candidate{
			cand("january", at(loc, 2026, time.January, 2, 9), model.TermIdle),
			cand("december", at(loc, 2025, time.December, 30, 9), model.TermIdle),
		}

		keep := Keep{Weekly: 1, Location: loc}

		got := keptIDs(selectKeep(cands, at(loc, 2026, time.January, 5, 0), Retention{Keep: &keep}))
		if !equalIDs(got, []string{"january"}) {
			t.Errorf("kept %v, want one result for the single ISO week", got)
		}
	})

	t.Run("the zone decides which day a scan falls in", func(t *testing.T) {
		t.Parallel()

		// 23:30 UTC is already the next day in Berlin.
		cands := []candidate{
			cand("late", time.Date(2026, time.March, 19, 23, 30, 0, 0, time.UTC), model.TermIdle),
			cand("early", time.Date(2026, time.March, 19, 8, 0, 0, 0, time.UTC), model.TermIdle),
		}

		utc := Keep{Daily: 2, Location: time.UTC}
		berlinKeep := Keep{Daily: 2, Location: loc}
		now := time.Date(2026, time.March, 20, 12, 0, 0, 0, time.UTC)

		if got := keptIDs(selectKeep(cands, now, Retention{Keep: &utc})); !equalIDs(got, []string{"late"}) {
			t.Errorf("in UTC kept %v, want both scans in one day", got)
		}

		got := keptIDs(selectKeep(cands, now, Retention{Keep: &berlinKeep}))
		if !equalIDs(got, []string{"late", "early"}) {
			t.Errorf("in Berlin kept %v, want one scan in each of two days", got)
		}
	})
}

func TestSelectKeepNamesTheRuleThatKept(t *testing.T) {
	t.Parallel()

	loc := time.UTC
	now := at(loc, 2026, time.March, 20, 12)

	cands := []candidate{
		cand("newest", at(loc, 2026, time.March, 20, 11), model.TermIdle),
		cand("yesterday", at(loc, 2026, time.March, 19, 11), model.TermIdle),
		cand("last-month", at(loc, 2026, time.February, 19, 11), model.TermIdle),
		cand("doomed", at(loc, 2026, time.February, 18, 11), model.TermIdle),
	}

	keep := Keep{Last: 1, Daily: 2, Monthly: 2, Location: loc}

	decisions := selectKeep(cands, now, Retention{Keep: &keep})

	for id, want := range map[string]string{
		"newest":     ruleLast,
		"yesterday":  ruleDaily,
		"last-month": ruleMonthly,
		"doomed":     "",
	} {
		if got := ruleFor(decisions, id); got != want {
			t.Errorf("%s was kept by %q, want %q", id, got, want)
		}
	}
}

func TestSelectKeepBounds(t *testing.T) {
	t.Parallel()

	loc := time.UTC
	now := at(loc, 2026, time.March, 20, 12)

	cands := []candidate{
		cand("new-3", at(loc, 2026, time.March, 20, 11), model.TermIdle),
		cand("new-2", at(loc, 2026, time.March, 20, 10), model.TermIdle),
		cand("new-1", at(loc, 2026, time.March, 20, 9), model.TermIdle),
		cand("old", at(loc, 2026, time.January, 1, 9), model.TermIdle),
	}

	tests := []struct {
		name string
		r    Retention
		want []string
	}{
		{
			name: "an age limit deletes everything older, including the only result",
			r:    Retention{MaxAge: 24 * time.Hour},
			want: []string{"new-3", "new-2", "new-1"},
		},
		{
			name: "a count limit keeps the newest N",
			r:    Retention{MaxPerSeries: 2},
			want: []string{"new-3", "new-2"},
		},
		{
			name: "both bounds have to allow a result",
			r:    Retention{MaxAge: 24 * time.Hour, MaxPerSeries: 2},
			want: []string{"new-3", "new-2"},
		},
		{
			name: "no bound keeps everything",
			r:    Retention{},
			want: []string{"new-3", "new-2", "new-1", "old"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if !tc.r.Active() {
				// An inactive policy never reaches selection; assert that
				// rather than the selection's answer.
				return
			}

			if got := keptIDs(selectKeep(cands, now, tc.r)); !equalIDs(got, tc.want) {
				t.Errorf("kept %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSelectKeepIsOrderIndependentAndIdempotent: the order results arrive in
// must not change the answer, and feeding the survivors back in must keep all
// of them — a prune that kept thinning on every pass would eat a history one
// run at a time.
func TestSelectKeepIsOrderIndependentAndIdempotent(t *testing.T) {
	t.Parallel()

	loc := time.UTC
	now := at(loc, 2026, time.March, 20, 12)

	var shuffled []candidate

	for i := range 40 {
		term := model.TermIdle
		if i%7 == 0 {
			term = model.TermTimeout
		}

		shuffled = append(shuffled, cand(
			string(rune('a'+i%26))+time.Duration(i).String(),
			now.Add(-time.Duration(i*7)*time.Hour),
			term,
		))
	}

	// Reverse the input: the selection sorts for itself.
	for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}

	keep := Keep{Last: 2, Daily: 3, Weekly: 2, Location: loc}
	r := Retention{Keep: &keep}

	first := selectKeep(shuffled, now, r)

	survivors := make([]candidate, 0, len(first))

	for _, d := range first {
		if d.Keep {
			survivors = append(survivors, cand(d.ScanID, d.StartedAt, d.Termination))
		}
	}

	second := selectKeep(survivors, now, r)

	if got := len(keptIDs(second)); got != len(survivors) {
		t.Errorf("a second pass kept %d of %d survivors; retention must be idempotent", got, len(survivors))
	}

	if !equalIDs(keptIDs(first), keptIDs(second)) {
		t.Errorf("a second pass kept %v, want %v", keptIDs(second), keptIDs(first))
	}
}

func TestKeepEmptyAndActive(t *testing.T) {
	t.Parallel()

	if !(Keep{}).Empty() {
		t.Error("an unset policy is not reported as keeping nothing")
	}

	if (Keep{Yearly: 1}).Empty() {
		t.Error("a policy with one rule is reported as keeping nothing")
	}

	if (Retention{Keep: &Keep{}}).Active() {
		t.Error("a policy that keeps nothing must not be active")
	}

	if !(Retention{MaxPerSeries: 1}).Active() {
		t.Error("a count bound must be active")
	}
}
