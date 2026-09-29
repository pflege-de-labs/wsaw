package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/container"
)

// imageCheckTimeout bounds the whole update check. It runs beside the
// scheduler rather than ahead of it, so an unreachable registry costs this
// long in the background and nothing on the startup path.
const imageCheckTimeout = 15 * time.Second

// CheckBrowserImage asks the browser image's registry whether a newer image
// is published under container.LatestTag and warns with the digest to pin if
// there is (Story 6.11). It never pulls: what the daemon runs changes only
// when an operator changes the configured digest (Story 1.8, AC6).
//
// A check that could not be made is logged as such, never as an image that
// is up to date (Tenet 5). It returns once the check is done or ctx ends.
func (a *App) CheckBrowserImage(ctx context.Context) {
	a.checkBrowserImage(ctx, &http.Client{Timeout: imageCheckTimeout})
}

func (a *App) checkBrowserImage(ctx context.Context, client *http.Client) {
	if a.BrowserImage == "" {
		return
	}

	if on := a.Config.Browser.Container.CheckForUpdates; on != nil && !*on {
		a.Logger.Debug("browser image update check is off", "image", a.BrowserImage)

		return
	}

	ctx, cancel := context.WithTimeout(ctx, imageCheckTimeout)
	defer cancel()

	update, err := container.CheckImage(ctx, client, "wsaw/"+a.Version, a.BrowserImage, container.LatestTag)

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
	default:
		a.Logger.Warn("a newer browser image is published; pull it and pin its digest in browser.container.image",
			"image", a.BrowserImage,
			"latest", update.PullReference(),
			"pull", string(a.Runtime.Kind)+" pull "+update.PullReference(),
		)
	}
}
