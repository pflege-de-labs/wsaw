package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/share"
)

// cmdShare mints a share link from the command line (Story 5.19, AC1).
//
// It exists alongside the button in the interface because the interface is
// not always exposed: a wsaw on a private network still needs a way to hand
// somebody one result, and an operator with shell access should not have to
// publish the API to do it.
func cmdShare(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("share", flag.ContinueOnError)

	var cf configFlags

	cf.register(fs)

	target := fs.String("target", "", "the target to share a scan of")
	mode := fs.String("mode", string(model.ConsentReject), "consent mode: none, reject or accept")
	scan := fs.String("scan", "latest", "scan ID, or \"latest\"")
	validity := fs.Duration("validity", 0, "how long the link lasts; empty uses the configured default")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *target == "" {
		return fmt.Errorf("share: --target is required")
	}

	consentMode := model.ConsentMode(*mode)
	if !consentMode.Valid() {
		return fmt.Errorf("share: --mode must be none, reject or accept")
	}

	cfg, err := cf.load()
	if err != nil {
		return err
	}

	if !cfg.API.Share.Enabled {
		return fmt.Errorf(
			"share: sharing is not enabled; set api.share.enabled and api.share.key in the configuration")
	}

	// No browser: this command reads the store and signs a token.
	a, err := app.New(ctx, cfg, app.Options{Version: version})
	if err != nil {
		return err
	}

	defer func() { _ = a.Close() }()

	res, err := loadShareTarget(a, *target, consentMode, *scan)
	if err != nil {
		return err
	}

	key, err := secret.Resolve(cfg.API.Share.Key)
	if err != nil {
		return fmt.Errorf("api.share.key: %w", err)
	}

	signer, err := share.New(share.Options{
		Key:         key.Reveal(),
		Validity:    cfg.API.Share.ShareValidity(),
		MaxValidity: cfg.API.Share.ShareMaxValidity(),
	})
	if err != nil {
		return err
	}

	token, claims, err := signer.Mint(res.Target, string(res.ConsentMode), res.ScanID, *validity)
	if err != nil {
		return err
	}

	link := sharePathFor(res.Target, string(res.ConsentMode), res.ScanID) + "?t=" + token

	if base := cfg.API.Share.BaseURL; base != "" {
		link = trimTrailingSlash(base) + link
	}

	// The link goes to stdout on its own, so it can be piped; everything else
	// goes to stderr. A failed write here matters: a caller piping the link
	// somewhere must not be told the link was delivered when it was not.
	if _, err := fmt.Fprintln(os.Stdout, link); err != nil {
		return fmt.Errorf("share: writing the link: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stderr, "\nshares %s/%s/%s, read-only, until %s (%s from now)\n",
		res.Target, res.ConsentMode, res.ScanID,
		claims.Expiry().Format(time.RFC3339), time.Until(claims.Expiry()).Round(time.Minute))

	if cfg.API.Share.BaseURL == "" {
		_, _ = fmt.Fprintln(os.Stderr,
			"this is a path, not a URL: set api.share.baseUrl to have wsaw print a sendable link")
	}

	_, _ = fmt.Fprintln(os.Stderr,
		"this link cannot be revoked before it expires; rotate api.share.key to invalidate every outstanding link")

	return nil
}

// loadShareTarget resolves the scan to share, accepting "latest" so an
// operator does not have to look a scan ID up first.
func loadShareTarget(a *app.App, target string, mode model.ConsentMode, scan string) (*model.Result, error) {
	if scan == "" || scan == "latest" {
		res, err := a.Store.LatestResult(target, mode)
		if err != nil {
			return nil, fmt.Errorf("share: no scan to share for %s/%s: %w", target, mode, err)
		}

		return res, nil
	}

	res, err := a.Store.GetResult(target, mode, scan)
	if err != nil {
		return nil, fmt.Errorf("share: %w", err)
	}

	return res, nil
}

func sharePathFor(target, mode, scan string) string {
	return "/shared/" + target + "/" + mode + "/" + scan
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}

	return s
}
