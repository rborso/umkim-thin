package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/rborso/umkim-thin/internal/catalog"
)

// catalog verify: report what each entry's test claims actually resolve to.
//
// The catalog says an app is "tested" through verify.checks. Those names are only
// meaningful if a Playwright test carries them, so this command resolves every
// one against the test tree and reports the derived category. ci/static.sh runs it
// with --strict, which fails on an unresolved check: that is what stops the list
// drifting into the free-text comments it used to be (four entries named checks
// that appeared in no test at all).
//
// A check is carried by a test's `@check:<name>` tag in its title, e.g.
//
//	test('a regular user signs in @check:ldap_user_visible', ...)
//
// The tag is greppable, survives reordering, and is visible in the HTML report.

// checkTag matches `@check:` followed by the check name. A per-app loop writes
// the tag once with a `${p.app}` placeholder, so `$`, `{`, `}` and `.` are
// permitted and expanded later by expandLoopTags.
var checkTag = regexp.MustCompile(`@check:([A-Za-z0-9_${}.]+)`)

type tagLocation struct {
	check string
	file  string // path relative to the playwright dir
	title string
}

// collectTags walks the suites and returns every @check tag with its location.
func collectTags(playwrightDir string) (map[string][]tagLocation, error) {
	tags := map[string][]tagLocation{}
	err := filepath.Walk(playwrightDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", "test-results", "reports", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".spec.ts") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(playwrightDir, path)
		if err != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		// A per-app loop writes its tag once with a placeholder, e.g.
		//   test(`... to ${p.app} ... @check:tenant_api_${p.app}`)
		// which expands to one check per app at run time. Register the tag for
		// every app this file loops over, so each declared check resolves instead
		// of demanding a duplicated literal.
		var fileApps []string
		for _, m := range loopApps.FindAllStringSubmatch(string(data), -1) {
			fileApps = append(fileApps, m[1])
		}
		for _, line := range strings.Split(string(data), "\n") {
			// The test title and its tag are on the same line in this suite; a
			// tag elsewhere is not a claim about a test, so it is ignored.
			if !strings.Contains(line, "test(") && !strings.Contains(line, "test.describe(") {
				continue
			}
			for _, name := range expandLoopTags(line, fileApps) {
				tags[name] = append(tags[name], tagLocation{
					check: name,
					file:  rel,
					title: testTitle(line),
				})
			}
		}
		return nil
	})
	return tags, err
}

// collectHarnessTags reads a non-Playwright harness script (e.g. ci/vpn-check.sh)
// for the same @check tags, so an app with no browser surface is checked just as
// concretely as one with a suite. A harness marks a passing check by tagging its
// ok/bad line; anywhere else in the file is ignored, because a tag in a comment
// proves nothing.
func collectHarnessTags(workdir, script string) (map[string]tagLocation, error) {
	found := map[string]tagLocation{}
	data, err := os.ReadFile(filepath.Join(workdir, script))
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "ok ") && !strings.Contains(line, "bad ") {
			continue
		}
		for _, m := range checkTag.FindAllStringSubmatch(line, -1) {
			found[m[1]] = tagLocation{check: m[1], file: script, title: strings.TrimSpace(line)}
		}
	}
	return found, nil
}

// loopApps matches the app list a per-app loop iterates, so a templated tag can
// be expanded. It reads the `{ app: 'name' ... }` literals in the same suite.
var loopApps = regexp.MustCompile(`app:\s*'([a-z0-9_-]+)'`)

// expandLoopTags returns the check names a source line carries, expanding a
// `${...}` placeholder in a tag for each app the suite loops over.
func expandLoopTags(line string, apps []string) []string {
	raw := checkTag.FindAllStringSubmatch(line, -1)
	if len(raw) == 0 {
		return nil
	}
	var out []string
	for _, m := range raw {
		name := m[1]
		if !strings.Contains(name, "${") {
			out = append(out, name)
			continue
		}
		for _, a := range apps {
			// The suite's loops use ${p.app}; any other placeholder is left alone
			// so a broken tag still shows up as unresolved.
			out = append(out, strings.ReplaceAll(name, "${p.app}", a))
		}
	}
	return out
}

func testTitle(line string) string {
	i := strings.Index(line, "test(")
	if i < 0 {
		return ""
	}
	rest := line[i+len("test("):]
	rest = strings.TrimLeft(rest, " ")
	if rest == "" {
		return ""
	}
	q := rest[0]
	if q != '\'' && q != '`' {
		return ""
	}
	if j := strings.IndexByte(rest[1:], q); j >= 0 {
		return rest[1 : 1+j]
	}
	return ""
}

// gating reports whether a suite file fails CI.
func gating(file string) bool {
	for _, g := range catalog.GatingSuites {
		if filepath.ToSlash(file) == g {
			return true
		}
	}
	return false
}

type entryReport struct {
	Entry         catalog.Entry
	Auto          bool
	Informational bool
	Missing       []string
	Tagged        map[string][]tagLocation
	Stale         bool
}

func (r entryReport) category() string {
	return r.Entry.Category(r.Auto, r.Informational)
}

func verifyCmd(workdir string, args []string) error {
	strict := false
	asJSON := false
	playwrightDir := filepath.Join(workdir, "tests", "playwright")
	for _, a := range args {
		switch a {
		case "--strict":
			strict = true
		case "--json":
			asJSON = true
		case "-h", "--help":
			fmt.Println(`umkim-thin catalog verify [--strict] [--json] [--playwright DIR]

Resolve every verify.checks name against the Playwright suites and report each
app's derived category (auto-tested, auto-checked, verified, untested).

--strict  exit 1 when a declared check is carried by no test, so CI cannot ship
          a claim it cannot prove.`)
			return nil
		default:
			if strings.HasPrefix(a, "--playwright=") {
				playwrightDir = strings.TrimPrefix(a, "--playwright=")
				continue
			}
			return fmt.Errorf("unknown option %q", a)
		}
	}

	entries, err := catalog.Load(filepath.Join(workdir, "catalog"))
	if err != nil {
		return err
	}
	tags, err := collectTags(playwrightDir)
	if err != nil {
		return fmt.Errorf("reading the Playwright suites: %w", err)
	}

	var reports []entryReport
	for _, e := range entries {
		r := entryReport{Entry: e, Tagged: map[string][]tagLocation{}, Stale: e.VerificationStale()}
		allAuto, anyTest := true, false
		// An app with no browser surface declares a harness; its tags come from that
		// script and count as gate-level, because ci/extended.sh runs it.
		harnessTags := map[string]tagLocation{}
		if e.Verify.Harness != "" {
			harnessTags, _ = collectHarnessTags(workdir, e.Verify.Harness)
		}
		for _, c := range e.Verify.Checks {
			if l, ok := harnessTags[c]; ok {
				// A harness tag is a real, gating check (ci/extended.sh runs the
				// script), so it counts for both flags.
				r.Tagged[c] = []tagLocation{l}
				anyTest = true
				continue
			}
			locs := tags[c]
			if len(locs) == 0 {
				r.Missing = append(r.Missing, c)
				allAuto = false
				continue
			}
			anyTest = true
			r.Tagged[c] = locs
			auto := false
			for _, l := range locs {
				if gating(l.file) {
					auto = true
				}
			}
			if !auto {
				allAuto = false
			}
		}
		if len(e.Verify.Checks) > 0 {
			r.Auto = allAuto && anyTest
			r.Informational = anyTest
		}
		reports = append(reports, r)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].Entry.Name < reports[j].Entry.Name })

	if asJSON {
		return printVerifyJSON(reports)
	}

	failed := 0
	fmt.Printf("%-14s %-13s %-9s %s\n", "APP", "AUTO", "VERSION", "HUMAN")
	for _, r := range reports {
		e := r.Entry
		human := "-"
		if h := e.Verify.Human; h != nil {
			human = h.Date + " " + h.By
			if r.Stale {
				human += " (STALE: on " + h.Version + ")"
			}
		}
		fmt.Printf("%-14s %-13s %-9s %s\n", e.Name, r.category(), e.Chart.Version, human)
		if len(r.Missing) > 0 {
			failed++
			fmt.Printf("               unresolved: %s\n", strings.Join(r.Missing, ", "))
		}
		for _, c := range e.Verify.Checks {
			if locs := r.Tagged[c]; len(locs) > 0 {
				// A harness check gates CI too (ci/extended.sh runs the script);
				// only an informational Playwright suite is "info".
				g := "gate"
				if r.Entry.Verify.Harness == "" && !gating(locs[0].file) {
					g = "info"
				}
				fmt.Printf("               %-32s %-4s %s\n", c, g, locs[0].file)
			}
		}
	}

	if failed > 0 && strict {
		return fmt.Errorf("%d entr(y/ies) declare a check that no test carries", failed)
	}
	return nil
}

func printVerifyJSON(reports []entryReport) error {
	type jsonCheck struct {
		Check string `json:"check"`
		Suite string `json:"suite"`
	}
	type jsonHuman struct {
		Date    string   `json:"date"`
		Version string   `json:"version"`
		By      string   `json:"by"`
		Targets []string `json:"targets,omitempty"`
		Did     []string `json:"did,omitempty"`
	}
	type jsonEntry struct {
		Name          string      `json:"name"`
		Category      string      `json:"category"`
		ChartVersion  string      `json:"chartVersion"`
		Auto          bool        `json:"auto"`
		Informational bool        `json:"informational"`
		Checks        []jsonCheck `json:"checks"`
		Human         *jsonHuman  `json:"human"`
	}
	out := make([]jsonEntry, 0, len(reports))
	for _, r := range reports {
		je := jsonEntry{
			Name:          r.Entry.Name,
			Category:      r.category(),
			ChartVersion:  r.Entry.Chart.Version,
			Auto:          r.Auto,
			Informational: r.Informational,
			Checks:        []jsonCheck{},
		}
		for _, c := range r.Entry.Verify.Checks {
			suite := ""
			if locs := r.Tagged[c]; len(locs) > 0 {
				suite = locs[0].file
			}
			je.Checks = append(je.Checks, jsonCheck{Check: c, Suite: suite})
		}
		if h := r.Entry.Verify.Human; h != nil {
			je.Human = &jsonHuman{
				Date: h.Date, Version: h.Version, By: h.By,
				Targets: h.Targets, Did: h.Did,
			}
		}
		out = append(out, je)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
