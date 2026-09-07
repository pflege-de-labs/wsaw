package report

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// HAR 1.2 export, so results open in Chrome DevTools and the HAR viewers
// people already use (Story 5.2).
//
// wsaw does not retain request or response headers — they can carry
// credentials and identifiers — so the exported HAR carries empty header
// lists rather than invented ones. That is a real limitation and is stated in
// the HAR comment field rather than papered over.

type har struct {
	Log harLog `json:"log"`
}

type harLog struct {
	Version string     `json:"version"`
	Creator harCreator `json:"creator"`
	Pages   []harPage  `json:"pages"`
	Entries []harEntry `json:"entries"`
	Comment string     `json:"comment,omitempty"`
}

type harCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type harPage struct {
	StartedDateTime string        `json:"startedDateTime"`
	ID              string        `json:"id"`
	Title           string        `json:"title"`
	PageTimings     harPageTiming `json:"pageTimings"`
}

type harPageTiming struct {
	OnContentLoad float64 `json:"onContentLoad"`
	OnLoad        float64 `json:"onLoad"`
}

type harEntry struct {
	Pageref         string      `json:"pageref"`
	StartedDateTime string      `json:"startedDateTime"`
	Time            float64     `json:"time"`
	Request         harRequest  `json:"request"`
	Response        harResponse `json:"response"`
	Cache           struct{}    `json:"cache"`
	Timings         harTimings  `json:"timings"`
	ServerIPAddress string      `json:"serverIPAddress,omitempty"`
	Comment         string      `json:"comment,omitempty"`
}

type harRequest struct {
	Method      string     `json:"method"`
	URL         string     `json:"url"`
	HTTPVersion string     `json:"httpVersion"`
	Cookies     []struct{} `json:"cookies"`
	Headers     []struct{} `json:"headers"`
	QueryString []struct{} `json:"queryString"`
	HeadersSize int        `json:"headersSize"`
	BodySize    int        `json:"bodySize"`
}

type harResponse struct {
	Status      int        `json:"status"`
	StatusText  string     `json:"statusText"`
	HTTPVersion string     `json:"httpVersion"`
	Cookies     []struct{} `json:"cookies"`
	Headers     []struct{} `json:"headers"`
	Content     harContent `json:"content"`
	RedirectURL string     `json:"redirectURL"`
	HeadersSize int        `json:"headersSize"`
	BodySize    int64      `json:"bodySize"`
}

type harContent struct {
	Size     int64  `json:"size"`
	MimeType string `json:"mimeType"`
	Comment  string `json:"comment,omitempty"`
}

type harTimings struct {
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
}

// WriteHAR exports a result as HAR 1.2.
func WriteHAR(w io.Writer, res *model.Result) error {
	const pageID = "page_1"

	doc := har{
		Log: harLog{
			Version: "1.2",
			Creator: harCreator{Name: "wsaw", Version: orNone(res.Environment.WsawVersion)},
			Pages: []harPage{{
				StartedDateTime: res.StartedAt.UTC().Format(time.RFC3339Nano),
				ID:              pageID,
				Title:           res.URL,
				PageTimings:     harPageTiming{OnContentLoad: -1, OnLoad: float64(res.Duration.Milliseconds())},
			}},
			Comment: "Exported by wsaw. Request and response headers are not retained, " +
				"because they can carry credentials and identifiers; header lists are therefore empty.",
		},
	}

	doc.Log.Entries = make([]harEntry, 0, len(res.Requests))

	for i := range res.Requests {
		req := &res.Requests[i]
		if req.NonNetwork {
			continue
		}

		entry := harEntry{
			Pageref:         pageID,
			StartedDateTime: res.StartedAt.Add(req.Timing.StartOffset).UTC().Format(time.RFC3339Nano),
			Time:            durationMillis(req.Timing.EndOffset - req.Timing.StartOffset),
			ServerIPAddress: req.RemoteIP,
			Request: harRequest{
				Method:      orDefault(req.Method, "GET"),
				URL:         req.URL,
				HTTPVersion: orDefault(req.Protocol, "HTTP/1.1"),
				Cookies:     []struct{}{},
				Headers:     []struct{}{},
				QueryString: []struct{}{},
				HeadersSize: -1,
				BodySize:    -1,
			},
			Response: harResponse{
				Status:      req.Status,
				StatusText:  req.StatusText,
				HTTPVersion: orDefault(req.Protocol, "HTTP/1.1"),
				Cookies:     []struct{}{},
				Headers:     []struct{}{},
				RedirectURL: req.RedirectTo,
				HeadersSize: -1,
				BodySize:    req.TransferSize,
				Content: harContent{
					Size:     req.DecodedSize,
					MimeType: orDefault(req.MimeType, "application/octet-stream"),
				},
			},
			Timings: harTimings{
				Send:    0,
				Wait:    durationMillis(req.Timing.TTFB),
				Receive: durationMillis(req.Timing.EndOffset - req.Timing.StartOffset - req.Timing.TTFB),
			},
		}

		// The consent phase has no HAR equivalent, but it is the most
		// important attribute wsaw records, so it goes in the comment.
		entry.Comment = fmt.Sprintf("wsaw: party=%s phase=%s", req.Party, req.Phase)

		if req.Failed {
			entry.Comment += " failed=" + req.FailureReason
		}

		if req.BodySHA256 != "" {
			entry.Response.Content.Comment = "sha256=" + req.BodySHA256
		}

		doc.Log.Entries = append(doc.Log.Entries, entry)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("writing HAR: %w", err)
	}

	return nil
}

// durationMillis renders a duration in milliseconds, using HAR's -1 for
// "unknown" rather than reporting a fabricated zero.
func durationMillis(d time.Duration) float64 {
	if d <= 0 {
		return -1
	}

	return float64(d) / float64(time.Millisecond)
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}

	return s
}
