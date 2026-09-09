//go:build ignore

// Command coverreport turns a Go coverage profile into a single self-contained
// HTML page: every package ranked, then broken down file by file and function
// by function.
//
// It is tagged `ignore` on purpose. This is developer tooling, not part of the
// shipped binary, and keeping it out of ./... means the tool never appears in
// the coverage numbers it reports on. Run it by naming the file:
//
//	go run tools/coverreport/main.go -profile coverage.out -out coverage-report.html
//
// `make cover-report` does that for you.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

func main() {
	profile := flag.String("profile", "coverage.out", "coverage profile to read")
	out := flag.String("out", "coverage-report.html", "HTML file to write")
	title := flag.String("title", "", "page title (default: derived from the module name)")
	flag.Parse()

	if err := run(*profile, *out, *title); err != nil {
		fmt.Fprintf(os.Stderr, "coverreport: %v\n", err)
		os.Exit(1)
	}
}

func run(profile, out, title string) error {
	files, err := readProfile(profile)
	if err != nil {
		return err
	}

	funcs, err := readFuncCoverage(profile)
	if err != nil {
		return err
	}

	metas, module, err := listPackages()
	if err != nil {
		return err
	}

	report, err := assemble(metas, module, files, funcs, title)
	if err != nil {
		return err
	}

	return render(out, report)
}

// ----------------------------------------------------------------- input

// counts is the statement tally for one file: how many statements the compiler
// instrumented, and how many of them a test executed at least once.
type counts struct {
	Stmts   int
	Covered int
}

// readProfile sums the coverage profile per file. A block counts as covered
// when any run touched it, which is how `go tool cover` totals it too.
func readProfile(path string) (map[string]counts, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening profile: %w", err)
	}
	defer f.Close()

	files := make(map[string]counts)
	sc := bufio.NewScanner(f)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}

		name, c, err := parseProfileLine(line)
		if err != nil {
			return nil, err
		}

		acc := files[name]
		acc.Stmts += c.Stmts
		acc.Covered += c.Covered
		files[name] = acc
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading profile: %w", err)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("profile %s contains no coverage blocks", path)
	}

	return files, nil
}

func parseProfileLine(line string) (string, counts, error) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return "", counts{}, fmt.Errorf("malformed profile line %q", line)
	}

	name, _, ok := strings.Cut(fields[0], ":")
	if !ok {
		return "", counts{}, fmt.Errorf("malformed profile position %q", fields[0])
	}

	stmts, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", counts{}, fmt.Errorf("statement count in %q: %w", line, err)
	}

	hits, err := strconv.Atoi(fields[2])
	if err != nil {
		return "", counts{}, fmt.Errorf("hit count in %q: %w", line, err)
	}

	c := counts{Stmts: stmts}
	if hits > 0 {
		c.Covered = stmts
	}

	return name, c, nil
}

// Fn is one function's coverage, as `go tool cover -func` reports it. File is
// kept alongside so a package-level list can still name the file.
type Fn struct {
	File string
	Name string
	Line int
	Pct  float64
}

// readFuncCoverage shells out to `go tool cover -func`, which already knows how
// to attribute profile blocks to function declarations. Re-deriving that from
// the AST here would be a second, divergent implementation of the same thing.
func readFuncCoverage(profile string) (map[string][]Fn, error) {
	cmd := exec.Command("go", "tool", "cover", "-func="+profile)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go tool cover -func: %w", err)
	}

	funcs := make(map[string][]Fn)

	for line := range strings.SplitSeq(string(stdout), "\n") {
		if line == "" || strings.HasPrefix(line, "total:") {
			continue
		}

		fn, file, ok := parseFuncLine(line)
		if !ok {
			continue
		}

		funcs[file] = append(funcs[file], fn)
	}

	return funcs, nil
}

// parseFuncLine reads one `file.go:12:\tName\t80.0%` row. Anything unparseable
// is skipped rather than fatal: the per-function list is detail, and losing a
// row must not cost the whole report.
func parseFuncLine(line string) (Fn, string, bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return Fn{}, "", false
	}

	file, rest, ok := strings.Cut(fields[0], ":")
	if !ok {
		return Fn{}, "", false
	}

	lineNo, err := strconv.Atoi(strings.TrimSuffix(rest, ":"))
	if err != nil {
		return Fn{}, "", false
	}

	pct, err := strconv.ParseFloat(strings.TrimSuffix(fields[2], "%"), 64)
	if err != nil {
		return Fn{}, "", false
	}

	return Fn{Name: fields[1], Line: lineNo, Pct: pct}, file, true
}

// pkgMeta is the subset of `go list -json` this tool needs.
type pkgMeta struct {
	ImportPath  string
	Dir         string
	Module      struct{ Path string }
	GoFiles     []string
	TestGoFiles []string
	// XTestGoFiles are the _test package files. They test the same package, so
	// they belong in the same test-file count.
	XTestGoFiles []string
}

func listPackages() ([]pkgMeta, string, error) {
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Stderr = os.Stderr

	stdout, err := cmd.Output()
	if err != nil {
		return nil, "", fmt.Errorf("go list: %w", err)
	}

	var (
		metas  []pkgMeta
		module string
		dec    = json.NewDecoder(strings.NewReader(string(stdout)))
	)

	for {
		var p pkgMeta

		if err := dec.Decode(&p); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, "", fmt.Errorf("decoding go list output: %w", err)
		}

		if module == "" {
			module = p.Module.Path
		}

		metas = append(metas, p)
	}

	return metas, module, nil
}

// ----------------------------------------------------------------- model

// File is one source file's row in a package card.
type File struct {
	Name    string
	Stmts   int
	Covered int
	Pct     float64
}

// Pkg is one package's place in the report.
type Pkg struct {
	Name      string // import path with the module prefix stripped
	Group     string // first path segment: internal, cmd, test …
	Stmts     int
	Covered   int
	Uncovered int
	Pct       float64
	Ratio     float64 // test lines per source line
	GoFiles   int
	TestFiles int
	SrcLOC    int
	TestLOC   int
	Files     []File
	Dead      []Fn // never executed
	Weak      []Fn // executed, but under 60%
}

// Band buckets a percentage so the page can encode it as a label as well as a
// colour step — never colour alone.
func (p Pkg) Band() string { return band(p.Pct, p.Stmts) }

// Band buckets a file the same way its package is bucketed.
func (f File) Band() string { return band(f.Pct, f.Stmts) }

func band(pct float64, stmts int) string {
	switch {
	case stmts == 0:
		return "none"
	case pct == 0:
		return "zero"
	case pct < 50:
		return "thin"
	case pct < 75:
		return "fair"
	case pct < 90:
		return "solid"
	default:
		return "strong"
	}
}

// Group is the set of packages sharing a top-level directory.
type Group struct {
	Name    string
	Note    string
	Pkgs    []Pkg
	Stmts   int
	Covered int
	Pct     float64
}

// Band buckets a whole group for its summary bar.
func (g Group) Band() string { return band(g.Pct, g.Stmts) }

// Band buckets the module total for the hero bar.
func (r Report) Band() string { return band(r.Pct, r.Stmts) }

// Report is everything the template renders.
type Report struct {
	Title     string
	Module    string
	Stmts     int
	Covered   int
	Uncovered int
	Pct       float64
	Packages  []Pkg // ranked best to worst
	Gaps      []Pkg // ranked by uncovered statements, worst first
	MaxGap    int
	Groups    []Group
	ZeroPkgs  int
	ZeroStmts int
	Strong    int
	Internal  int
	Commit    string
	GoVersion string
	Platform  string
	RunDate   string
	CSS       template.CSS
}

// groupNotes says what a top-level directory holds. An unlisted directory gets
// no note rather than a wrong one.
var groupNotes = map[string]string{
	"internal": "Application packages — everything the daemon, scanner and HTTP API are built from.",
	"cmd":      "The CLI entry point: flag wiring, subcommand dispatch, process startup.",
	"test":     "End-to-end harness: the fixture server under test and the browser driver.",
	"pkg":      "Packages importable by other modules.",
}

func assemble(metas []pkgMeta, module string, files map[string]counts, funcs map[string][]Fn, title string) (Report, error) {
	rep := Report{
		Title:     title,
		Module:    module,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		RunDate:   time.Now().Format("2006-01-02"),
		Commit:    gitCommit(),
		CSS:       template.CSS(pageCSS),
	}

	if rep.Title == "" {
		rep.Title = shortModule(module) + " Coverage Gutter"
	}

	for _, m := range metas {
		p, err := buildPkg(m, module+"/", files, funcs)
		if err != nil {
			return Report{}, err
		}

		rep.Packages = append(rep.Packages, p)
		rep.Stmts += p.Stmts
		rep.Covered += p.Covered
	}

	if rep.Stmts == 0 {
		return Report{}, errors.New("no instrumented statements found — did the profile come from this module?")
	}

	rep.Pct = percent(rep.Covered, rep.Stmts)
	rep.Uncovered = rep.Stmts - rep.Covered

	finish(&rep)

	return rep, nil
}

func buildPkg(m pkgMeta, prefix string, files map[string]counts, funcs map[string][]Fn) (Pkg, error) {
	p := Pkg{
		Name:      strings.TrimPrefix(m.ImportPath, prefix),
		GoFiles:   len(m.GoFiles),
		TestFiles: len(m.TestGoFiles) + len(m.XTestGoFiles),
	}
	p.Group, _, _ = strings.Cut(p.Name, "/")

	for name, c := range files {
		base, ok := fileInPackage(name, m.ImportPath)
		if !ok {
			continue
		}

		p.Files = append(p.Files, File{
			Name:    base,
			Stmts:   c.Stmts,
			Covered: c.Covered,
			Pct:     percent(c.Covered, c.Stmts),
		})
		p.Stmts += c.Stmts
		p.Covered += c.Covered

		for _, fn := range funcs[name] {
			fn.File = base

			switch {
			case fn.Pct == 0:
				p.Dead = append(p.Dead, fn)
			case fn.Pct < 60:
				p.Weak = append(p.Weak, fn)
			}
		}
	}

	p.Pct = percent(p.Covered, p.Stmts)
	p.Uncovered = p.Stmts - p.Covered

	sortPkgDetail(&p)

	loc, err := countLOC(m.Dir)
	if err != nil {
		return Pkg{}, err
	}

	p.SrcLOC, p.TestLOC = loc[0], loc[1]
	if p.SrcLOC > 0 {
		p.Ratio = float64(p.TestLOC) / float64(p.SrcLOC)
	}

	return p, nil
}

// fileInPackage reports whether a profile entry belongs directly to importPath,
// and returns its base name. A nested package's file is not this package's.
func fileInPackage(profileName, importPath string) (string, bool) {
	base, ok := strings.CutPrefix(profileName, importPath+"/")
	if !ok || strings.Contains(base, "/") {
		return "", false
	}

	return base, true
}

// sortPkgDetail puts the weakest first everywhere inside a card, so the reason
// to open the card is the first thing visible in it.
func sortPkgDetail(p *Pkg) {
	sort.Slice(p.Files, func(i, j int) bool {
		if p.Files[i].Pct != p.Files[j].Pct {
			return p.Files[i].Pct < p.Files[j].Pct
		}

		return p.Files[i].Stmts > p.Files[j].Stmts
	})
	sort.Slice(p.Weak, func(i, j int) bool { return p.Weak[i].Pct < p.Weak[j].Pct })
	sort.Slice(p.Dead, func(i, j int) bool {
		if p.Dead[i].File != p.Dead[j].File {
			return p.Dead[i].File < p.Dead[j].File
		}

		return p.Dead[i].Line < p.Dead[j].Line
	})
}

// finish derives every ordering and roll-up the page needs, so the template
// itself stays free of logic.
func finish(rep *Report) {
	sort.Slice(rep.Packages, func(i, j int) bool {
		if rep.Packages[i].Pct != rep.Packages[j].Pct {
			return rep.Packages[i].Pct > rep.Packages[j].Pct
		}

		return rep.Packages[i].Stmts > rep.Packages[j].Stmts
	})

	byGroup := map[string]*Group{}
	order := []string{}

	for _, p := range rep.Packages {
		if p.Pct >= 90 {
			rep.Strong++
		}

		if p.Group == "internal" {
			rep.Internal++
		}

		if p.Stmts > 0 && p.Covered == 0 {
			rep.ZeroPkgs++
			rep.ZeroStmts += p.Stmts
		}

		if p.Uncovered > 0 {
			rep.Gaps = append(rep.Gaps, p)
		}

		g, ok := byGroup[p.Group]
		if !ok {
			g = &Group{Name: p.Group, Note: groupNotes[p.Group]}
			byGroup[p.Group] = g
			order = append(order, p.Group)
		}

		g.Pkgs = append(g.Pkgs, p)
		g.Stmts += p.Stmts
		g.Covered += p.Covered
	}

	sort.Slice(rep.Gaps, func(i, j int) bool { return rep.Gaps[i].Uncovered > rep.Gaps[j].Uncovered })

	if len(rep.Gaps) > 0 {
		rep.MaxGap = rep.Gaps[0].Uncovered
	}

	if len(rep.Gaps) > gapsShown {
		rep.Gaps = rep.Gaps[:gapsShown]
	}

	rep.Groups = orderGroups(byGroup, order)
}

// orderGroups runs application code before entry points before harness, then
// anything unrecognised alphabetically.
func orderGroups(byGroup map[string]*Group, order []string) []Group {
	rank := map[string]int{"internal": 0, "cmd": 1, "pkg": 2, "test": 3}

	sort.SliceStable(order, func(i, j int) bool {
		ri, oki := rank[order[i]]
		rj, okj := rank[order[j]]

		if oki != okj {
			return oki
		}

		if oki && ri != rj {
			return ri < rj
		}

		return order[i] < order[j]
	})

	groups := make([]Group, 0, len(order))

	for _, name := range order {
		g := byGroup[name]
		g.Pct = percent(g.Covered, g.Stmts)

		sort.Slice(g.Pkgs, func(i, j int) bool {
			if g.Pkgs[i].Pct != g.Pkgs[j].Pct {
				return g.Pkgs[i].Pct < g.Pkgs[j].Pct
			}

			return g.Pkgs[i].Stmts > g.Pkgs[j].Stmts
		})

		groups = append(groups, *g)
	}

	return groups
}

// countLOC returns [source lines, test lines] for the .go files directly in
// dir. It is a crude effort signal, deliberately: nothing here judges test
// quality, and the page says so.
func countLOC(dir string) ([2]int, error) {
	var loc [2]int

	entries, err := os.ReadDir(dir)
	if err != nil {
		return loc, fmt.Errorf("reading %s: %w", dir, err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}

		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return loc, fmt.Errorf("reading %s: %w", e.Name(), err)
		}

		n := strings.Count(string(b), "\n")
		if strings.HasSuffix(e.Name(), "_test.go") {
			loc[1] += n
		} else {
			loc[0] += n
		}
	}

	return loc, nil
}

// gitCommit labels the report with the tree it describes. A report is worth
// less without one, but not worthless, so a missing commit is not an error.
func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}

	return strings.TrimSpace(string(out))
}

func shortModule(module string) string {
	parts := strings.Split(module, "/")

	return parts[len(parts)-1]
}

func percent(covered, total int) float64 {
	if total == 0 {
		return 0
	}

	return 100 * float64(covered) / float64(total)
}

// ----------------------------------------------------------------- output

const (
	// gapsShown caps the "where the untested code is" list. Past ten entries
	// the list stops being a shortlist, and the full table is right below it.
	gapsShown = 10
	// deadShown and weakShown cap the per-package function lists for the same
	// reason. Both render a "+N more" line so nothing is silently dropped.
	deadShown = 24
	weakShown = 12
)

func render(out string, rep Report) error {
	t, err := template.New("report").Funcs(funcMap()).Parse(pageHTML)
	if err != nil {
		return fmt.Errorf("parsing template: %w", err)
	}

	f, err := os.Create(out)
	if err != nil {
		return fmt.Errorf("creating %s: %w", out, err)
	}
	defer f.Close()

	if err := t.Execute(f, rep); err != nil {
		return fmt.Errorf("rendering %s: %w", out, err)
	}

	fmt.Printf("coverreport: wrote %s — %.1f%% of %s statements across %d packages\n",
		out, rep.Pct, comma(rep.Stmts), len(rep.Packages))

	return nil
}

func funcMap() template.FuncMap {
	return template.FuncMap{
		"pct1":      func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) },
		"ratio":     func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) },
		"num":       comma,
		"share":     func(part, whole int) string { return strconv.Itoa(int(percent(part, whole) + 0.5)) },
		"width":     width,
		"gapWidth":  func(part, whole int) template.CSS { return width(percent(part, whole)) },
		"bandLabel": bandLabel,
		"openIf":    openIf,
		"deadHead":  func(fns []Fn) []Fn { return head(fns, deadShown) },
		"deadRest":  func(fns []Fn) int { return rest(fns, deadShown) },
		"weakHead":  func(fns []Fn) []Fn { return head(fns, weakShown) },
		"weakRest":  func(fns []Fn) int { return rest(fns, weakShown) },
		"plural":    plural,
	}
}

// width renders a bar. A zero-width bar reads as a rendering bug, so a covered
// but tiny package still gets a visible sliver.
func width(v float64) template.CSS {
	if v < 0.8 {
		v = 0.8
	}

	return template.CSS("width:" + strconv.FormatFloat(v, 'f', 3, 64) + "%")
}

// openIf expands the cards that are the reason to read the page at all.
func openIf(p Pkg) template.HTMLAttr {
	if p.Pct < 50 {
		return template.HTMLAttr(" open")
	}

	return ""
}

func head(fns []Fn, n int) []Fn {
	if len(fns) > n {
		return fns[:n]
	}

	return fns
}

func rest(fns []Fn, n int) int {
	if len(fns) > n {
		return len(fns) - n
	}

	return 0
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}

	return word + "s"
}

func bandLabel(b string) string {
	switch b {
	case "zero":
		return "No tests"
	case "thin":
		return "Thin"
	case "fair":
		return "Fair"
	case "solid":
		return "Solid"
	case "strong":
		return "Strong"
	default:
		return "—"
	}
}

func comma(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}

	var b strings.Builder

	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}

		b.WriteRune(r)
	}

	return b.String()
}

// pageCSS is the whole stylesheet. Three theme states are covered: bare :root
// is light, the media query handles a viewer who never chose (guarded so an
// explicit light choice still wins), and [data-theme="dark"] handles one who
// did. Every colour goes through a token so no rule is defined in only one of
// the three.
const pageCSS = `
:root {
  --paper:#F4F7F8; --surface:#FFFFFF; --surface-2:#E7EDEF;
  --ink:#111C22; --ink-2:#465A65; --ink-3:#7A8C96; --rule:#D5DFE3;
  --s1:#A8D2CB; --s2:#5CA79C; --s3:#2A8177; --s4:#0F5A52;
  --warm:#A83E24; --warm-soft:#F2DCD4;
  --shadow:0 1px 2px rgba(17,28,34,.05), 0 8px 24px -16px rgba(17,28,34,.35);
}
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
    --paper:#0C1317; --surface:#131C21; --surface-2:#1C272D;
    --ink:#E7EFF2; --ink-2:#9FB2BC; --ink-3:#6A7D88; --rule:#25333A;
    --s1:#2E5A55; --s2:#47897F; --s3:#63B3A5; --s4:#8AD8C7;
    --warm:#E4785A; --warm-soft:#3A2119;
    --shadow:0 1px 2px rgba(0,0,0,.4), 0 10px 30px -20px rgba(0,0,0,.9);
  }
}
:root[data-theme="dark"] {
  --paper:#0C1317; --surface:#131C21; --surface-2:#1C272D;
  --ink:#E7EFF2; --ink-2:#9FB2BC; --ink-3:#6A7D88; --rule:#25333A;
  --s1:#2E5A55; --s2:#47897F; --s3:#63B3A5; --s4:#8AD8C7;
  --warm:#E4785A; --warm-soft:#3A2119;
  --shadow:0 1px 2px rgba(0,0,0,.4), 0 10px 30px -20px rgba(0,0,0,.9);
}
* { box-sizing:border-box; }
body {
  margin:0; background:var(--paper); color:var(--ink);
  font-family:"IBM Plex Sans", ui-sans-serif, system-ui, sans-serif;
  font-size:15px; line-height:1.55; -webkit-font-smoothing:antialiased;
}
.wrap { max-width:1080px; margin:0 auto; padding:0 24px 96px; }
h1,h2,h3,h4 { font-family:"IBM Plex Sans Condensed","IBM Plex Sans",sans-serif;
  font-weight:700; letter-spacing:-.01em; text-wrap:balance; margin:0; }
code, .pkg, .file, .loc, .pctnum, .tile-num, .hero-num, .c-num, .fnpct,
.group-pct, .gap-n { font-family:"IBM Plex Mono", ui-monospace, SFMono-Regular, Menlo, monospace;
  font-variant-numeric:tabular-nums; }

.mast { padding:56px 0 32px; border-bottom:2px solid var(--ink); margin-bottom:36px; }
.eyebrow { font-family:"IBM Plex Mono",monospace; font-size:11.5px; letter-spacing:.16em;
  text-transform:uppercase; color:var(--ink-3); display:block; margin-bottom:14px; }
.mast h1 { font-size:clamp(34px,6vw,54px); line-height:1.02; }
.mast .lede { margin:16px 0 0; max-width:62ch; color:var(--ink-2); font-size:16.5px; }
.runmeta { display:flex; flex-wrap:wrap; gap:6px 20px; margin-top:22px;
  font-family:"IBM Plex Mono",monospace; font-size:12px; color:var(--ink-3); }
.runmeta b { color:var(--ink-2); font-weight:500; }

.tiles { display:grid; grid-template-columns:repeat(auto-fit,minmax(168px,1fr)); gap:1px;
  background:var(--rule); border:1px solid var(--rule); box-shadow:var(--shadow); }
.tile { background:var(--surface); padding:20px; display:flex; flex-direction:column; gap:6px; }
.tile-hero { grid-column:span 2; }
@media (max-width:680px) { .tile-hero { grid-column:span 1; } }
.tile-label { font-size:11.5px; letter-spacing:.1em; text-transform:uppercase; color:var(--ink-3);
  font-family:"IBM Plex Mono",monospace; }
.tile-num { font-size:34px; font-weight:600; line-height:1.05; }
.tile-num.warm { color:var(--warm); }
.hero-num { font-size:64px; font-weight:600; line-height:.95; letter-spacing:-.03em; }
.hero-sign { font-size:26px; color:var(--ink-3); margin-left:2px; }
.tile-sub { font-size:13px; color:var(--ink-2); }

.bar { height:8px; background:var(--surface-2); border-radius:1px; overflow:hidden; min-width:60px; }
.bar-lg { height:12px; margin:6px 0 2px; }
.bar-sm { height:6px; }
.bar-fill { height:100%; border-radius:0 1px 1px 0; }
.b-zero { background:var(--warm); }
.b-thin { background:var(--s1); }
.b-fair { background:var(--s2); }
.b-solid { background:var(--s3); }
.b-strong { background:var(--s4); }
.b-none { background:var(--ink-3); }

.pill { font-family:"IBM Plex Mono",monospace; font-size:10.5px; letter-spacing:.08em;
  text-transform:uppercase; padding:3px 8px; border:1px solid var(--rule); border-radius:2px;
  color:var(--ink-2); white-space:nowrap; background:var(--surface-2); }
.p-zero { color:var(--warm); border-color:var(--warm); background:var(--warm-soft); font-weight:600; }

section.block { margin-top:56px; }
.block-head { display:flex; align-items:baseline; gap:16px; flex-wrap:wrap;
  border-bottom:1px solid var(--rule); padding-bottom:10px; margin-bottom:22px; }
.block-head h2 { font-size:24px; }
.block-head p { margin:0; color:var(--ink-3); font-size:13.5px; }

.tablewrap { overflow-x:auto; }
table { border-collapse:collapse; width:100%; }
caption { text-align:left; font-family:"IBM Plex Mono",monospace; font-size:11px;
  letter-spacing:.1em; text-transform:uppercase; color:var(--ink-3); padding:14px 0 8px; }
th, td { text-align:left; padding:7px 12px 7px 0; border-bottom:1px solid var(--rule);
  font-weight:400; vertical-align:middle; }
thead th { font-size:11px; letter-spacing:.1em; text-transform:uppercase; color:var(--ink-3);
  font-family:"IBM Plex Mono",monospace; border-bottom:1px solid var(--ink-3); }
.rank tbody tr:hover { background:var(--surface); }
.pkg { font-size:13.5px; }
.file { font-size:12.5px; color:var(--ink-2); }
.c-bar { width:34%; min-width:110px; }
.c-pct { text-align:right; white-space:nowrap; width:70px; }
.pctnum { font-size:14px; font-weight:500; }
.pctsign { color:var(--ink-3); font-size:11px; margin-left:1px; }
.c-num { text-align:right; font-size:12.5px; color:var(--ink-2); white-space:nowrap; }
.slash { color:var(--ink-3); padding:0 1px; }
.dim { color:var(--ink-3); }
.c-band { text-align:right; }

.gaps { list-style:none; margin:0; padding:0; display:grid;
  grid-template-columns:repeat(auto-fit,minmax(310px,1fr)); gap:20px 32px; }
.gap-head { display:flex; justify-content:space-between; align-items:baseline; gap:12px; }
.gap-n { font-size:14px; font-weight:500; }
.gap-n .unit { font-family:"IBM Plex Sans",sans-serif; font-size:11px; color:var(--ink-3);
  text-transform:uppercase; letter-spacing:.08em; }
.gap-bar { margin:7px 0 5px; }
.gap-sub { font-size:12.5px; color:var(--ink-3); }

.group { margin-top:44px; }
.group-head { display:grid; grid-template-columns:1fr auto; gap:4px 24px; align-items:end;
  border-bottom:2px solid var(--ink); padding-bottom:12px; margin-bottom:14px; }
.group-head h3 { font-size:21px; display:flex; align-items:baseline; gap:12px; }
.group-count { font-family:"IBM Plex Mono",monospace; font-size:11px; font-weight:400;
  letter-spacing:.1em; text-transform:uppercase; color:var(--ink-3); }
.group-note { margin:0; grid-column:1; font-size:13.5px; color:var(--ink-2); max-width:58ch; }
.group-stat { grid-row:1 / span 2; grid-column:2; display:flex; flex-direction:column;
  align-items:flex-end; gap:4px; min-width:180px; }
.group-stat .bar { width:180px; }
.group-pct { font-size:19px; font-weight:600; }
.group-sub { font-size:12px; color:var(--ink-3); }
@media (max-width:640px) { .group-head { grid-template-columns:1fr; }
  .group-stat { grid-row:auto; grid-column:1; align-items:flex-start; } }

.card { background:var(--surface); border:1px solid var(--rule); margin-bottom:8px;
  box-shadow:var(--shadow); }
.card > summary { list-style:none; cursor:pointer; padding:13px 16px; display:grid;
  grid-template-columns:minmax(150px,1fr) minmax(90px,220px) 74px 92px 16px;
  align-items:center; gap:16px; }
.card > summary::-webkit-details-marker { display:none; }
.card > summary:hover { background:var(--surface-2); }
.card > summary:focus-visible { outline:2px solid var(--s3); outline-offset:-2px; }
.sum-name { font-family:"IBM Plex Mono",monospace; font-size:13.5px; font-weight:500; }
.sum-pct { text-align:right; }
.sum-pct .pctnum { font-size:15px; font-weight:600; }
.chev { width:8px; height:8px; border-right:1.5px solid var(--ink-3);
  border-bottom:1.5px solid var(--ink-3); transform:rotate(45deg) translate(-2px,-2px);
  transition:transform .18s ease; justify-self:end; }
.card[open] > summary { border-bottom:1px solid var(--rule); }
.card[open] .chev { transform:rotate(-135deg) translate(-2px,-2px); }
@media (prefers-reduced-motion:reduce) { .chev { transition:none; } }
@media (max-width:720px) {
  .card > summary { grid-template-columns:1fr auto auto; }
  .sum-bar { display:none; } }
.card-body { padding:4px 16px 22px; }
.facts { display:grid; grid-template-columns:repeat(auto-fit,minmax(112px,1fr));
  gap:14px 20px; margin:16px 0 4px; padding:14px; background:var(--surface-2); }
.facts div { display:flex; flex-direction:column; gap:2px; }
.facts dt { font-family:"IBM Plex Mono",monospace; font-size:10.5px; letter-spacing:.09em;
  text-transform:uppercase; color:var(--ink-3); }
.facts dd { margin:0; font-family:"IBM Plex Mono",monospace; font-size:14px; font-weight:500; }
.ftable th[scope="row"] { padding-left:0; }
.fnblock { margin-top:20px; }
.fnblock h4 { font-size:11.5px; letter-spacing:.1em; text-transform:uppercase; color:var(--ink-3);
  font-family:"IBM Plex Mono",monospace; font-weight:500; display:flex; align-items:center; gap:8px; }
.fnblock .count { background:var(--surface-2); padding:1px 7px; color:var(--ink-2); }
.fnlist { list-style:none; margin:10px 0 0; padding:0; display:grid;
  grid-template-columns:repeat(auto-fill,minmax(240px,1fr)); gap:4px 20px; }
.fnlist li { display:flex; align-items:baseline; gap:8px; font-size:12.5px;
  border-bottom:1px dotted var(--rule); padding:3px 0; }
.fnlist code { font-size:12.5px; color:var(--ink); }
.fnlist .loc { font-size:10.5px; color:var(--ink-3); margin-left:auto; white-space:nowrap; }
.fnlist .fnpct { font-family:"IBM Plex Mono",monospace; font-size:11px; color:var(--s3);
  font-weight:500; }
.fnlist .more { color:var(--ink-3); font-style:italic; border:none; }
.clean { margin:16px 0 0; font-size:13.5px; color:var(--ink-2); padding:12px 14px;
  background:var(--surface-2); border-left:2px solid var(--s3); }

.legend { display:flex; flex-wrap:wrap; gap:8px 20px; margin-top:16px;
  font-family:"IBM Plex Mono",monospace; font-size:11.5px; color:var(--ink-2); }
.legend span { display:inline-flex; align-items:center; gap:7px; }
.swatch { width:16px; height:8px; display:inline-block; }
.notes { margin-top:64px; padding-top:22px; border-top:1px solid var(--rule);
  font-size:13px; color:var(--ink-2); }
.notes h2 { font-size:15px; margin-bottom:10px; }
.notes ul { margin:0; padding-left:18px; }
.notes li { margin-bottom:7px; max-width:76ch; }
.notes code { background:var(--surface-2); padding:1px 5px; font-size:12px; }
`

// pageHTML is the report itself. It carries no logic beyond ranging over what
// assemble() and finish() already ordered.
const pageHTML = `<title>{{.Title}}</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500;600&family=IBM+Plex+Sans+Condensed:wght@600;700&family=IBM+Plex+Sans:wght@400;500;600&display=swap">
<style>{{.CSS}}</style>

<div class="wrap">
<header class="mast">
  <span class="eyebrow">{{.Module}} &middot; go test coverage</span>
  <h1>Coverage by package</h1>
  <p class="lede">Statement coverage for every buildable package in the module, ranked, then
  broken down file by file and function by function.{{if .ZeroPkgs}} {{.ZeroPkgs}}
  {{plural .ZeroPkgs "package"}} {{if eq .ZeroPkgs 1}}has{{else}}have{{end}} no test of
  {{if eq .ZeroPkgs 1}}its{{else}}their{{end}} own; together they hold
  {{share .ZeroStmts .Stmts}}% of all statements in the module.{{end}}</p>
  <div class="runmeta">
    <span><b>go test ./... -covermode=atomic</b></span>
    <span>commit <b>{{.Commit}}</b></span>
    <span><b>{{.GoVersion}}</b> {{.Platform}}</span>
    <span>run <b>{{.RunDate}}</b></span>
    <span><b>{{len .Packages}}</b> packages &middot; <b>{{num .Stmts}}</b> statements</span>
  </div>
</header>

<div class="tiles">
  <div class="tile tile-hero">
    <span class="tile-label">Overall statement coverage</span>
    <span class="hero-num">{{pct1 .Pct}}<span class="hero-sign">%</span></span>
    <div class="bar bar-lg"><div class="bar-fill b-{{.Band}}" style="{{width .Pct}}"></div></div>
    <span class="tile-sub">{{num .Covered}} of {{num .Stmts}} statements executed</span>
  </div>
  <div class="tile"><span class="tile-label">Statements never run</span>
    <span class="tile-num warm">{{num .Uncovered}}</span>
    <span class="tile-sub">{{share .Uncovered .Stmts}}% of the codebase</span></div>
  <div class="tile"><span class="tile-label">Packages measured</span>
    <span class="tile-num">{{len .Packages}}</span>
    <span class="tile-sub">{{.Internal}} under internal/</span></div>
  <div class="tile"><span class="tile-label">Packages with no test</span>
    <span class="tile-num warm">{{.ZeroPkgs}}</span>
    <span class="tile-sub">{{num .ZeroStmts}} statements, none executed</span></div>
  <div class="tile"><span class="tile-label">Packages at 90% or better</span>
    <span class="tile-num">{{.Strong}}</span>
    <span class="tile-sub">of {{len .Packages}} measured</span></div>
</div>

<section class="block">
  <div class="block-head">
    <h2>Where the untested code actually is</h2>
    <p>Ranked by number of statements never executed &mdash; not by percentage</p>
  </div>
  <ol class="gaps">
  {{- range .Gaps}}
    <li class="gap">
      <div class="gap-head"><span class="pkg">{{.Name}}</span>
        <span class="gap-n">{{num .Uncovered}} <span class="unit">uncovered</span></span></div>
      <div class="bar gap-bar"><div class="bar-fill b-{{.Band}}" style="{{gapWidth .Uncovered $.MaxGap}}"></div></div>
      <div class="gap-sub">{{pct1 .Pct}}% covered &middot; {{num .Stmts}} statements &middot; {{num .SrcLOC}} lines of source</div>
    </li>
  {{- end}}
  </ol>
</section>

<section class="block">
  <div class="block-head">
    <h2>Every package, best to worst</h2>
    <p>Test&thinsp;:&thinsp;source is the ratio of test lines to source lines on disk</p>
  </div>
  <div class="tablewrap">
  <table class="rank">
    <thead><tr>
      <th scope="col">Package</th><th scope="col">Coverage</th><th scope="col"></th>
      <th scope="col">Covered / total</th><th scope="col">Test : source</th><th scope="col">Band</th>
    </tr></thead>
    <tbody>
    {{- range .Packages}}
      <tr>
        <th scope="row"><span class="pkg">{{.Name}}</span></th>
        <td class="c-bar"><div class="bar"><div class="bar-fill b-{{.Band}}" style="{{width .Pct}}"></div></div></td>
        <td class="c-pct"><span class="pctnum">{{pct1 .Pct}}</span><span class="pctsign">%</span></td>
        <td class="c-num">{{num .Covered}}<span class="slash">/</span>{{num .Stmts}}</td>
        <td class="c-num dim">{{ratio .Ratio}}&times;</td>
        <td class="c-band"><span class="pill p-{{.Band}}">{{bandLabel .Band}}</span></td>
      </tr>
    {{- end}}
    </tbody>
  </table></div>
  <div class="legend">
    <span><i class="swatch b-zero"></i>No tests &mdash; 0%</span>
    <span><i class="swatch b-thin"></i>Thin &mdash; under 50%</span>
    <span><i class="swatch b-fair"></i>Fair &mdash; 50 to 75%</span>
    <span><i class="swatch b-solid"></i>Solid &mdash; 75 to 90%</span>
    <span><i class="swatch b-strong"></i>Strong &mdash; 90% and up</span>
  </div>
</section>

<section class="block">
  <div class="block-head">
    <h2>Package detail</h2>
    <p>Open a package for its files and its untested functions &mdash; anything under 50% is already open</p>
  </div>
  {{- range .Groups}}
  <section class="group">
    <header class="group-head">
      <h3>{{.Name}}<span class="group-count">{{len .Pkgs}} {{plural (len .Pkgs) "package"}}</span></h3>
      <p class="group-note">{{.Note}}</p>
      <div class="group-stat">
        <div class="bar"><div class="bar-fill b-{{.Band}}" style="{{width .Pct}}"></div></div>
        <span class="group-pct">{{pct1 .Pct}}%</span>
        <span class="group-sub">{{num .Covered}} / {{num .Stmts}} statements</span>
      </div>
    </header>
    {{- range .Pkgs}}
    <details class="card"{{openIf .}}>
      <summary>
        <span class="sum-name">{{.Name}}</span>
        <span class="sum-bar"><div class="bar"><div class="bar-fill b-{{.Band}}" style="{{width .Pct}}"></div></div></span>
        <span class="sum-pct"><span class="pctnum">{{pct1 .Pct}}</span><span class="pctsign">%</span></span>
        <span class="pill p-{{.Band}}">{{bandLabel .Band}}</span>
        <span class="chev" aria-hidden="true"></span>
      </summary>
      <div class="card-body">
        <dl class="facts">
          <div><dt>Statements</dt><dd>{{num .Covered}} <span class="slash">/</span> {{num .Stmts}}</dd></div>
          <div><dt>Uncovered</dt><dd>{{num .Uncovered}}</dd></div>
          <div><dt>Source files</dt><dd>{{.GoFiles}}</dd></div>
          <div><dt>Test files</dt><dd>{{.TestFiles}}</dd></div>
          <div><dt>Source lines</dt><dd>{{num .SrcLOC}}</dd></div>
          <div><dt>Test lines</dt><dd>{{num .TestLOC}} <span class="dim">({{ratio .Ratio}}&times;)</span></dd></div>
        </dl>
        <div class="tablewrap">
        <table class="ftable">
          <caption>Per-file coverage</caption>
          <thead><tr><th scope="col">File</th><th scope="col"></th>
            <th scope="col">Covered</th><th scope="col">Statements</th></tr></thead>
          <tbody>
          {{- range .Files}}
            <tr>
              <th scope="row"><span class="file">{{.Name}}</span></th>
              <td class="c-bar"><div class="bar bar-sm"><div class="bar-fill b-{{.Band}}" style="{{width .Pct}}"></div></div></td>
              <td class="c-pct"><span class="pctnum">{{pct1 .Pct}}</span><span class="pctsign">%</span></td>
              <td class="c-num">{{num .Covered}}<span class="slash">/</span>{{num .Stmts}}</td>
            </tr>
          {{- end}}
          </tbody>
        </table></div>
        {{- if .Dead}}
        <div class="fnblock">
          <h4>Never executed <span class="count">{{len .Dead}}</span></h4>
          <ul class="fnlist">
          {{- range deadHead .Dead}}
            <li><code>{{.Name}}</code><span class="loc">{{.File}}:{{.Line}}</span></li>
          {{- end}}
          {{- with deadRest .Dead}}<li class="more">+{{.}} more</li>{{end}}
          </ul>
        </div>
        {{- end}}
        {{- if .Weak}}
        <div class="fnblock">
          <h4>Partly covered <span class="count">{{len .Weak}}</span></h4>
          <ul class="fnlist">
          {{- range weakHead .Weak}}
            <li><code>{{.Name}}</code><span class="loc">{{.File}}:{{.Line}}</span><span class="fnpct">{{pct1 .Pct}}%</span></li>
          {{- end}}
          {{- with weakRest .Weak}}<li class="more">+{{.}} more under 60%</li>{{end}}
          </ul>
        </div>
        {{- end}}
        {{- if and (not .Dead) (not .Weak)}}
        <p class="clean">Every function in this package is executed by the suite, and none of them
        sits under 60%.</p>
        {{- end}}
      </div>
    </details>
    {{- end}}
  </section>
  {{- end}}
</section>

<section class="notes">
  <h2>How this was measured</h2>
  <ul>
    <li>One <code>go test -coverprofile -covermode=atomic ./...</code> run over the whole module.
    Each package is measured by its own tests only; cross-package execution is not credited, so a
    package exercised entirely through another one still reads 0%.</li>
    <li>Percentages are <b>statements</b>, not lines or branches &mdash; the unit Go's cover tool
    emits. Percentage answers "how well tested is this package"; the uncovered-statement count
    answers "where should the next test go". The two rankings disagree, which is why both are here.</li>
    <li>Packages holding only build-tagged test files have no source statements to cover and do not
    appear at all.</li>
    <li>Test&thinsp;:&thinsp;source counts raw lines of <code>*_test.go</code> against non-test
    <code>.go</code> files in the same directory. It is a rough measure of effort, not of quality.</li>
    <li>Regenerate with <code>make cover-report</code>.</li>
  </ul>
</section>
</div>
`
