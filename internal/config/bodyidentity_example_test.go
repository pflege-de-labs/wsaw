package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
)

// The example's body identities are the ones an operator copies, so each is
// checked against a body of the shape it was written for: two fetches whose
// bytes differ while the declared version does not must yield one identity,
// and a new version must yield another. The bodies are synthetic excerpts of
// that shape, not stored copies of the vendors' scripts.
func TestTheShippedExampleBodyIdentitiesExtractTheVersion(t *testing.T) {
	t.Parallel()

	b, err := os.ReadFile(filepath.Clean(examplePath))
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Parse(b)
	if err != nil {
		t.Fatal(err)
	}

	rules, err := cfg.NormalizeRules()
	if err != nil {
		t.Fatal(err)
	}

	n, err := normalize.New(rules)
	if err != nil {
		t.Fatal(err)
	}

	gtag := func(version, flags string) string {
		return "// Copyright 2012 Google Inc.\n(function(){\nvar data = {\n\"resource\": {\n  \"version\":\"" +
			version + "\",\n  \"macros\":[{\"function\":\"__c\",\"vtp_value\":" + flags + "}]}}})();"
	}

	cmp := func(version, region string) string {
		return `window.cmp_config_data={"intID":4770,"usr_cc":"` + region + `","intVersion":` + version +
			`,"intDesignVersion":102};`
	}

	tests := []struct {
		name, url, label   string
		same, also, bumped string
	}{
		{
			"gtag loader", "https://www.googletagmanager.com/gtag/js?id=G-1&cx=c&gtm=4e69t1", "gtag version",
			gtag("10", "true"), gtag("10", "false"), gtag("11", "true"),
		},
		{
			"consentmanager cmp.php", "https://c.delivery.consentmanager.net/delivery/cmp.php?cdid=abc&l=de",
			"consentmanager settings version",
			cmp("97", "DE"), cmp("97", "AT"), cmp("98", "DE"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			label, a := n.BodyIdentity(tc.url, tc.same)
			_, b := n.BodyIdentity(tc.url, tc.also)
			_, c := n.BodyIdentity(tc.url, tc.bumped)

			switch {
			case label != tc.label:
				t.Errorf("label = %q, want %q", label, tc.label)
			case a == "" || a != b:
				t.Errorf("identities %q and %q; bytes that moved without the version must compare alike", a, b)
			case c == a:
				t.Errorf("identity %q unchanged after the version moved; a publish would go unreported", c)
			}
		})
	}
}
