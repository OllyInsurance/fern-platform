package api

import (
	"regexp"
	"strings"
)

// Criterion verdicts, from the most specific linked test result.
const (
	VerdictPassing  = "passing"   // a linked test passed and none says otherwise
	VerdictFailing  = "failing"   // a linked test failed (not a known defect)
	VerdictKnownGap = "known_gap" // a linked test stopped at a KNOWN GAP skip or a Playwright fixme
	VerdictDefect   = "defect"    // a linked defect test (named "defect") failed or was held back
	VerdictNotRun   = "not_run"   // linked tests, but no result in the window
	VerdictUntested = "untested"  // no linked test
)

// Verdicts in display order.
var Verdicts = []string{VerdictPassing, VerdictFailing, VerdictKnownGap, VerdictDefect, VerdictNotRun, VerdictUntested}

// verdictRank orders test verdicts when several tests prove one criterion:
// the highest rank decides. A failure outranks everything; a test that says
// its part is a known gap outranks another that passed (the criterion is not
// whole); a test with no result counts only when nothing ran.
var verdictRank = map[string]int{
	VerdictNotRun:   1,
	VerdictPassing:  2,
	VerdictKnownGap: 3,
	VerdictDefect:   4,
	VerdictFailing:  5,
}

var (
	reKnownGap    = regexp.MustCompile(`(?i)known[ _-]gap`)
	reKnownDefect = regexp.MustCompile(`(?i)known[ _-]defect`)
	reFixme       = regexp.MustCompile(`(?i)(\bfixme\b|test\.fixme)`)
	reDefectName  = regexp.MustCompile(`(?i)defect`)
)

// gapMarker reads a skipped result's message, description and metadata for
// why it was skipped: "known_gap" (a KNOWN GAP skip), "fixme" (a Playwright
// fixme), "known_defect" (held back by a filed defect) or "".
func gapMarker(text string) string {
	switch {
	case reKnownDefect.MatchString(text):
		return "known_defect"
	case reKnownGap.MatchString(text):
		return "known_gap"
	case reFixme.MatchString(text):
		return "fixme"
	}
	return ""
}

// markerLine is the first line of text naming the marker, for display.
func markerLine(text string) string {
	for _, l := range strings.Split(text, "\n") {
		if reKnownGap.MatchString(l) || reKnownDefect.MatchString(l) || reFixme.MatchString(l) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// testVerdict is one test's verdict from its latest result. name is the
// registry name; a defect test is one whose name (or reported name) says
// "defect", the house convention for a subtest that asserts a filed bug.
func testVerdict(name string, r *CoverageResult) string {
	if r == nil {
		return VerdictNotRun
	}
	defectNamed := reDefectName.MatchString(name) || reDefectName.MatchString(r.SpecName)
	switch r.Status {
	case "passed":
		return VerdictPassing
	case "failed":
		if defectNamed {
			return VerdictDefect
		}
		return VerdictFailing
	case "skipped":
		switch {
		// The gate skips a defect test (helpers.Defect); that skip is the
		// filed defect, never a known gap.
		case r.GapMarker == "known_defect" || defectNamed:
			return VerdictDefect
		case r.GapMarker == "known_gap" || r.GapMarker == "fixme":
			return VerdictKnownGap
		}
		// A plain skip proves nothing and explains nothing.
		return VerdictNotRun
	}
	return VerdictNotRun
}

// outcome is a test's verdict and the result that set it. A Go defect test
// is decided by the defects pass, which runs what the gate skips: failed
// there means the defect still reproduces, passed means it is fixed. Every
// other test, and a defect test the defects pass has not run, is decided by
// its gate result.
func (t *coverageTest) outcome() (string, *CoverageResult) {
	if d := t.DefectsLatest; d != nil && !gateRanSince(t.Latest, d) {
		defectNamed := reDefectName.MatchString(t.Name) || reDefectName.MatchString(strings.TrimPrefix(d.SpecName, GoDefectsPrefix))
		if defectNamed || t.Latest == nil || t.Latest.Status == "skipped" {
			switch d.Status {
			case "failed":
				return VerdictDefect, d
			case "passed":
				return VerdictPassing, d
			}
		}
	}
	return testVerdict(t.Name, t.Latest), t.Latest
}

// gateRanSince says whether the gate ran the test (passed or failed, not
// skipped) after the defects pass did. A fixed defect drops its
// helpers.Defect marker but keeps its "defect_N" name, so the gate runs it
// and the defects pass never runs it again; its last defects-pass failure is
// then stale and the newer gate run decides.
func gateRanSince(g, d *CoverageResult) bool {
	if g == nil || d == nil || (g.Status != "passed" && g.Status != "failed") {
		return false
	}
	return g.StartTime.After(d.StartTime)
}

// hasResult says whether the test has any result in the window.
func (t *coverageTest) hasResult() bool { return t.Latest != nil || t.DefectsLatest != nil }

// linkAncestors records, for every test, the keys of the tests above it by
// following the registry's parent links (a cycle stops the walk).
func linkAncestors(byKey map[string]*coverageTest) {
	for _, t := range byKey {
		t.ancestors = map[string]bool{}
		for p := t.Parent; p != "" && !t.ancestors[p] && p != t.Key; {
			t.ancestors[p] = true
			pt := byKey[p]
			if pt == nil {
				break
			}
			p = pt.Parent
		}
	}
}

// isUnder says whether t is a subtest of p: by the registry's parent links,
// or failing those by name.
func (t *coverageTest) isUnder(p *coverageTest) bool {
	return t.Parent == p.Key || t.ancestors[p.Key] || isDescendant(t.Name, p.Name)
}

// isDescendant reports whether child names a subtest of parent: a Go subtest
// ("TestX/sub", spaces reported as underscores) or a Playwright test inside a
// describe ("describe › title").
func isDescendant(child, parent string) bool {
	if parent == "" || len(child) <= len(parent) {
		return false
	}
	for _, sep := range []string{"/", " › "} {
		if strings.HasPrefix(child, parent+sep) {
			return true
		}
		u := strings.ReplaceAll(parent, " ", "_") + sep
		if strings.HasPrefix(strings.ReplaceAll(child, " ", "_"), u) {
			return true
		}
	}
	return false
}

// criterionVerdict decides a criterion from its linked tests and returns the
// keys of the tests that decided it. A linked subtest with a result beats its
// parent: a parent that passes while the subtest proving this criterion stopped
// at a known gap must not read as proof.
func criterionVerdict(tests []*coverageTest) (string, []string) {
	if len(tests) == 0 {
		return VerdictUntested, nil
	}
	var effective []*coverageTest
	for _, p := range tests {
		superseded := false
		for _, c := range tests {
			if c != p && c.hasResult() && c.isUnder(p) {
				superseded = true
				break
			}
		}
		if !superseded {
			effective = append(effective, p)
		}
	}
	best := VerdictNotRun
	for _, t := range effective {
		if v, _ := t.outcome(); verdictRank[v] > verdictRank[best] {
			best = v
		}
	}
	var by []string
	for _, t := range effective {
		if v, _ := t.outcome(); v == best {
			by = append(by, t.Key)
		}
	}
	return best, by
}

// newVerdictCounts has a zero for every verdict, so a reader can rely on keys.
func newVerdictCounts() map[string]int {
	m := make(map[string]int, len(Verdicts))
	for _, v := range Verdicts {
		m[v] = 0
	}
	return m
}
