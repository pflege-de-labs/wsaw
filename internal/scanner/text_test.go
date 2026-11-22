package scanner_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/capture"
	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// legalSite serves a privacy statement that can be switched from complete to
// cut, and a short imprint with no in-page links, under site.test (Story
// 4.15, AC11). The page also redefines innerText for its own scripts, which
// must not change what wsaw reads (Tenet 9).
func legalSite(t *testing.T, cut, imprintComplete *atomic.Bool) *fixtureSite {
	t.Helper()

	f := &fixtureSite{}

	f.thirdParty = httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(f.thirdParty.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		_, _ = w.Write([]byte{0x00, 0x00, 0x01, 0x00})
	})

	mux.HandleFunc("/datenschutz/", func(w http.ResponseWriter, _ *http.Request) {
		body := `<h2 id="s1">1. Verantwortlicher</h2><p>web care LBJ GmbH</p>
<h2 id="s14">14. Dienste Dritter</h2><p>YouTube erlaubt das kostenlose Einstellen von Videoclips und deren kostenfreie Ansicht.</p>
<h2 id="s15">15. Ihre Rechte</h2><p>Auskunft, Berichtigung, Löschung.</p>`
		if cut.Load() {
			body = `<h2 id="s1">1. Verantwortlicher</h2><p>web care LBJ GmbH</p>
<h2 id="s14">14. Dienste Dritter</h2><p>YouTube erlaubt das kostenlose Einstellen von Videoclips und deren kostenfre</p>`
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>Datenschutz</title>
<script>Object.defineProperty(HTMLElement.prototype, 'innerText', {get: function () { return 'LIE'; }});</script>
</head><body><nav>Menü</nav>
<main>
<ol><li><a href="#s1">Verantwortlicher</a></li><li><a href="#s14">Dienste</a></li><li><a href="#s15">Ihre Rechte</a></li><li><a href="#">nach oben</a></li></ol>
<p>Kontakt: <a href="mailto:datenschutz@site.test">Datenschutz-Kontakt</a>, <a href="/presse/">Presse</a>,
<a href="javascript:void(0)">Drucken</a></p>
%s
</main><footer>Footer</footer></body></html>`, body)
	})

	mux.HandleFunc("/impressum/", func(w http.ResponseWriter, _ *http.Request) {
		reg := "<p>Handelsregister: HRB 117601, Amtsgericht Hamburg</p>"
		if !imprintComplete.Load() {
			reg = ""
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>Impressum</title></head><body>
<div id="imprint"><h1>Impressum</h1><p>web care LBJ GmbH</p><p>Großer Grasbrook 9, 20457 Hamburg</p>%s
<p>USt-IdNr: DE276352378</p></div></body></html>`, reg)
	})

	f.site = httptest.NewServer(mux)
	t.Cleanup(f.site.Close)

	return f
}

// Story 4.15, AC3, AC8, AC11 and AC14 against a real browser: the text and
// its links are read from the region after settle, a cut is reported as a
// changed and incomplete text with the dangling fragment named, and an imprint
// with no in-page links reports the line that went missing.
func TestTextWatchAgainstFixturePages(t *testing.T) {
	info := requireChrome(t)

	var cut, imprintComplete atomic.Bool

	imprintComplete.Store(true)

	site := legalSite(t, &cut, &imprintComplete)
	s, closePool := newScanner(t, info, site)

	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	privacy := site.target(t)
	privacy.URL = "http://" + siteHost + "/datenschutz/"
	privacy.Text = &capture.TextRegion{Selector: "main", MaxBytes: 1 << 20}
	privacyChecks := diff.Options{Text: &diff.TextChecks{MustContain: []string{"15. Ihre Rechte"}, CheckAnchors: true}}

	scan := func() *model.Result {
		t.Helper()

		out, err := s.Scan(ctx, privacy, model.ConsentNone)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}

		if !out.Result.OK() || out.Result.Text == nil {
			t.Fatalf("scan: termination=%s error=%s text=%+v", out.Result.Termination, out.Result.Error, out.Result.Text)
		}

		return out.Result
	}

	complete := scan()
	tc := complete.Text

	if tc.Unavailable != "" || strings.Contains(tc.Text, "LIE") || !strings.Contains(tc.Text, "15. Ihre Rechte") ||
		strings.Contains(tc.Text, "Menü") || strings.Contains(tc.Text, "Footer") {
		t.Fatalf("complete text = %+v", tc)
	}

	wantLinks := map[string]string{
		"Datenschutz-Kontakt": "mailto:datenschutz@site.test",
		"Presse":              "http://site.test/presse/",
	}
	if len(tc.Links) != len(wantLinks) {
		t.Errorf("links = %+v, want the mailto and the relative link only", tc.Links)
	}

	for _, l := range tc.Links {
		if wantLinks[l.Text] != l.Target {
			t.Errorf("link %+v, want target %q", l, wantLinks[l.Text])
		}
	}

	if len(tc.Fragments) != 3 {
		t.Errorf("fragments = %+v, want #s1, #s14, #s15 (a bare # is not a link)", tc.Fragments)
	}

	if got := changesOfType(diff.Compare(nil, complete, privacyChecks), diff.TextIncomplete); len(got) != 0 {
		t.Errorf("the complete page is reported incomplete: %+v", got)
	}

	// Determinism: an unchanged page gives no text change (Tenet 6).
	again := scan()
	if got := changesOfType(diff.Compare(complete, again, privacyChecks), diff.TextChanged); len(got) != 0 {
		t.Errorf("an unchanged page changed: %+v", got)
	}

	cut.Store(true)

	cutRes := scan()
	rep := diff.Compare(complete, cutRes, privacyChecks)

	if got := changesOfType(rep, diff.TextChanged); len(got) != 1 {
		t.Errorf("text-changed = %+v", got)
	}

	inc := changesOfType(rep, diff.TextIncomplete)
	if len(inc) != 1 || !strings.Contains(inc[0].Detail, `"#s15"`) || !strings.Contains(inc[0].Detail, `missing "15. Ihre Rechte"`) {
		t.Errorf("text-incomplete = %+v", inc)
	}

	// The imprint: no in-page links at all, so only mustContain can tell.
	imprint := site.target(t)
	imprint.URL = "http://" + siteHost + "/impressum/"
	imprint.Text = &capture.TextRegion{Selector: "#imprint", MaxBytes: 1 << 20}
	imprintChecks := diff.Options{Text: &diff.TextChecks{
		MustContain: []string{"web care LBJ GmbH", "HRB 117601", "DE276352378"}, CheckAnchors: true,
	}}

	privacy = imprint

	if got := changesOfType(diff.Compare(nil, scan(), imprintChecks), diff.TextIncomplete); len(got) != 0 {
		t.Errorf("the complete imprint is reported incomplete: %+v", got)
	}

	imprintComplete.Store(false)

	inc = changesOfType(diff.Compare(nil, scan(), imprintChecks), diff.TextIncomplete)
	if len(inc) != 1 || inc[0].Detail != `missing "HRB 117601"` {
		t.Errorf("imprint without its register line = %+v", inc)
	}

	// A selector that matches nothing is a reason, not an empty text.
	privacy.Text = &capture.TextRegion{Selector: "#no-such-region", MaxBytes: 1 << 20}

	if got := scan().Text; got.Unavailable != "the selector matched nothing" || got.Text != "" {
		t.Errorf("missing region = %+v", got)
	}
}

// Story 4.15, AC3: the text is read after consent handling, so in reject mode
// it is the page's text and not the banner's; with no interaction the banner
// is still there and is part of the region.
func TestTextIsReadAfterConsent(t *testing.T) {
	info := requireChrome(t)

	site := newFixtureSite(t, true)
	s, closePool := newScanner(t, info, site)

	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	target := site.target(t, model.ConsentNone, model.ConsentReject)
	target.Text = &capture.TextRegion{Selector: "body", MaxBytes: 1 << 20}

	for mode, wantBanner := range map[model.ConsentMode]bool{model.ConsentNone: true, model.ConsentReject: false} {
		out, err := s.Scan(ctx, target, mode)
		if err != nil {
			t.Fatalf("Scan %s: %v", mode, err)
		}

		tc := out.Result.Text
		if tc == nil || tc.Unavailable != "" || !strings.Contains(tc.Text, "fixture site") {
			t.Fatalf("%s: text = %+v", mode, tc)
		}

		if got := strings.Contains(tc.Text, "We use cookies"); got != wantBanner {
			t.Errorf("%s: banner text in the region = %v, want %v", mode, got, wantBanner)
		}
	}
}

func changesOfType(rep *diff.Report, typ diff.ChangeType) []diff.Change {
	var out []diff.Change

	for _, c := range rep.Changes {
		if c.Type == typ {
			out = append(out, c)
		}
	}

	return out
}
