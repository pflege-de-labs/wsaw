package config

import (
	"reflect"
	"sort"
	"strings"
)

// A reload replaces the target list and the per-target settings derived from
// it, and nothing else. Every other section became a browser pool, a
// normalizer, an HTTP server, a store handle or a scheduler when the process
// started, and a running daemon has no way to swap those out underneath
// in-flight scans.
//
// The consequence is that a reload has to refuse a file whose other sections
// moved, rather than adopt the half it can. Accepting such a file and
// applying part of it is the failure this whole package exists to avoid: the
// operator sees "configuration reloaded", believes the change took effect,
// and the daemon goes on scanning with the old normalizer (Tenet 5).
//
// reloadableKeys is therefore a deny-by-default list, keyed by the name a
// section has in the file. A field added later and not listed here counts as
// non-reloadable, so the new failure mode is a loud refusal rather than a
// silent no-op — and making something reloadable is a deliberate edit here,
// next to the reasoning.
var reloadableKeys = map[string]struct{}{
	// The target list itself, and the defaults every target inherits.
	"defaults": {},
	"targets":  {},

	// These three are read through the resolved per-target settings the
	// reload hands to the scheduler, so a change genuinely does take effect.
	"detection.severity":   {},
	"detection.allowHosts": {},
	"detection.denyHosts":  {},

	// Schedule shape is resolved per target too.
	"scheduler.interval":    {},
	"scheduler.cron":        {},
	"scheduler.jitter":      {},
	"scheduler.minInterval": {},
}

// NonReloadableChanges returns the configuration keys that differ between the
// running configuration and a candidate one and that a running daemon cannot
// adopt. The names are the ones an operator sees in the file, dotted, so the
// error message points at the line to look at. The result is sorted, so the
// same pair of configurations always produces the same message (Tenet 6).
//
// A nil or identical pair yields nothing, which is the ordinary case: most
// reloads only add or remove a target.
func NonReloadableChanges(running, next *Config) []string {
	if running == nil || next == nil {
		return nil
	}

	var changed []string

	runVal, nextVal := reflect.ValueOf(*running), reflect.ValueOf(*next)
	typ := runVal.Type()

	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}

		name := yamlName(field)
		if name == "" || name == "-" {
			continue
		}

		if _, ok := reloadableKeys[name]; ok {
			continue
		}

		a, b := runVal.Field(i), nextVal.Field(i)

		// A section is descended into one level, so the message names the
		// setting rather than the whole block. Anything deeper is reported as
		// the field that contains it, which is still the line to edit.
		if field.Type.Kind() == reflect.Struct {
			changed = append(changed, structChanges(name, a, b)...)

			continue
		}

		if !reflect.DeepEqual(a.Interface(), b.Interface()) {
			changed = append(changed, name)
		}
	}

	sort.Strings(changed)

	return changed
}

func structChanges(prefix string, running, next reflect.Value) []string {
	var changed []string

	typ := running.Type()

	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}

		name := yamlName(field)
		if name == "" || name == "-" {
			continue
		}

		key := prefix + "." + name
		if _, ok := reloadableKeys[key]; ok {
			continue
		}

		if !reflect.DeepEqual(running.Field(i).Interface(), next.Field(i).Interface()) {
			changed = append(changed, key)
		}
	}

	return changed
}

// yamlName reads the field's name in the configuration file from its yaml
// tag, so the error message quotes what the operator wrote rather than a Go
// identifier.
func yamlName(field reflect.StructField) string {
	tag, ok := field.Tag.Lookup("yaml")
	if !ok {
		return ""
	}

	name, _, _ := strings.Cut(tag, ",")

	return name
}
