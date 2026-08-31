package logging_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/martint17r/wsaw/internal/logging"
	"github.com/martint17r/wsaw/internal/secret"
)

const plaintext = "hunter2-super-secret"

func TestJSONFormat(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	log, err := logging.New(logging.Options{Output: &b, Format: "json"})
	if err != nil {
		t.Fatal(err)
	}

	log.Info("scan finished", "target", "site", "consent_mode", "reject")

	out := b.String()
	if !strings.HasPrefix(out, "{") {
		t.Errorf("output is not JSON: %s", out)
	}

	for _, want := range []string{`"msg":"scan finished"`, `"target":"site"`, `"consent_mode":"reject"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
}

func TestLevelFiltering(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	log, err := logging.New(logging.Options{Output: &b, Level: "warn"})
	if err != nil {
		t.Fatal(err)
	}

	log.Debug("noise")
	log.Info("also noise")
	log.Warn("signal")

	out := b.String()

	if strings.Contains(out, "noise") {
		t.Error("lines below the configured level were emitted")
	}

	if !strings.Contains(out, "signal") {
		t.Error("a line at the configured level was dropped")
	}
}

func TestInvalidLevelAndFormatAreRejected(t *testing.T) {
	t.Parallel()

	if _, err := logging.New(logging.Options{Level: "verbose"}); err == nil {
		t.Error("an unknown level was accepted")
	}

	if _, err := logging.New(logging.Options{Format: "xml"}); err == nil {
		t.Error("an unknown format was accepted")
	}
}

// TestSecretsAreScrubbedFromErrorText is the case a typed secret cannot
// cover: a credential embedded in a third-party library's error string.
func TestSecretsAreScrubbedFromErrorText(t *testing.T) {
	t.Parallel()

	var reg secret.Registry

	reg.Add(secret.Literal(plaintext))

	var b strings.Builder

	log, err := logging.New(logging.Options{Output: &b, Secrets: &reg})
	if err != nil {
		t.Fatal(err)
	}

	log.Error("request failed", "error", errors.New("dial https://user:"+plaintext+"@proxy: refused"))

	if strings.Contains(b.String(), plaintext) {
		t.Errorf("secret leaked through an error attribute: %s", b.String())
	}

	if !strings.Contains(b.String(), secret.Redacted) {
		t.Errorf("no redaction marker in output: %s", b.String())
	}
}

func TestSecretsAreScrubbedFromMessageAndStringAttrs(t *testing.T) {
	t.Parallel()

	var reg secret.Registry

	reg.Add(secret.Literal(plaintext))

	var b strings.Builder

	log, err := logging.New(logging.Options{Output: &b, Secrets: &reg})
	if err != nil {
		t.Fatal(err)
	}

	log.Info("token is "+plaintext, "url", "https://hook/"+plaintext)

	if strings.Contains(b.String(), plaintext) {
		t.Errorf("secret leaked: %s", b.String())
	}
}

// TestSecretsAreScrubbedFromInheritedAttributes covers the common pattern of
// a scan-scoped logger built with With().
func TestSecretsAreScrubbedFromInheritedAttributes(t *testing.T) {
	t.Parallel()

	var reg secret.Registry

	reg.Add(secret.Literal(plaintext))

	var b strings.Builder

	log, err := logging.New(logging.Options{Output: &b, Secrets: &reg})
	if err != nil {
		t.Fatal(err)
	}

	scoped := log.With("proxy", "https://user:"+plaintext+"@proxy")
	scoped.Info("scan started")

	if strings.Contains(b.String(), plaintext) {
		t.Errorf("secret leaked through an inherited attribute: %s", b.String())
	}
}

func TestScrubbingIsOptional(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	log, err := logging.New(logging.Options{Output: &b})
	if err != nil {
		t.Fatal(err)
	}

	log.Info("plain message", "k", "v")

	if !strings.Contains(b.String(), "plain message") {
		t.Error("logging without a secret registry lost the message")
	}
}
