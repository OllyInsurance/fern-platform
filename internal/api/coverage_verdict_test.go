package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func res(status, specName, marker string) *CoverageResult {
	return &CoverageResult{Status: status, SpecName: specName, GapMarker: marker}
}

func TestGapMarker(t *testing.T) {
	cases := map[string]string{
		"note: KNOWN GAP ENG-630 NFR-02: no detail chips (no code)":         "known_gap",
		"--- SKIP: TestX/S01 (0.0s)\n    x_test.go:12: known gap: no route": "known_gap",
		"KNOWN DEFECT olly#2348: excluded from the gate":                    "known_defect",
		"note: fixme: the composer has no detail chips":                     "fixme",
		`{"annotations":[{"type":"fixme","description":"no chips"}]}`:       "fixme",
		"skipped 3s":                          "",
		"note: no audio module on this build": "",
		"":                                    "",
	}
	for text, want := range cases {
		if got := gapMarker(text); got != want {
			t.Errorf("gapMarker(%q) = %q, want %q", text, got, want)
		}
	}
	if got := markerLine("✓ step one\n✗ step two\nnote: KNOWN GAP ENG-1 AC-2: missing"); got != "note: KNOWN GAP ENG-1 AC-2: missing" {
		t.Errorf("markerLine = %q", got)
	}
}

func TestTestVerdict(t *testing.T) {
	cases := []struct {
		name, test string
		r          *CoverageResult
		want       string
	}{
		{"no result", "TestX", nil, VerdictNotRun},
		{"passed", "TestX", res("passed", "TestX", ""), VerdictPassing},
		{"failed", "TestX/S01 works", res("failed", "TestX/S01_works", ""), VerdictFailing},
		{"failing defect subtest", "TestX/E2E-05 defect 2344 billing collects twice", res("failed", "TestX/E2E-05_defect_2344_billing", ""), VerdictDefect},
		{"defect named only in the reported name", "a test", res("failed", "TestX/defect_2344", ""), VerdictDefect},
		{"defect held back by the gate", "TestX/defect 2344", res("skipped", "TestX/defect_2344", ""), VerdictDefect},
		{"known defect skip", "TestX/S01", res("skipped", "TestX/S01", "known_defect"), VerdictDefect},
		{"passing defect subtest means fixed", "TestX/defect 2344", res("passed", "TestX/defect_2344", ""), VerdictPassing},
		{"known gap skip", "TestX/S02", res("skipped", "TestX/S02", "known_gap"), VerdictKnownGap},
		{"playwright fixme", "d › S03 chips", res("skipped", "d › S03 chips", "fixme"), VerdictKnownGap},
		{"plain skip proves nothing", "TestX/S04", res("skipped", "TestX/S04", ""), VerdictNotRun},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := testVerdict(c.test, c.r); got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestIsDescendant(t *testing.T) {
	yes := [][2]string{
		{"TestENG465_E2E07/ENG-465-E2E-07 replayed event", "TestENG465_E2E07"},
		{"TestA/sub/deeper", "TestA/sub"},
		{"TestA/S01_works", "TestA"},
		{"ENG-481 Mind · Today › ENG-481 AC-14 · denied", "ENG-481 Mind · Today"},
		{"TestA/S01_two_words", "TestA"},
	}
	no := [][2]string{
		{"TestAB/sub", "TestA"},
		{"TestA", "TestA"},
		{"TestA", ""},
		{"Other › TestA", "TestA"},
	}
	for _, p := range yes {
		if !isDescendant(p[0], p[1]) {
			t.Errorf("isDescendant(%q, %q) = false, want true", p[0], p[1])
		}
	}
	for _, p := range no {
		if isDescendant(p[0], p[1]) {
			t.Errorf("isDescendant(%q, %q) = true, want false", p[0], p[1])
		}
	}
}

func ct(key, name string, r *CoverageResult) *coverageTest {
	return &coverageTest{Key: key, Name: name, Latest: r}
}

func TestCriterionVerdict(t *testing.T) {
	parentPass := ct("p", "TestENG465_S08", res("passed", "TestENG465_S08", ""))
	cases := []struct {
		name  string
		tests []*coverageTest
		want  string
		by    []string
	}{
		{"no linked test", nil, VerdictUntested, nil},
		{"linked, never run", []*coverageTest{ct("a", "TestA", nil)}, VerdictNotRun, []string{"a"}},
		{"passing", []*coverageTest{ct("a", "TestA", res("passed", "TestA", ""))}, VerdictPassing, []string{"a"}},
		{
			// The overstatement this exists for: the parent passes while the
			// subtest that proves this criterion stopped at a KNOWN GAP.
			"subtest known gap beats passing parent",
			[]*coverageTest{parentPass, ct("c", "TestENG465_S08/S08 no handover", res("skipped", "TestENG465_S08/S08_no_handover", "known_gap"))},
			VerdictKnownGap, []string{"c"},
		},
		{
			"subtest failure beats passing parent",
			[]*coverageTest{parentPass, ct("c", "TestENG465_S08/S08 x", res("failed", "TestENG465_S08/S08_x", ""))},
			VerdictFailing, []string{"c"},
		},
		{
			// A subtest beats its parent in both directions: a parent that
			// failed on another subtest does not fail this criterion.
			"passing subtest beats failing parent",
			[]*coverageTest{ct("p", "TestB", res("failed", "TestB", "")), ct("c", "TestB/S01 ok", res("passed", "TestB/S01_ok", ""))},
			VerdictPassing, []string{"c"},
		},
		{
			"subtest with no result does not hide its parent",
			[]*coverageTest{parentPass, ct("c", "TestENG465_S08/S08 renamed", nil)},
			VerdictPassing, []string{"p"},
		},
		{
			"playwright fixme beats a passing describe",
			[]*coverageTest{ct("p", "ENG-630 composer", res("passed", "ENG-630 composer", "")), ct("c", "ENG-630 composer › NFR-02 chips", res("skipped", "ENG-630 composer › NFR-02 chips", "fixme"))},
			VerdictKnownGap, []string{"c"},
		},
		{
			"failing defect subtest",
			[]*coverageTest{parentPass, ct("c", "TestENG465_S08/S08 defect 2348 foreign subscriber", res("failed", "TestENG465_S08/S08_defect_2348", ""))},
			VerdictDefect, []string{"c"},
		},
		{
			"independent tests: a failure outranks a pass",
			[]*coverageTest{ct("a", "TestA", res("passed", "TestA", "")), ct("b", "app › S01", res("failed", "app › S01", ""))},
			VerdictFailing, []string{"b"},
		},
		{
			"independent tests: a known gap outranks a pass",
			[]*coverageTest{ct("a", "TestA", res("passed", "TestA", "")), ct("b", "app › S01", res("skipped", "app › S01", "known_gap"))},
			VerdictKnownGap, []string{"b"},
		},
		{
			"independent tests: a pass outranks one that did not run",
			[]*coverageTest{ct("a", "TestA", nil), ct("b", "app › S01", res("passed", "app › S01", ""))},
			VerdictPassing, []string{"b"},
		},
		{
			"plain skip is not run",
			[]*coverageTest{ct("a", "TestA/S01", res("skipped", "TestA/S01", ""))},
			VerdictNotRun, []string{"a"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, by := criterionVerdict(c.tests)
			if got != c.want || !reflect.DeepEqual(by, c.by) {
				t.Fatalf("got %s %v, want %s %v", got, by, c.want, c.by)
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	byKey := map[string]*coverageTest{
		"pass": ct("pass", "TestA", res("passed", "TestA", "")),
		"gap":  ct("gap", "TestA/S02 x", res("skipped", "TestA/S02_x", "known_gap")),
	}
	specs := []coverageSpec{
		{Key: "ENG-1", Criteria: []coverageCriterion{
			{ID: "S01", Tests: []string{"pass"}},
			{ID: "S02", Tests: []string{"pass", "gap"}, gapCategory: "not_built"},
			{ID: "S03", Tests: []string{}, gapCategory: "awaiting_decision"},
			{ID: "S04", Tests: []string{}},
		}},
		{Key: "ENG-2", Criteria: []coverageCriterion{{ID: "AC-1", Tests: []string{"pass"}, gapCategory: "test_infra"}}},
	}
	sum := summarize(specs, byKey)
	if sum.Criteria != 5 || sum.Verdicts[VerdictPassing] != 2 || sum.Verdicts[VerdictKnownGap] != 1 || sum.Verdicts[VerdictUntested] != 2 {
		t.Fatalf("summary verdicts %+v", sum)
	}
	// A gap on a criterion that passes is not counted: it is not unproven.
	if sum.Gaps["not_built"] != 1 || sum.Gaps["awaiting_decision"] != 1 || sum.Unexplained != 1 || sum.Gaps["test_infra"] != 0 || len(sum.Gaps) != 5 {
		t.Fatalf("summary gaps %+v", sum.Gaps)
	}
	if specs[0].Summary.Verdicts[VerdictPassing] != 1 || specs[0].Summary.Verdicts[VerdictUntested] != 2 || specs[1].Summary.Verdicts[VerdictPassing] != 1 {
		t.Fatalf("spec verdicts %+v %+v", specs[0].Summary.Verdicts, specs[1].Summary.Verdicts)
	}
	s02 := specs[0].Criteria[1]
	if s02.Verdict != VerdictKnownGap || !reflect.DeepEqual(s02.DecidedBy, []string{"gap"}) {
		t.Fatalf("S02 = %s %v", s02.Verdict, s02.DecidedBy)
	}
	if s02.VerdictFrom == nil || s02.VerdictFrom.TestKey != "gap" || s02.VerdictFrom.Status != "skipped" || s02.VerdictFrom.SpecName != "TestA/S02_x" {
		t.Fatalf("S02 verdict_from = %+v", s02.VerdictFrom)
	}
	if s02.Gap == nil || s02.Gap.Category != "not_built" || specs[0].Criteria[3].Gap != nil {
		t.Fatalf("gap objects %+v %+v", s02.Gap, specs[0].Criteria[3].Gap)
	}
	if specs[0].Criteria[2].VerdictFrom != nil {
		t.Fatalf("untested criterion has verdict_from %+v", specs[0].Criteria[2].VerdictFrom)
	}
	for _, v := range Verdicts {
		if _, ok := specs[1].Summary.Verdicts[v]; !ok {
			t.Fatalf("spec verdicts lack %s", v)
		}
	}
}

func TestImportRejectsUnknownGapCategory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{
		`{"specs":[{"key":"ENG-1","criteria":[{"id":"S01","gap_category":"later"}]}],"tests":[]}`,
		`{"specs":[{"key":"ENG-1","criteria":[{"id":"S01","gap":{"category":"later","reason":"r"}}]}],"tests":[]}`,
	} {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/v1/requirements", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		NewCoverageHandler(nil, nil).importRegistry(ctx)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unknown gap category later") {
			t.Fatalf("got %d %s", w.Code, w.Body.String())
		}
	}
}

func TestNormalizeGap(t *testing.T) {
	cr := registryCriterion{ID: "S01", GapReason: " flat wins ", Gap: &registryGap{Category: "test_infra", Reason: "nested", Pathway: "add a route", Ticket: "OllyInsurance/olly#1", Source: "x_test.go:9"}}
	cr.normalizeGap()
	if cr.GapCategory != "test_infra" || cr.GapReason != "flat wins" || cr.GapPathway != "add a route" || cr.GapTicket != "OllyInsurance/olly#1" || cr.GapSource != "x_test.go:9" {
		t.Fatalf("normalizeGap = %+v", cr)
	}
	for _, c := range append([]string{""}, GapCategories...) {
		if !validGapCategory(c) {
			t.Fatalf("%q should be valid", c)
		}
	}
}
