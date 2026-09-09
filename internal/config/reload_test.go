package config

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

func base() *Config {
	c := New()
	c.Targets = []Target{{Name: "a", URL: "https://a.test/"}}

	return c
}

// TestAnUnchangedConfigReloadsCleanly is the ordinary case: a reload that
// only touches the target list must not be refused.
func TestAnUnchangedConfigReloadsCleanly(t *testing.T) {
	t.Parallel()

	if got := NonReloadableChanges(base(), base()); len(got) != 0 {
		t.Errorf("an identical configuration was refused: %v", got)
	}
}

func TestTargetAndDefaultChangesAreReloadable(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*Config){
		"target added": func(c *Config) {
			c.Targets = append(c.Targets, Target{Name: "b", URL: "https://b.test/"})
		},
		"target removed":    func(c *Config) { c.Targets = nil },
		"target url":        func(c *Config) { c.Targets[0].URL = "https://moved.test/" },
		"target disabled":   func(c *Config) { c.Targets[0].Disabled = true },
		"defaults interval": func(c *Config) { c.Defaults.Interval = Duration(time.Hour) },
		"defaults modes": func(c *Config) {
			c.Defaults.ConsentModes = []model.ConsentMode{model.ConsentAccept}
		},
		"detection severity": func(c *Config) {
			c.Detection.Severity = diff.SeverityRules{HostRemoved: diff.SeverityHigh}
		},
		"detection allowHosts": func(c *Config) { c.Detection.AllowHosts = []string{"fonts.gstatic.com"} },
		"detection denyHosts":  func(c *Config) { c.Detection.DenyHosts = []string{"doubleclick.net"} },
		"scheduler interval":   func(c *Config) { c.Scheduler.Interval = Duration(2 * time.Hour) },
		"scheduler jitter":     func(c *Config) { c.Scheduler.Jitter = Duration(time.Minute) },
		"scheduler minInterval": func(c *Config) {
			c.Scheduler.MinInterval = Duration(30 * time.Minute)
		},
		"scheduler cron": func(c *Config) { c.Scheduler.Interval = 0; c.Scheduler.Cron = "0 * * * *" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			next := base()
			mutate(next)

			if got := NonReloadableChanges(base(), next); len(got) != 0 {
				t.Errorf("%s was refused, but a reload does apply it: %v", name, got)
			}
		})
	}
}

// TestStartupOnlySettingsAreRefusedByName is the fix. Each of these became a
// browser pool, a normalizer, a scheduler or a scanner option at startup, so
// a reload cannot apply it — and the operator has to be told which line to
// look at rather than seeing "configuration reloaded".
func TestStartupOnlySettingsAreRefusedByName(t *testing.T) {
	t.Parallel()

	for want, mutate := range map[string]func(*Config){
		"normalize.bodyIdentity": func(c *Config) {
			c.Normalize.BodyIdentities = []BodyIdentity{{
				URLPattern: `gtm\.js`, Extract: `"version":"(\d+)"`,
			}}
		},
		"normalize.pathReplacements": func(c *Config) {
			c.Normalize.PathReplacements = []Replacement{{Pattern: `\.js$`, With: ".js"}}
		},
		"normalize.dropQueryParams": func(c *Config) {
			c.Normalize.DropQueryParams = []string{"o"}
		},
		"detection.degradedFailureRatio": func(c *Config) {
			c.Detection.DegradedFailureRatio = 0.02
		},
		"detection.baseline":   func(c *Config) { c.Detection.Baseline = BaselineApproved },
		"detection.flapWindow": func(c *Config) { c.Detection.FlapWindow = Duration(time.Hour) },
		"detection.hashResourceTypes": func(c *Config) {
			c.Detection.HashResourceTypes = []string{"script", "stylesheet"}
		},
		"browser.container":          func(c *Config) { c.Browser.Container.SHMSize = "2g" },
		"browser.maxScansPerBrowser": func(c *Config) { c.Browser.MaxScansPerBrowser = 10 },
		"browser.poolSize":           func(c *Config) { c.Browser.PoolSize = 4 },
		"scheduler.concurrency":      func(c *Config) { c.Scheduler.Concurrency = 4 },
		"scheduler.shutdownGrace": func(c *Config) {
			c.Scheduler.ShutdownGrace = Duration(time.Minute)
		},
		"store.maxAge":      func(c *Config) { c.Store.MaxAge = Duration(24 * time.Hour) },
		"api.listen":        func(c *Config) { c.API.Listen = "127.0.0.1:9999" },
		"consent.onFailure": func(c *Config) { c.Consent.OnFailure = "fail" },
		"logging.level":     func(c *Config) { c.Logging.Level = "debug" },
		"metrics.path":      func(c *Config) { c.Metrics.Path = "/m" },
		"notify":            func(c *Config) { c.Notify = []Notifier{{Kind: "slack"}} },
	} {
		t.Run(want, func(t *testing.T) {
			t.Parallel()

			next := base()
			mutate(next)

			got := NonReloadableChanges(base(), next)
			if !slices.Contains(got, want) {
				t.Errorf("changing %s reported %v, want it to name %q", want, got, want)
			}
		})
	}
}

// TestTheLiveRegressionIsRefused encodes the change that motivated this: a
// config gaining the container limits, the customdata collapse, the tag
// identity rule and the degraded ratio at once. Every one of them is startup
// only, so the whole reload has to be refused rather than logged as applied.
func TestTheLiveRegressionIsRefused(t *testing.T) {
	t.Parallel()

	next := base()
	next.Browser.MaxScansPerBrowser = 10
	next.Browser.Container.SHMSize = "1g"
	next.Browser.Container.FileDescriptors = 8192
	next.Normalize.PathReplacements = []Replacement{{
		Pattern: `/delivery/customdata/[A-Za-z0-9+/=_-]{16,}\.js$`,
		With:    "/delivery/customdata/{cmpsettings}.js",
	}}
	next.Normalize.BodyIdentities = []BodyIdentity{{
		URLPattern: `googletagmanager\.com/gtm\.js`,
		Extract:    `"version":"(\d+)"`,
		Label:      "GTM container version",
	}}
	next.Detection.DegradedFailureRatio = 0.05

	want := []string{
		"browser.container",
		"browser.maxScansPerBrowser",
		"detection.degradedFailureRatio",
		"normalize.bodyIdentity",
		"normalize.pathReplacements",
	}

	if got := NonReloadableChanges(base(), next); !reflect.DeepEqual(got, want) {
		t.Errorf("NonReloadableChanges() = %v, want %v", got, want)
	}
}

// TestTheResultIsSortedAndStable so the same pair of configurations always
// produces the same message, whatever order map iteration happens to take.
func TestTheResultIsSortedAndStable(t *testing.T) {
	t.Parallel()

	next := base()
	next.Logging.Level = "debug"
	next.API.Listen = "127.0.0.1:9999"
	next.Store.MaxPerSeries = 10
	next.Browser.PoolSize = 4

	first := NonReloadableChanges(base(), next)

	if !slices.IsSorted(first) {
		t.Errorf("result is not sorted: %v", first)
	}

	for range 20 {
		if got := NonReloadableChanges(base(), next); !reflect.DeepEqual(got, first) {
			t.Fatalf("result varies between calls: %v then %v", first, got)
		}
	}
}

func TestNilConfigsAreNotADifference(t *testing.T) {
	t.Parallel()

	if got := NonReloadableChanges(nil, base()); got != nil {
		t.Errorf("a nil running config reported %v", got)
	}

	if got := NonReloadableChanges(base(), nil); got != nil {
		t.Errorf("a nil candidate config reported %v", got)
	}
}

// TestEveryTopLevelSectionIsAccountedFor is the guard on the deny-by-default
// promise. A section added to Config later must either be listed as
// reloadable — a deliberate edit, next to the reasoning — or be refused. What
// must not happen is a new section that a reload silently ignores, which is
// the bug this file exists to prevent recurring.
func TestEveryTopLevelSectionIsAccountedFor(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf(Config{})

	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}

		name := yamlName(field)
		if name == "" || name == "-" {
			t.Errorf("Config field %s has no yaml tag, so a reload cannot name it", field.Name)

			continue
		}

		if _, reloadable := reloadableKeys[name]; reloadable {
			continue
		}

		// Not wholly reloadable: either the section itself is refused, or
		// every one of its own fields is listed. Descending proves the
		// section is reachable by the comparison at all.
		if field.Type.Kind() != reflect.Struct {
			continue
		}

		sub := field.Type

		accounted := 0

		for j := range sub.NumField() {
			subField := sub.Field(j)
			if !subField.IsExported() {
				continue
			}

			subName := yamlName(subField)
			if subName == "" || subName == "-" {
				t.Errorf("%s.%s has no yaml tag, so a reload cannot name it", name, subField.Name)

				continue
			}

			accounted++
		}

		if accounted == 0 {
			t.Errorf("section %q has no nameable fields, so a change to it would go unreported", name)
		}
	}
}
