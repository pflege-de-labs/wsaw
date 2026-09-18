package httpapi

// The budget behind the typed-URL page (Story 5.27), tested directly: a
// rolling window cannot be observed through requests without waiting an hour.

import (
	"testing"
	"time"
)

func TestRollingBudgetSpendsAndRefuses(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	b := newRollingBudget(2, time.Hour)

	if _, ok := b.take(now); !ok {
		t.Fatal("the first scan was refused")
	}

	if _, ok := b.take(now.Add(time.Minute)); !ok {
		t.Fatal("the second scan was refused")
	}

	wait, ok := b.take(now.Add(2 * time.Minute))
	if ok {
		t.Fatal("a third scan was allowed inside the window")
	}

	// The wait is until the oldest start leaves the window, not a guess.
	if want := 58 * time.Minute; wait != want {
		t.Errorf("wait = %s, want %s", wait, want)
	}

	if got := b.remaining(now.Add(2 * time.Minute)); got != 0 {
		t.Errorf("remaining = %d, want 0", got)
	}

	// One slot comes back when the first start ages out, not both.
	if got := b.remaining(now.Add(time.Hour)); got != 1 {
		t.Errorf("remaining after the first start expired = %d, want 1", got)
	}

	if _, ok := b.take(now.Add(time.Hour)); !ok {
		t.Error("the freed slot was not usable")
	}

	if _, ok := b.take(now.Add(time.Hour)); ok {
		t.Error("the window gave back more than it freed")
	}
}

func TestRollingBudgetIsWholeAgainAfterTheWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	b := newRollingBudget(3, time.Hour)

	for range 3 {
		if _, ok := b.take(now); !ok {
			t.Fatal("a scan inside the budget was refused")
		}
	}

	if got := b.remaining(now.Add(time.Hour + time.Second)); got != 3 {
		t.Errorf("remaining after the window passed = %d, want 3", got)
	}
}
