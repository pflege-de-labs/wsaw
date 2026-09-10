package httpapi_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/daemon"
	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/metrics"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

type noopScanner struct{}

func (noopScanner) Scan(context.Context, config.Resolved, model.ConsentMode) (scanner.Outcome, error) {
	return scanner.Outcome{}, nil
}

// The dashboard's Schedule table must read soonest-first, so a reader can
// tell what wsaw will do next without hunting through config-declaration
// order for the smallest "next run".
func TestScheduleIsSortedByNextRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	st, err := store.Open(store.Options{
		Path:        dir + "/wsaw.db",
		ArtifactDir: dir + "/artifacts",
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	// "later" is declared first but its cron next-occurrence (Jan 1) is far
	// behind "sooner", declared second, which fires every minute.
	targets := []config.Resolved{
		{
			Name:         "later",
			URL:          "https://later.example/",
			ConsentModes: []model.ConsentMode{model.ConsentNone},
			Cron:         "0 0 1 1 *",
		},
		{
			Name:         "sooner",
			URL:          "https://sooner.example/",
			ConsentModes: []model.ConsentMode{model.ConsentNone},
			Cron:         "* * * * *",
		},
	}

	dmn, err := daemon.New(noopScanner{}, nil, targets, daemon.Options{})
	if err != nil {
		t.Fatal(err)
	}

	reg := metrics.New("test")
	reg.SetReady(true, true)

	srv, err := httpapi.New(httpapi.Options{WebUI: true, Version: "test"}, httpapi.Deps{
		Store:   st,
		Logger:  slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
		Metrics: reg,
		Daemon:  dmn,
		Targets: func() []config.Resolved { return targets },
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	html := body(t, resp)

	i := strings.Index(html, "<h2>Schedule</h2>")
	if i < 0 {
		t.Fatal("dashboard has no Schedule section")
	}

	scheduleHTML := html[i:]

	sooner := strings.Index(scheduleHTML, "sooner")
	later := strings.Index(scheduleHTML, "later")

	if sooner < 0 || later < 0 {
		t.Fatalf("both targets must appear in the schedule table; sooner=%d later=%d", sooner, later)
	}

	if sooner > later {
		t.Error("schedule table is not sorted by next run: \"later\" appears before \"sooner\"")
	}
}
