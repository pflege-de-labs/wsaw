package capture

import "testing"

// originOf decides which origins get wiped between scans, so a URL it refuses
// to read is a leak that survives. Frame security origins arrive here too,
// already in origin form, and must come back unchanged.
func TestOriginOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"url with path and query", "https://www.example.com/de/home/?a=1", "https://www.example.com"},
		{"origin as given by a frame", "https://www.example.com", "https://www.example.com"},
		{"port is part of the origin", "http://site.test:8080/index.html", "http://site.test:8080"},
		{"http and https are different origins", "http://www.example.com/", "http://www.example.com"},
		{"surrounding space", "  https://example.com/  ", "https://example.com"},
		{"opaque origin", "null", ""},
		{"data url", "data:text/html,<p>hi</p>", ""},
		{"about:blank", "about:blank", ""},
		{"no scheme", "www.example.com/home", ""},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := originOf(tt.raw); got != tt.want {
				t.Errorf("originOf(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
