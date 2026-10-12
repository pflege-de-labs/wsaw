package diff_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
)

// TestSeverityRulesListsEveryField guards the hand-written list behind Over
// and Each: a rule added to the struct but not to that list would be dropped
// by configuration merging and skipped by validation without any error.
func TestSeverityRulesListsEveryField(t *testing.T) {
	t.Parallel()

	var base diff.SeverityRules

	rv := reflect.ValueOf(&base).Elem()
	rt := rv.Type()

	// Each field gets its own YAML key as value, so a list entry pointing at
	// the wrong field or carrying the wrong key shows up as a mismatch.
	for i := range rt.NumField() {
		rv.Field(i).SetString(yamlKey(rt.Field(i)))
	}

	if got := (diff.SeverityRules{}).Over(base); got != base {
		t.Errorf("unset rules over base = %+v, want %+v", got, base)
	}

	if got := base.Over(diff.SeverityRules{}); got != base {
		t.Errorf("set rules over empty base = %+v, want %+v", got, base)
	}

	seen := make(map[string]bool, rt.NumField())

	base.Each(func(key string, sev diff.Severity) {
		if string(sev) != key {
			t.Errorf("Each(%q) = %q, want the field tagged %q", key, sev, key)
		}

		seen[key] = true
	})

	if len(seen) != rt.NumField() {
		t.Errorf("Each visited %d rules, struct has %d", len(seen), rt.NumField())
	}
}

func TestSeverityRulesOverKeepsSetRules(t *testing.T) {
	t.Parallel()

	own := diff.SeverityRules{HostRemoved: diff.SeverityHigh}
	base := diff.SeverityRules{HostRemoved: diff.SeverityLow, DeniedHost: diff.SeverityLow}

	got := own.Over(base)
	want := diff.SeverityRules{HostRemoved: diff.SeverityHigh, DeniedHost: diff.SeverityLow}

	if got != want {
		t.Errorf("Over = %+v, want %+v", got, want)
	}
}

func yamlKey(f reflect.StructField) string {
	key, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")

	return key
}
