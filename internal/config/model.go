package config

import (
	"net"
	"net/url"
	"strings"

	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/textcheck"
)

// ModelAPI is the one optional external service wsaw may use besides Chrome
// (AGENTS.md §2, Story 4.16, D2): a server speaking the Anthropic Messages
// API that reads a watched text against a target's prompt. Nothing calls it
// unless a target has a text.check.
type ModelAPI struct {
	// API is how requests travel: anthropic (default), the Messages API and
	// every server compatible with it, or mistral, Mistral AI's own API.
	API string `yaml:"api,omitempty"`
	// BaseURL is the server, https://api.anthropic.com by default, or
	// https://api.mistral.ai for the mistral API. A local
	// server such as Ollama keeps the page text inside the operator's
	// network (AC10).
	BaseURL string `yaml:"baseURL,omitempty"`
	// Auth is how the key is sent: x-api-key (default) or bearer.
	Auth string `yaml:"auth,omitempty"`
	// APIKey is a secret reference, ${env:NAME} or ${file:/path}. Empty
	// sends no key, for a local server.
	APIKey string `yaml:"apiKey,omitempty"`
	// Model is the model ID; required once a target has a check.
	Model string `yaml:"model,omitempty"`
	// Timeout bounds one check, retries included, and is part of the scan's
	// own time: the scan's result waits for it (D1).
	Timeout Duration `yaml:"timeout,omitempty"`
	// Retries is how often a failed request is repeated.
	Retries *int `yaml:"retries,omitempty"`
	// MaxOutputTokens is the reply's max_tokens.
	MaxOutputTokens int `yaml:"maxOutputTokens,omitempty"`
	// DailyRequests caps requests across all targets per UTC day.
	DailyRequests int `yaml:"dailyRequests,omitempty"`
}

// TextCheckSettings resolves the model section into client settings. The key
// is resolved here, once at load, and registered for redaction.
func (c *Config) TextCheckSettings(reg *secret.Registry) (textcheck.Settings, error) {
	m := c.Model

	key, err := resolveSecret(m.APIKey, "model.apiKey", reg)
	if err != nil {
		return textcheck.Settings{}, err
	}

	return textcheck.Settings{
		API:             m.API,
		BaseURL:         m.BaseURL,
		Auth:            m.Auth,
		Key:             key.Reveal(),
		Model:           m.Model,
		Timeout:         m.Timeout.Or(textcheck.DefaultTimeout),
		Retries:         retries(m.Retries),
		MaxOutputTokens: m.MaxOutputTokens,
		DailyRequests:   m.DailyRequests,
	}, nil
}

// UsesModel reports whether any enabled target has a text check, which is
// the only reason to build a model client.
func (c *Config) UsesModel() bool {
	for i := range c.Targets {
		if t := &c.Targets[i]; !t.Disabled && t.Text != nil && t.Text.Check != nil {
			return true
		}
	}

	return false
}

// retries is the configured count, or the default when none is set; an
// explicit zero means never retry.
func retries(p *int) int {
	if p == nil {
		return textcheck.DefaultRetries
	}

	return *p
}

// validateModel refuses settings that would send the key or the page text
// somewhere the operator did not mean (Story 4.16, AC10; Tenet 15).
func (c *Config) validateModel(add addFunc) {
	m := c.Model

	if m.BaseURL != "" {
		if msg := baseURLProblem(m.BaseURL); msg != "" {
			add(0, "model.baseURL", "%s", msg)
		}
	}

	if m.Auth != "" && m.Auth != textcheck.AuthAPIKey && m.Auth != textcheck.AuthBearer {
		add(0, "model.auth", "must be %s or %s", textcheck.AuthAPIKey, textcheck.AuthBearer)
	}

	switch m.API {
	case "", textcheck.APIMessages:
	case textcheck.APIMistral:
		if m.Auth == textcheck.AuthAPIKey {
			add(0, "model.auth", "the mistral API takes the key only as a bearer token; leave auth unset")
		}
	default:
		add(0, "model.api", "must be %s or %s", textcheck.APIMessages, textcheck.APIMistral)
	}

	if m.APIKey != "" && !secret.IsReference(m.APIKey) {
		add(0, "model.apiKey", "must be a reference such as ${env:ANTHROPIC_API_KEY} or ${file:/path}, not the key itself")
	}

	if m.Timeout < 0 || m.MaxOutputTokens < 0 || m.DailyRequests < 0 || m.Retries != nil && *m.Retries < 0 {
		add(0, "model", "timeout, retries, maxOutputTokens and dailyRequests must not be negative")
	}

	if c.UsesModel() && strings.TrimSpace(m.Model) == "" {
		add(0, "model.model", "is required when a target has a text.check; name the model ID, for example claude-opus-5-5")
	}
}

// baseURLProblem allows https anywhere and plain http only to this host: the
// key and the page text must not cross a network unencrypted.
func baseURLProblem(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "is not an absolute URL"
	}

	switch u.Scheme {
	case "https":
		return ""
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
			return ""
		}

		return "uses http to a host other than this one; the key and the page text would cross the network unencrypted, use https"
	default:
		return "must be an http or https URL"
	}
}
