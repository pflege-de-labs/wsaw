package config

import (
	"fmt"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/capture"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Body storage defaults (Story 1.11).
const (
	// DefaultBodyRatioWindow is the timespan a ratio is honoured over when
	// none is configured. A week is long enough that a daily target still
	// has several scans to spread a ratio across, and short enough that a
	// change of ratio shows within an ordinary review cycle.
	DefaultBodyRatioWindow = 7 * 24 * time.Hour

	// DefaultMaxScanBytes caps what one scan may store across all its bodies.
	// It is a quarter of the scan's network budget (capture.DefaultMaxBytes):
	// the bodies worth keeping are text, and a page that transfers more than
	// that is mostly media nobody will open.
	DefaultMaxScanBytes = 64 << 20
)

// Bodies configures which bodies a scan keeps, and for what share of scans
// (Story 1.11). It is accepted under defaults and on each target, and a
// target's fields override the defaults field by field.
type Bodies struct {
	// Store is none, hashed or all. Empty means "not set here".
	Store model.BodyStore `yaml:"store,omitempty"`
	// Ratio is the share of scans that store bodies, from 0 to 1. A pointer,
	// because 0 is a meaningful setting rather than "not set".
	Ratio *float64 `yaml:"ratio,omitempty"`
	// RatioWindow is the timespan the ratio is honoured over.
	RatioWindow Duration `yaml:"ratioWindow,omitempty"`
	// RequestBodies also stores what the page sent.
	RequestBodies *bool `yaml:"requestBodies,omitempty"`
	// MaxBodyBytes caps one body; MaxScanBytes caps all bodies of one scan.
	MaxBodyBytes int64 `yaml:"maxBodyBytes,omitempty"`
	MaxScanBytes int64 `yaml:"maxScanBytes,omitempty"`
}

// BodyPolicy is a target's resolved body storage.
type BodyPolicy struct {
	Store         model.BodyStore
	Ratio         float64
	RatioWindow   time.Duration
	RequestBodies bool
	MaxBodyBytes  int64
	MaxScanBytes  int64
}

// Enabled reports whether this policy stores anything at all.
func (p BodyPolicy) Enabled() bool {
	return p.Store == model.BodyStoreHashed || p.Store == model.BodyStoreAll
}

// levelBodies is one level's bodies block, with storeBodies read as the
// shorthand it is: true means {store: hashed, ratio: 1}, which is exactly what
// it stored before Story 1.11, and false means {store: none}. Setting both on
// one level is refused by validation, so here at most one of them is set.
func levelBodies(t *Target) Bodies {
	if t.Bodies != nil {
		return *t.Bodies
	}

	if t.StoreBodies == nil {
		return Bodies{}
	}

	if !*t.StoreBodies {
		return Bodies{Store: model.BodyStoreNone}
	}

	one := 1.0

	return Bodies{Store: model.BodyStoreHashed, Ratio: &one}
}

// bodyPolicy resolves a target's body storage over the defaults.
func (c *Config) bodyPolicy(t *Target) BodyPolicy {
	own, def := levelBodies(t), levelBodies(&c.Defaults)

	p := BodyPolicy{
		Store:         model.BodyStoreNone,
		Ratio:         1,
		RatioWindow:   firstDuration(own.RatioWindow, def.RatioWindow, DefaultBodyRatioWindow),
		RequestBodies: firstBool(own.RequestBodies, def.RequestBodies, false),
		MaxBodyBytes:  firstInt64(own.MaxBodyBytes, def.MaxBodyBytes, capture.DefaultMaxBodyBytes),
		MaxScanBytes:  firstInt64(own.MaxScanBytes, def.MaxScanBytes, DefaultMaxScanBytes),
	}

	switch {
	case own.Store != "":
		p.Store = own.Store
	case def.Store != "":
		p.Store = def.Store
	}

	switch {
	case own.Ratio != nil:
		p.Ratio = *own.Ratio
	case def.Ratio != nil:
		p.Ratio = *def.Ratio
	}

	return p
}

// validateBodies checks one level's own settings. Settings that only make
// sense once the levels are merged are checked by validateBodyPolicy.
func validateBodies(t *Target, field string, add addFunc) {
	if t.Bodies != nil && t.StoreBodies != nil {
		// Two settings that disagree about evidence must not be settled by a
		// precedence rule nobody reads (Tenet 15).
		add(t.line, field, "both storeBodies and bodies are set; storeBodies: true is shorthand for "+
			"bodies: {store: hashed, ratio: 1}, so use one or the other")
	}

	if t.Bodies == nil {
		return
	}

	b := t.Bodies

	if b.Store != "" && !b.Store.Valid() {
		add(t.line, field+".bodies.store", "%q is not a valid mode; use none, hashed or all", b.Store)
	}

	if b.Ratio != nil && (*b.Ratio < 0 || *b.Ratio > 1) {
		add(t.line, field+".bodies.ratio", "%v is not a share of scans; use a number from 0 to 1", *b.Ratio)
	}

	if b.MaxBodyBytes < 0 {
		add(t.line, field+".bodies.maxBodyBytes", "must not be negative")
	}

	if b.MaxScanBytes < 0 {
		add(t.line, field+".bodies.maxScanBytes", "must not be negative")
	}
}

// validateBodyPolicy checks a target's merged body storage.
func validateBodyPolicy(p BodyPolicy, interval time.Duration, line int, field string, add addFunc) {
	if !p.Enabled() {
		return
	}

	if p.MaxBodyBytes > p.MaxScanBytes {
		add(line, field+".bodies.maxBodyBytes",
			"%d is larger than maxScanBytes (%d); one body could never be stored whole", p.MaxBodyBytes, p.MaxScanBytes)
	}

	// A window that can hold at most one scan cannot express a ratio: every
	// scan would be the first of its window, and would be sampled. A cron
	// schedule has no fixed interval to check against, so it is not refused
	// here; its window still behaves, it merely samples more than asked if
	// the window is shorter than the gaps between runs.
	if p.Ratio > 0 && p.Ratio < 1 && interval > 0 && p.RatioWindow < interval {
		add(line, field+".bodies.ratioWindow",
			"%s is shorter than the scan interval (%s); a window that holds at most one scan cannot express a ratio of %v",
			p.RatioWindow, interval, p.Ratio)
	}
}

// String describes a policy in one line, for the startup log.
func (p BodyPolicy) String() string {
	if !p.Enabled() {
		return string(model.BodyStoreNone)
	}

	return fmt.Sprintf("%s, ratio %v over %s, request bodies %v", p.Store, p.Ratio, p.RatioWindow, p.RequestBodies)
}
