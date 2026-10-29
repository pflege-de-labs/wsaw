package report_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/report"
)

// longURL is the shape that filled half a page: a login callback with a
// token in its query string.
var longURL = "https://shop.example/services/login_with_shop/buyer/callback?token=" + strings.Repeat("TUVkRnlSL2FsUCt", 130)

func markdownOf(t *testing.T, res *model.Result, rep *diff.Report) string {
	t.Helper()

	var md strings.Builder
	if err := report.WriteMarkdown(&md, res, rep, report.BasisUnknown); err != nil {
		t.Fatalf("WriteMarkdown: %v", err)
	}

	return md.String()
}

// Short values never wrap; long ones get the width (Story 5.39, AC10).
func TestPDFColumnsAreCompactOrWide(t *testing.T) {
	t.Parallel()

	html := printPDF(t, fixture(), nil, report.PDFOptions{}).html

	for _, want := range []string{
		`<th class="wide">Domain</th>`, `<th class="compact">Party</th>`, `<th class="compact num">Requests</th>`,
		`<th class="compact num">Bytes</th>`, `<th class="wide">Types</th>`,
		`<th class="wide">Name</th>`, `<th class="compact">SameSite</th>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the PDF lacks %s", want)
		}
	}

	if strings.Contains(html, "<th>") {
		t.Error("a column header has no class, so nothing stops it breaking inside a word")
	}
}

// Detail and subject share one wide cell, detail first (AC11), while the
// Markdown keeps its four columns.
func TestPDFStacksChangeDetailOverSubject(t *testing.T) {
	t.Parallel()

	rep := &diff.Report{Comparable: true, BaselineScanID: "scan-old", Changes: []diff.Change{
		{Severity: diff.SeverityLow, Type: diff.StorageAdded, Subject: "https://shop.example|session:key", Detail: "new sessionStorage key"},
	}}

	html := printPDF(t, fixture(), rep, report.PDFOptions{}).html

	for _, want := range []string{
		`<th class="compact">Severity</th><th class="compact">Change</th><th class="wide">Detail / Subject</th></tr>`,
		`<td class="wide">new sessionStorage key<br><code>https://<wbr>shop.<wbr>example|<wbr>session:<wbr>key</code></td>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the PDF lacks %s", want)
		}
	}

	if md := markdownOf(t, fixture(), rep); !strings.Contains(md, "| Severity | Change | Subject | Detail |") {
		t.Error("the Markdown changes table lost its columns")
	}
}

// A long value is shortened on paper, visibly, and only there (AC12).
func TestPDFShortensALongValueVisibly(t *testing.T) {
	t.Parallel()

	rep := &diff.Report{Comparable: true, BaselineScanID: "scan-old", Changes: []diff.Change{
		{Severity: diff.SeverityInfo, Type: diff.AssetAdded, Subject: longURL, Detail: "new document asset"},
	}}

	html := printPDF(t, fixture(), rep, report.PDFOptions{}).html

	if strings.Contains(html, longURL[150:250]) {
		t.Error("the PDF prints the long URL whole")
	}

	note := regexp.MustCompile(`…[^<]*(<wbr>[^<]*)*</code><span class="clipped"> \((\d+) characters; full value in the result document\)</span>`)

	if m := note.FindStringSubmatch(html); m == nil || m[2] != "2017" {
		t.Errorf("the shortened URL does not say it was shortened, or how long it is: %v", m)
	}

	if !strings.Contains(markdownOf(t, fixture(), rep), longURL) {
		t.Error("the Markdown report shortened the URL; only the PDF may")
	}
}

// What a reader has to type back is never cut, however long (AC12).
func TestPDFNeverShortensTheScanIDOrDigest(t *testing.T) {
	t.Parallel()

	res := fixture()
	res.ScanID = "scan-" + strings.Repeat("a", 250)
	// Nor is wsaw's own prose, such as why the scan failed.
	res.Termination, res.Error = model.TermError, "navigation failed: "+strings.Repeat("why ", 80)

	html := printPDF(t, res, nil, report.PDFOptions{JSONPath: "/api/v1/results/x?bodies=false&" + strings.Repeat("p", 250)}).html

	for _, want := range []string{res.ScanID, strings.Repeat("p", 250), res.Error} {
		if !strings.Contains(html, want) {
			t.Errorf("the PDF shortened %.20s…", want)
		}
	}
}

// Sizes read as sizes on paper and stay exact in the Markdown (AC13).
func TestPDFShowsBytesInHumanUnits(t *testing.T) {
	t.Parallel()

	res := fixture()
	res.Requests[0].TransferSize = 2271497

	html := printPDF(t, res, nil, report.PDFOptions{}).html
	if !strings.Contains(html, `<td class="compact num">2.2 MiB</td>`) {
		t.Error("the PDF does not show 2271497 bytes as 2.2 MiB")
	}

	if !strings.Contains(markdownOf(t, res, nil), "| 2271497 |") {
		t.Error("the Markdown report lost the exact byte count")
	}
}

// Identical rows appear once with a count; rows that only look identical
// say what tells them apart (AC14).
func TestReportMergesIdenticalRows(t *testing.T) {
	t.Parallel()

	entry := model.StorageEntry{
		Origin: "https://shop.example", Area: model.StorageSession, Key: "forms_session_id",
		Party: model.FirstParty, ValueSHA256: strings.Repeat("a", 64), ValueLength: 36,
	}
	other := entry
	other.ValueSHA256 = strings.Repeat("b", 64)

	res := fixture()
	res.Storage = []model.StorageEntry{entry, entry, other}
	res.Cookies = append(res.Cookies, res.Cookies[0])

	html := printPDF(t, res, nil, report.PDFOptions{}).html
	md := markdownOf(t, res, nil)

	for name, out := range map[string]string{"PDF": html, "Markdown": md} {
		if n := strings.Count(out, "forms_session_id"); n != 2 {
			t.Errorf("the %s lists the storage key %d times, want 2: once merged, once distinct", name, n)
		}

		for _, want := range []string{"×2 (value aaaaaaaa)", " (value bbbbbbbb)", res.Cookies[0].Name + "</code> ×2"} {
			if name == "Markdown" {
				want = strings.ReplaceAll(want, "</code>", "`")
			}

			if !strings.Contains(out, want) {
				t.Errorf("the %s lacks %q", name, want)
			}
		}
	}
}

// From the Hosts table on, the PDF is landscape; nothing before it is
// (AC15).
func TestPDFTurnsLandscapeAtTheHostsTable(t *testing.T) {
	t.Parallel()

	html := printPDF(t, fixture(), nil, report.PDFOptions{}).html

	if strings.Count(html, `<div class="landscape">`) != 1 ||
		!strings.Contains(html, `<div class="landscape"><h2>Hosts</h2>`) {
		t.Error("the landscape pages do not start at the Hosts table")
	}

	if !strings.Contains(html, "@page landscape { size: A4 landscape; }") {
		t.Error("the stylesheet names no landscape page")
	}
}

// The scan ID links to the scan's page when there is one (AC16).
func TestPDFLinksTheScanIDToItsPage(t *testing.T) {
	t.Parallel()

	res := fixture()
	res.Target = "shop/a?b#c"

	page := report.ResultURL("https://wsaw.example.com/", res)
	if want := "https://wsaw.example.com/results/shop%2Fa%3Fb%23c/reject/scan-abc"; page != want {
		t.Fatalf("ResultURL = %q, want %q", page, want)
	}

	html := printPDF(t, res, nil, report.PDFOptions{ResultURL: page}).html

	for _, want := range []string{
		`<a href="https://wsaw.example.com/results/shop%2Fa%3Fb%23c/reject/scan-abc"><code>scan-abc</code></a>`,
		`— <a href="https://wsaw.example.com/results/shop%2Fa%3Fb%23c/reject/scan-abc">https://wsaw.example.com/results/shop%2Fa%3Fb%23c/reject/scan-abc</a>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the first page lacks %s", want)
		}
	}

	if report.ResultURL("", res) != "" {
		t.Error("ResultURL invented an address with no base configured")
	}

	if html := printPDF(t, res, nil, report.PDFOptions{}).html; strings.Contains(html, "<a ") {
		t.Error("the PDF links somewhere without a configured base URL")
	}
}
