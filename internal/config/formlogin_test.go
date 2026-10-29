package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/secret"
)

func TestFormLoginResolvesEveryFieldAsSecret(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "shop-password")
	if err := os.WriteFile(path, []byte("open-sesame\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := parse(t, `
targets:
  - name: shop
    url: https://shop.example.com/
    formLogin:
      url: https://shop.example.com/password
      fields:
        form_type: storefront_password
        password: "${file:`+path+`}"
`)

	var reg secret.Registry

	resolved, err := cfg.ResolveTargets(&reg)
	if err != nil {
		t.Fatal(err)
	}

	login := resolved[0].FormLogin
	if login == nil || login.URL != "https://shop.example.com/password" {
		t.Fatalf("formLogin = %+v", login)
	}

	if got := login.Fields["password"].Reveal(); got != "open-sesame" {
		t.Errorf("password = %q", got)
	}

	if got := login.Fields["form_type"].Reveal(); got != "storefront_password" {
		t.Errorf("form_type = %q", got)
	}

	if strings.Contains(reg.Scrub("rejected open-sesame"), "open-sesame") {
		t.Error("form field was not registered for scrubbing")
	}
}

func TestFormLoginMissingSecretFailsAtLoad(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
targets:
  - name: shop
    url: https://shop.example.com/
    formLogin:
      url: https://shop.example.com/password
      fields:
        password: "${env:WSAW_DEFINITELY_UNSET}"
`)

	if _, err := cfg.ResolveTargets(nil); err == nil || !strings.Contains(err.Error(), "formLogin.fields.password") {
		t.Fatalf("err = %v, want one naming formLogin.fields.password", err)
	}
}

// TestFormLoginIsNotSentToTypedURLs: a typed address must not be handed the
// operator's login, any more than their basic auth (Story 5.27, AC4).
func TestFormLoginIsNotSentToTypedURLs(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
targets:
  - name: shop
    url: https://shop.example.com/
    formLogin:
      url: https://shop.example.com/password
      fields: {password: hunter2}
`)

	if r := cfg.ResolveAdHocTarget("typed", "https://shop.example.com/", "none"); r.FormLogin != nil {
		t.Errorf("ad-hoc target carries formLogin %+v", r.FormLogin)
	}
}

func TestFormLoginValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, body, want string
	}{
		{
			name: "in defaults",
			body: `
defaults:
  formLogin:
    url: https://shop.example.com/password
    fields: {password: x}
targets:
  - name: shop
    url: https://shop.example.com/
`,
			want: "defaults.formLogin",
		},
		{
			name: "relative url",
			body: `
targets:
  - name: shop
    url: https://shop.example.com/
    formLogin:
      url: /password
      fields: {password: x}
`,
			want: "absolute http or https",
		},
		{
			name: "credentials in url",
			body: `
targets:
  - name: shop
    url: https://shop.example.com/
    formLogin:
      url: https://u:p@shop.example.com/password
      fields: {password: x}
`,
			want: "embedded credentials",
		},
		{
			name: "no fields",
			body: `
targets:
  - name: shop
    url: https://shop.example.com/
    formLogin:
      url: https://shop.example.com/password
`,
			want: "formLogin.fields",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := parseErr(t, tc.body); !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
