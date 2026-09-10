package capture

import "testing"

func TestParseDataURIHeader(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		raw        string
		wantMime   string
		wantPay    string
		wantBase64 bool
		wantOK     bool
	}{
		{"base64 image", "data:image/png;base64,QUJD", "image/png", "QUJD", true, true},
		{"plain text", "data:text/javascript,void 0", "text/javascript", "void 0", false, true},
		{"no media type", "data:,hello", "", "hello", false, true},
		{"no media type base64", "data:;base64,QUJD", "", "QUJD", true, true},
		{"not a data URI", "https://example.com/a.png", "", "", false, false},
		{"missing comma", "data:image/png;base64", "", "", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mime, payload, isBase64, ok := parseDataURIHeader(tc.raw)

			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}

			if !ok {
				return
			}

			if mime != tc.wantMime {
				t.Errorf("mime = %q, want %q", mime, tc.wantMime)
			}

			if payload != tc.wantPay {
				t.Errorf("payload = %q, want %q", payload, tc.wantPay)
			}

			if isBase64 != tc.wantBase64 {
				t.Errorf("isBase64 = %v, want %v", isBase64, tc.wantBase64)
			}
		})
	}
}

func TestDataURIUpperBound(t *testing.T) {
	t.Parallel()

	// "QUJD" decodes to "ABC" (3 bytes), no padding.
	if got := dataURIUpperBound("QUJD", true); got != 3 {
		t.Errorf("base64 upper bound = %d, want 3", got)
	}

	// "QQ==" decodes to "A" (1 byte), two padding characters.
	if got := dataURIUpperBound("QQ==", true); got != 1 {
		t.Errorf("padded base64 upper bound = %d, want 1", got)
	}

	if got := dataURIUpperBound("hello", false); got != 5 {
		t.Errorf("plain upper bound = %d, want 5", got)
	}
}

func TestDecodeDataURIPayload(t *testing.T) {
	t.Parallel()

	data, err := decodeDataURIPayload("QUJD", true)
	if err != nil || string(data) != "ABC" {
		t.Errorf("base64 decode = %q, %v, want %q, nil", data, err, "ABC")
	}

	// Unpadded base64 ("QQ", not "QQ=="), which some pages emit and which
	// StdEncoding rejects outright.
	data, err = decodeDataURIPayload("QQ", true)
	if err != nil || string(data) != "A" {
		t.Errorf("unpadded base64 decode = %q, %v, want %q, nil", data, err, "A")
	}

	data, err = decodeDataURIPayload("void%200", false)
	if err != nil || string(data) != "void 0" {
		t.Errorf("percent decode = %q, %v, want %q, nil", data, err, "void 0")
	}

	if _, err := decodeDataURIPayload("%zz", false); err == nil {
		t.Error("decodeDataURIPayload accepted an invalid percent escape")
	}
}

func TestTruncatedDataURI(t *testing.T) {
	t.Parallel()

	if got, want := truncatedDataURI("image/png", true, 42, true), "data:image/png;base64,<truncated: 42 bytes>"; got != want {
		t.Errorf("truncatedDataURI = %q, want %q", got, want)
	}

	if got, want := truncatedDataURI("text/plain", false, 42, true), "data:text/plain,<truncated: 42 bytes>"; got != want {
		t.Errorf("truncatedDataURI = %q, want %q", got, want)
	}

	if got, want := truncatedDataURI("image/png", true, 1000, false), "data:image/png;base64,<truncated: over 1000 bytes, not decoded>"; got != want {
		t.Errorf("truncatedDataURI = %q, want %q", got, want)
	}
}
