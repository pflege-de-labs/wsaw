package capture

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/domstorage"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"

	"github.com/pflege-de-labs/wsaw/internal/classify"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// maxStorageEntries bounds what one scan records. A page can write unbounded
// amounts of Web Storage, and a hostile one will; the cap is generous enough
// that an ordinary site is recorded whole and small enough that a result
// document stays readable (Tenet 17).
const maxStorageEntries = 500

// collectStorage reads localStorage and sessionStorage for every frame of
// this scan once the page has settled.
//
// It exists because a cookie list describes only half of what a site keeps on
// a visitor's machine. Consent state and analytics identifiers live in
// localStorage on a large class of sites — one that writes
// localStorage["cookie-accepted"] leaves no cookie at all — and a result
// reporting no cookies for such a site is true and misleading at once
// (Story 2.9, AC5).
//
// The values are read through CDP rather than by evaluating script in the
// page, because the page is hostile input: a script that redefines
// window.localStorage can lie to an injected snippet, and cannot lie to the
// browser (NFR §4).
//
// It returns the origins it walked, which is the set clearStorage has to wipe
// for the next scan on a reused browser to start from nothing.
func (s *session) collectStorage() []string {
	const storageTimeout = 10 * time.Second

	// A fresh context: the run context may already be done, and the storage
	// is still worth collecting, exactly as for the cookie jar.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.runCtx), storageTimeout)
	defer cancel()

	cl, err := classify.New(s.opts.URL, s.opts.FirstPartyDomains)
	if err != nil {
		return nil
	}

	var (
		entries   []model.StorageEntry
		origins   []string
		truncated bool
	)

	err = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := domstorage.Enable().Do(ctx); err != nil {
			return fmt.Errorf("enabling DOM storage: %w", err)
		}

		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return fmt.Errorf("reading the frame tree: %w", err)
		}

		for _, frame := range flattenFrames(tree) {
			origin := frameOrigin(frame)
			if origin == "" {
				continue
			}

			key, err := storage.GetStorageKey().WithFrameID(frame.ID).Do(ctx)
			if err != nil {
				// A frame can be gone by now, or carry an opaque origin. That
				// is not a scan failure; it is one frame with no storage to
				// report.
				continue
			}

			origins = append(origins, origin)

			party := cl.Classify(origin).Party

			for _, area := range []model.StorageArea{model.StorageLocal, model.StorageSession} {
				found, hitCap := readStorageArea(ctx, storageRead{
					key:    string(key),
					origin: origin,
					area:   area,
					party:  party,
					room:   maxStorageEntries - len(entries),
				})

				entries = append(entries, found...)

				if hitCap {
					truncated = true
				}
			}
		}

		return nil
	}))
	if err != nil {
		s.res.Warnings = append(s.res.Warnings, "web storage could not be read: "+s.scrub(err.Error()))

		return origins
	}

	if truncated {
		s.res.Warnings = append(s.res.Warnings, fmt.Sprintf(
			"web storage was recorded up to the cap of %d entries; the page kept more", maxStorageEntries))
	}

	sortStorage(entries)
	s.res.Storage = entries

	return origins
}

// storageRead is one area of one frame's storage: what to read and how much
// room is left in the scan's budget.
type storageRead struct {
	key    string
	origin string
	area   model.StorageArea
	party  model.Party
	room   int
}

// readStorageArea reads one storage area, stopping at the budget. An area
// that cannot be read yields nothing rather than failing the collection: one
// unreadable frame is not a reason to lose the rest.
func readStorageArea(ctx context.Context, r storageRead) ([]model.StorageEntry, bool) {
	if r.room <= 0 {
		return nil, true
	}

	items, err := domstorage.GetDOMStorageItems(&domstorage.StorageID{
		StorageKey:     domstorage.SerializedStorageKey(r.key),
		IsLocalStorage: r.area == model.StorageLocal,
	}).Do(ctx)
	if err != nil {
		return nil, false
	}

	out := make([]model.StorageEntry, 0, len(items))

	for _, item := range items {
		if len(out) >= r.room {
			return out, true
		}

		name, value := storageItem(item)
		if name == "" {
			continue
		}

		out = append(out, model.StorageEntry{
			Origin:      r.origin,
			Area:        r.area,
			Key:         name,
			Party:       r.party,
			ValueSHA256: sha256Hex([]byte(value)),
			ValueLength: len(value),
		})
	}

	return out, false
}

// storageItem unpacks one [key, value] pair. The protocol types it as a
// free-form array, so a malformed pair is skipped rather than panicking on a
// hostile page's behalf.
func storageItem(item domstorage.Item) (string, string) {
	switch len(item) {
	case 0:
		return "", ""
	case 1:
		return item[0], ""
	default:
		return item[0], item[1]
	}
}

func flattenFrames(tree *page.FrameTree) []*cdp.Frame {
	if tree == nil {
		return nil
	}

	var out []*cdp.Frame

	if tree.Frame != nil {
		out = append(out, tree.Frame)
	}

	for _, child := range tree.ChildFrames {
		out = append(out, flattenFrames(child)...)
	}

	return out
}

// frameOrigin returns the frame's security origin, skipping the opaque ones
// ("://", "null") that carry no storage a reader could act on.
func frameOrigin(frame *cdp.Frame) string {
	origin := strings.TrimSpace(frame.SecurityOrigin)
	if origin == "" || origin == "null" || !strings.HasPrefix(origin, "http") {
		return ""
	}

	return origin
}

// sortStorage orders entries deterministically, so two scans of an unchanged
// site produce an identical list (Tenet 6).
func sortStorage(entries []model.StorageEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Origin != entries[j].Origin {
			return entries[i].Origin < entries[j].Origin
		}

		if entries[i].Area != entries[j].Area {
			return entries[i].Area < entries[j].Area
		}

		return entries[i].Key < entries[j].Key
	})
}

// clearStorage wipes quota storage — localStorage, IndexedDB, service
// workers, cache storage — for every origin this scan reached, so a browser
// that serves another scan hands it nothing to inherit (Story 1.5, AC1).
//
// It runs after the result has been assembled: what the page stored is
// evidence and is recorded first. The origins come from the frame tree
// collectStorage walked, plus the requested and final URLs, because those two
// differ whenever a site redirects — and the origin a redirect lands on is
// exactly where a site keeps the consent decision that must not carry over.
//
// A wipe that fails is recorded as a warning rather than an error: the scan
// itself is complete and correct, and the risk it leaves behind belongs to
// the next one, whose result already says whether its browser was reused.
func (s *session) clearStorage(origins []string) {
	const clearTimeout = 10 * time.Second

	seen := make(map[string]struct{}, len(origins)+2)
	targets := make([]string, 0, len(origins)+2)

	for _, raw := range append(origins, s.opts.URL, s.res.FinalURL) {
		origin := originOf(raw)
		if origin == "" {
			continue
		}

		if _, dup := seen[origin]; dup {
			continue
		}

		seen[origin] = struct{}{}
		targets = append(targets, origin)
	}

	if len(targets) == 0 {
		return
	}

	// A fresh context, as for the collectors: the run context may already be
	// done, and this is precisely when the wipe matters most.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.runCtx), clearTimeout)
	defer cancel()

	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		for _, origin := range targets {
			if err := storage.ClearDataForOrigin(origin, string(storage.TypeAll)).Do(ctx); err != nil {
				return fmt.Errorf("clearing storage for %s: %w", origin, err)
			}
		}

		return nil
	}))
	if err != nil {
		s.res.Warnings = append(s.res.Warnings,
			"web storage could not be cleared after the scan; a reused browser may carry it into the next one: "+
				s.scrub(err.Error()))
	}
}

// originOf returns the scheme://host[:port] form Chrome's storage calls take,
// or "" for anything without one — an opaque origin, a data: URL, a value the
// page controls. A frame's security origin is already in that form; a URL is
// not, so both paths go through here.
func originOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}

	return u.Scheme + "://" + u.Host
}
