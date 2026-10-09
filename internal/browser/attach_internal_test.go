package browser

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDebuggerURL(t *testing.T) {
	t.Parallel()

	const wsURL = "ws://127.0.0.1:9222/devtools/browser/abc"

	cdp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write([]byte(`{"Browser":"Chrome/154","webSocketDebuggerUrl":"` + wsURL + `"}`))
	}))
	t.Cleanup(cdp.Close)

	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Browser":"Chrome/154"}`))
	}))
	t.Cleanup(silent.Close)

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)

	tests := []struct {
		name     string
		endpoint string
		want     string
		wantErr  string
	}{
		{name: "http endpoint asks the browser", endpoint: cdp.URL, want: wsURL},
		{name: "ws endpoint without a path asks the browser", endpoint: strings.Replace(cdp.URL, "http", "ws", 1) + "/", want: wsURL},
		{name: "full websocket address is kept", endpoint: wsURL, want: wsURL},
		{name: "localhost is kept", endpoint: "ws://localhost:9222/devtools/browser/abc", want: "ws://localhost:9222/devtools/browser/abc"},
		{name: "answer without an address", endpoint: silent.URL, wantErr: "named no websocket address"},
		{name: "failing endpoint", endpoint: broken.URL, wantErr: "500"},
		{name: "no port", endpoint: "http://127.0.0.1/", wantErr: "port"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := debuggerURL(t.Context(), tt.endpoint)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("debuggerURL(%q) error = %v, want one containing %q", tt.endpoint, err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("debuggerURL(%q): %v", tt.endpoint, err)
			}

			if got != tt.want {
				t.Errorf("debuggerURL(%q) = %q, want %q", tt.endpoint, got, tt.want)
			}
		})
	}
}
