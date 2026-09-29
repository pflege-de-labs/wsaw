package app

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/container"
)

// imageCheckTimeout bounds the whole update check. It runs beside the
// scheduler rather than ahead of it, so an unreachable registry costs this
// long in the background and nothing on the startup path.
const imageCheckTimeout = 15 * time.Second

// imageChecker compares a pinned image with what its registry publishes.
type imageChecker func(ctx context.Context, image string) (container.ImageUpdate, error)

// CheckBrowserImage asks the browser image's registry whether a newer image
// is published under container.LatestTag and warns with the digest to pin if
// there is (Story 6.11). It never pulls: what the daemon runs changes only
// when an operator changes the configured digest (Story 1.8, AC6).
//
// A check that could not be made is logged as such, never as an image that
// is up to date (Tenet 5). It returns once the check is done or ctx ends.
func (a *App) CheckBrowserImage(ctx context.Context) {
	client := &http.Client{Timeout: imageCheckTimeout}

	a.checkBrowserImage(ctx, func(ctx context.Context, image string) (container.ImageUpdate, error) {
		return container.CheckImage(ctx, client, "wsaw/"+a.Version, image, container.LatestTag)
	})
}

func (a *App) checkBrowserImage(ctx context.Context, check imageChecker) {
	if a.BrowserImage == "" {
		return
	}

	if on := a.Config.Browser.Container.CheckForUpdates; on != nil && !*on {
		a.Logger.Debug("browser image update check is off", "image", a.BrowserImage)

		return
	}

	ctx, cancel := context.WithTimeout(ctx, imageCheckTimeout)
	defer cancel()

	update, err := check(ctx, a.BrowserImage)

	switch {
	case errors.Is(err, container.ErrNotPinned):
		a.Logger.Debug("browser image is not pinned by digest; not checking for a newer one", "image", a.BrowserImage)
	case ctx.Err() != nil && errors.Is(err, context.Canceled):
		// Shutdown overtook the check; there is no outcome to report.
	case err != nil:
		a.Logger.Warn("could not check for a newer browser image",
			"image", a.BrowserImage, "error", err)
	case update.Current:
		a.Logger.Info("browser image is the latest published", "image", a.BrowserImage, "checked", update.Checked)
	case sameChromium(a.Chrome.Version, update.LatestChromium):
		// Every release rebuilds the image with its own version labels, so
		// the digest moves even when the browser does not. The browser is
		// what a result depends on, so that is what decides.
		a.Logger.Info("browser image runs the latest published Chromium",
			"image", a.BrowserImage, "chromium", update.LatestChromium, "latest", update.PullReference())
	default:
		args := []any{
			"image", a.BrowserImage,
			"latest", update.PullReference(),
			"pull", string(a.Runtime.Kind) + " pull " + update.PullReference(),
		}
		if update.LatestChromium != "" {
			args = append(args, "running", a.Chrome.Version, "latest_chromium", update.LatestChromium)
		}

		a.Logger.Warn("a newer browser image is published; pull it and pin its digest in browser.container.image", args...)
	}
}

// chromiumVersion matches a four-part Chromium version wherever it appears:
// "Chromium 152.0.7977.82" as the browser reports itself, and
// "152.0.7977.82-r0" as an Alpine package version names it.
var chromiumVersion = regexp.MustCompile(`\b\d+\.\d+\.\d+\.\d+\b`)

// sameChromium reports whether the running browser's version and a latest
// image's declared container.LabelChromiumVersion name the same Chromium.
// Either one missing or unreadable is not a match: the check then falls back
// to the digest, which errs towards saying an update exists.
func sameChromium(running, label string) bool {
	r := chromiumVersion.FindString(running)
	l := chromiumVersion.FindString(label)

	return r != "" && r == l
}
