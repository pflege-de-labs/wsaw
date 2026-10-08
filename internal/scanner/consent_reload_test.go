package scanner_test

// A consent click that reloads the page. The helpers wsaw injects before the
// interaction live in the document, and a reload replaces the document: any
// verification that calls them afterwards throws "is not a function", and the
// scan was recorded `unverified` although the choice was recorded and the
// banner was gone. The shape is the one found on touringen.de and wetter.com
// in the Funke run of 2026-10-06.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// reloadingBannerFixtureHTML stores the answer and reloads, and shows its
// banner only while no answer is stored, which is how a server-rendered site
// applies a consent choice.
const reloadingBannerFixtureHTML = `<!DOCTYPE html>
<html lang="de"><head><title>reloading banner fixture</title></head>
<body>
<h1>Touren entdecken</h1>
<p>Eine Seite, die nach der Auswahl neu l&auml;dt.</p>
<script>
(function () {
  if (window.localStorage.getItem('cookie-consent') !== null) return;

  var box = document.createElement('div');
  box.id = 'cookie-consent';
  box.setAttribute('style',
    'position:fixed;top:100px;left:100px;width:700px;height:300px;background:#fff;padding:20px');
  box.innerHTML =
    '<p>Cookies und Privatsph&auml;re</p>' +
    '<p>Wir nutzen Cookies auf unserer Website. Einige von ihnen sind technisch notwendig.</p>' +
    '<button id="all">Alles ausw&auml;hlen</button>' +
    '<button id="deny">Ablehnen</button>';
  document.body.appendChild(box);

  function answer(value) {
    window.localStorage.setItem('cookie-consent', value);
    window.location.reload();
  }

  document.getElementById('all').addEventListener('click', function () { answer('all'); });
  document.getElementById('deny').addEventListener('click', function () { answer('none'); });
})();
</script>
</body></html>`

func TestConsentVerificationSurvivesAReload(t *testing.T) {
	info := requireChrome(t)

	srv := bespokeFixtureServer(t, reloadingBannerFixtureHTML)

	rules, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatal(err)
	}

	s, closePool := newScannerWithRules(t, info, rules)
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, scanTarget("reloading-banner-fixture", srv.URL+"/"), model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	c := out.Result.Consent

	if strings.Contains(c.Reason, "is not a function") {
		t.Fatalf("verification called a helper the reload had removed: %s", c.Reason)
	}

	if c.Outcome != model.OutcomeApplied {
		t.Fatalf("outcome = %q (%s), want applied", c.Outcome, c.Reason)
	}

	if !c.Heuristic {
		t.Error("a label-matched banner was not flagged as heuristic")
	}
}
