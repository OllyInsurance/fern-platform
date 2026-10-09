package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestLinkAncestors(t *testing.T) {
	byKey := map[string]*coverageTest{
		"a": {Key: "a"}, "b": {Key: "b", Parent: "a"}, "c": {Key: "c", Parent: "b"},
		"x": {Key: "x", Parent: "y"}, "y": {Key: "y", Parent: "x"}, // a cycle must not hang
		"o": {Key: "o", Parent: "missing"},
	}
	linkAncestors(byKey)
	if !byKey["c"].ancestors["a"] || !byKey["c"].ancestors["b"] || len(byKey["a"].ancestors) != 0 || !byKey["o"].ancestors["missing"] {
		t.Fatalf("ancestors c=%v a=%v o=%v", byKey["c"].ancestors, byKey["a"].ancestors, byKey["o"].ancestors)
	}
	if !byKey["c"].isUnder(byKey["a"]) || byKey["a"].isUnder(byKey["c"]) {
		t.Fatal("isUnder")
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
			// The registry's parent link decides, whatever the names say.
			"subtest by parent link beats its parent",
			[]*coverageTest{parentPass, {Key: "c", Name: "ENG-465 S08 handover", Parent: "p", Latest: res("skipped", "x", "known_gap")}},
			VerdictKnownGap, []string{"c"},
		},
		{
			"grandchild by parent links beats its grandparent",
			[]*coverageTest{parentPass, {Key: "g", Name: "deep", Parent: "mid", ancestors: map[string]bool{"mid": true, "p": true}, Latest: res("failed", "deep", "")}},
			VerdictFailing, []string{"g"},
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

// The Go defects pass decides a defect test: failed there means the defect
// still reproduces, passed means it is fixed; the gate's skip never reads as
// a known gap.
func TestDefectsPassDecidesDefectTests(t *testing.T) {
	gateSkip := res("skipped", "TestX/defect_2348_foreign_subscriber", "known_gap")
	cases := []struct {
		name    string
		test    *coverageTest
		want    string
		fromDef bool
	}{
		{"still reproduces", &coverageTest{Key: "k", Name: "TestX/defect 2348 foreign subscriber", Latest: gateSkip,
			DefectsLatest: res("failed", "defects/TestX/defect_2348_foreign_subscriber", "")}, VerdictDefect, true},
		{"fixed", &coverageTest{Key: "k", Name: "TestX/defect 2348 foreign subscriber", Latest: gateSkip,
			DefectsLatest: res("passed", "defects/TestX/defect_2348_foreign_subscriber", "")}, VerdictPassing, true},
		{"defects pass not run: the gate skip is the defect", &coverageTest{Key: "k", Name: "TestX/defect 2348 foreign subscriber", Latest: gateSkip},
			VerdictDefect, false},
		{"a passing gate parent is not decided by its defects-pass run", &coverageTest{Key: "k", Name: "TestX", Latest: res("passed", "TestX", ""),
			DefectsLatest: res("failed", "defects/TestX", "")}, VerdictPassing, false},
		{"only the defects pass ran it", &coverageTest{Key: "k", Name: "TestY/S02 clip", DefectsLatest: res("failed", "defects/TestY/S02_clip", "")},
			VerdictDefect, true},
		{"a sibling the gate held back for a defect", &coverageTest{Key: "k", Name: "TestY/S02 clip", Latest: res("skipped", "TestY/S02_clip", "known_defect"),
			DefectsLatest: res("failed", "defects/TestY/S02_clip", "")}, VerdictDefect, true},
		{"a known-gap sibling is not decided by the defects pass", &coverageTest{Key: "k", Name: "TestZ/four and five digits", Latest: res("skipped", "TestZ/four_and_five_digits", "known_gap"),
			DefectsLatest: res("failed", "defects/TestZ/four_and_five_digits", "")}, VerdictKnownGap, false},
		{"a plain-skipped sibling is not decided by the defects pass", &coverageTest{Key: "k", Name: "TestZ/four and five digits", Latest: res("skipped", "TestZ/four_and_five_digits", ""),
			DefectsLatest: res("failed", "defects/TestZ/four_and_five_digits", "")}, VerdictNotRun, false},
		{"a fixme sibling is not decided by the defects pass", &coverageTest{Key: "k", Name: "TestZ/clip", Latest: res("skipped", "TestZ/clip", "fixme"),
			DefectsLatest: res("passed", "defects/TestZ/clip", "")}, VerdictKnownGap, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, r := c.test.outcome()
			if v != c.want || (r == c.test.DefectsLatest) != c.fromDef {
				t.Fatalf("got %s from %+v, want %s (from defects pass: %v)", v, r, c.want, c.fromDef)
			}
		})
	}
	// In a criterion: a subtest decided by the defects pass beats its parent.
	parent := ct("p", "TestX", res("passed", "TestX", ""))
	child := cases[0].test
	if v, by := criterionVerdict([]*coverageTest{parent, child}); v != VerdictDefect || by[0] != "k" {
		t.Fatalf("criterion = %s %v", v, by)
	}
}

// A fixed defect drops its helpers.Defect marker and keeps its name, so the
// gate runs it and the defects pass never runs it again. A gate run that ran
// the test after the defects pass decides it.
func TestNewerGateRunBeatsStaleDefectsPass(t *testing.T) {
	now := time.Now()
	old := res("failed", "defects/TestX/defect_2371", "")
	old.StartTime = now.Add(-24 * time.Hour)
	gatePass := res("passed", "TestX/defect_2371", "")
	gatePass.StartTime = now
	gateFail := res("failed", "TestX/defect_2371", "")
	gateFail.StartTime = now
	olderGatePass := res("passed", "TestX/defect_2371", "")
	olderGatePass.StartTime = now.Add(-48 * time.Hour)
	newSkip := res("skipped", "TestX/defect_2371", "known_defect")
	newSkip.StartTime = now
	for _, c := range []struct {
		name    string
		latest  *CoverageResult
		want    string
		fromDef bool
	}{
		{"a newer gate pass decides: the marker is gone", gatePass, VerdictPassing, false},
		{"a newer gate failure is still the defect", gateFail, VerdictDefect, false},
		{"an older gate pass loses to the defects pass", olderGatePass, VerdictDefect, true},
		{"a newer gate skip leaves it to the defects pass", newSkip, VerdictDefect, true},
	} {
		tt := &coverageTest{Key: "k", Name: "TestX/defect 2371", Latest: c.latest, DefectsLatest: old}
		if v, r := tt.outcome(); v != c.want || (r == old) != c.fromDef {
			t.Errorf("%s: got %s from %+v, want %s (from defects pass: %v)", c.name, v, r, c.want, c.fromDef)
		}
	}
}

// Defects-pass rows never match a gate lookup, and gate rows never match a
// defects lookup, even with a pattern that would catch both.
func TestResultIndexKeepsDefectsPassApart(t *testing.T) {
	now := time.Now()
	rows := []specRunRow{
		{ProjectID: "p", SpecName: "defects/TestX/defect_1", Status: "failed", TestRunID: 2, StartTime: now, DefectsPass: true},
		{ProjectID: "p", SpecName: "TestX/defect_1", Status: "skipped", TestRunID: 1, StartTime: now.Add(-time.Hour)},
		{ProjectID: "p", SpecName: "TestX", Status: "passed", TestRunID: 1, StartTime: now.Add(-time.Hour)},
	}
	var gate, defects resultIndex
	gate.rows, defects.rows = rows, rows
	gate.init(func(r specRunRow) bool { return !r.DefectsPass && !strings.HasPrefix(r.SpecName, GoDefectsPrefix) })
	defects.init(func(r specRunRow) bool { return r.DefectsPass || strings.HasPrefix(r.SpecName, GoDefectsPrefix) })

	if r := gate.match("p", "TestX/defect 1", "exact", ""); r == nil || r.Status != "skipped" {
		t.Fatalf("gate exact = %+v", r)
	}
	if r := gate.match("p", "defect_1$", "regex", ""); r == nil || r.Status != "skipped" || r.TestRunID != 1 {
		t.Fatalf("gate regex picked a defects-pass row: %+v", r)
	}
	if r := defects.match("p", "defects/TestX/defect 1", "exact", ""); r == nil || r.Status != "failed" {
		t.Fatalf("defects exact = %+v", r)
	}
	if r := defects.match("p", "TestX", "exact", ""); r != nil {
		t.Fatalf("defects lookup matched a gate row: %+v", r)
	}
}

// Each environment fault class, from the failures of the 2026-10-09 replica
// runs, is inconclusive; a product failure next to them stays failed.
func TestEnvFault(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		// gateway 502/503/504
		{"gateway error page", `identity_pin_change_test.go:189: CHECK FAIL Equal want=400 got=502 msg=server must refuse "12a456"; body=<html> <head><title>502 Bad Gateway</title></head>`, "gateway 5xx"},
		{"gateway status in a Go check", `eng495_address_lookup_test.go:697: CHECK FAIL Equal want=200 got=502 msg=the lookup is up now: {"error":"address provider error"}`, "gateway 502"},
		{"gateway status in a Playwright assertion", "Error: the gateway lookup of B1 1DH answers\n\x1b[2mexpect(\x1b[22mreceived).toBe(expected)\nExpected: \x1b[32m200\x1b[39m\nReceived: \x1b[31m502\x1b[39m", "gateway 502"},
		{"503 page", "Error: GET /healthz: 503 Service Temporarily Unavailable", "gateway 5xx"},
		{"504 status", "Expected: 200\nReceived: 504", "gateway 504"},
		// connection refused or reset
		{"connection refused", `Post "http://10.20.0.100:9080/identity/me/pin": dial tcp 10.20.0.100:9080: connect: connection refused`, "connection refused or reset"},
		{"econnreset", "Error: apiRequestContext.post: read ECONNRESET", "connection refused or reset"},
		{"browser connection refused", "Error: page.goto: net::ERR_CONNECTION_REFUSED at https://member--x.dev.hiolly.com/", "connection refused or reset"},
		// DNS
		{"no such host", `dial tcp: lookup api--x.dev.hiolly.com: no such host`, "dns failure"},
		{"enotfound", "Error: getaddrinfo ENOTFOUND api--x.dev.hiolly.com", "dns failure"},
		// TLS
		{"x509", `tls: failed to verify certificate: x509: certificate signed by unknown authority`, "tls error"},
		{"browser cert", "Error: page.goto: net::ERR_CERT_DATE_INVALID", "tls error"},
		// a route missing from the gateway (APISIX), not from the product
		{"apisix route missing", `CHECK FAIL Equal want=200 got=404 msg=POST /me/pin; body={"error_msg":"404 Route Not Found"}`, "gateway route missing"},
		// replica health
		{"replica health check", "replica rjr2026100907531jp1 health check failed: identity not ready", "replica health check failed"},
		// a third-party dependency the environment cannot use
		{"env precondition", `eng495_address_lookup_test.go:371: PRECONDITION ENG-495: the address provider is not usable from this environment. enrollment answered 502`, "environment precondition"},

		// product failures stay failed
		{"product 404", `identity_pin_change_test.go:365: CHECK FAIL Equal want=200 got=404 msg=POST /me/pin should store the hashed PIN; body=404 page not found`, ""},
		{"playwright 404", "Error: identity accepted the pin\nExpected: 200\nReceived: 404", ""},
		{"assertion", "Error: the policy has a Schedule\nexpect(received).toBeTruthy()\nReceived: undefined", ""},
		{"timeout", "TimeoutError: page.waitForURL: Timeout 45000ms exceeded.", ""},
		{"product 500", `CHECK FAIL Equal want=200 got=500 msg=a DRAFT transaction is priced: {"error":"rating elements"}`, ""},
		{"expected a 5xx, got another", "Expected: 503\nReceived: 502", ""},
		{"expected a 5xx in Go", "CHECK FAIL Equal want=503 got=502 msg=x", ""},
		{"a 502 next to a product failure", "CHECK FAIL Equal want=200 got=502 msg=x\nCHECK FAIL True got=false msg=the event was published", ""},
		{"passing check with a 502", "CHECK PASS Equal want=502 got=502 msg=misconfiguration is visible\nCHECK FAIL True got=false msg=the banner shows", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := envFault(c.text)
			if c.want == "" && got != "" || c.want != "" && !strings.HasPrefix(got, c.want+": ") {
				t.Fatalf("envFault = %q, want class %q", got, c.want)
			}
		})
	}
}

// A run that marks its environment unhealthy makes every failure in it
// inconclusive.
func TestRunEnvFault(t *testing.T) {
	for health, want := range map[string]bool{"": false, "ok": false, "healthy": false, "unhealthy": true, "down": true} {
		if got := runEnvFault(health, "gateway 502 at start") != ""; got != want {
			t.Errorf("runEnvFault(%q) inconclusive = %v, want %v", health, got, want)
		}
	}
}

func envRows(rows ...specRunRow) (gate, defects *resultIndex) {
	gate, defects = &resultIndex{rows: rows}, &resultIndex{rows: rows}
	gate.init(func(r specRunRow) bool { return !r.DefectsPass && !strings.HasPrefix(r.SpecName, GoDefectsPrefix) })
	defects.init(func(r specRunRow) bool { return r.DefectsPass || strings.HasPrefix(r.SpecName, GoDefectsPrefix) })
	return gate, defects
}

const gateway502 = "Error: enrollment answers the live lookup\nExpected: 200\nReceived: 502"

// An inconclusive result never decides: the test keeps its previous decided
// result in the window and the criterion its verdict.
func TestInconclusiveKeepsPreviousVerdict(t *testing.T) {
	now := time.Now()
	gate, _ := envRows(
		specRunRow{ID: 3, ProjectID: "p", SpecName: "TestA", Status: "failed", TestRunID: 3, RunID: "r3", StartTime: now, Detail: gateway502},
		specRunRow{ID: 2, ProjectID: "p", SpecName: "TestA", Status: "failed", TestRunID: 2, RunID: "r2", StartTime: now.Add(-time.Hour), Detail: "x: connect: connection refused"},
		specRunRow{ID: 1, ProjectID: "p", SpecName: "TestA", Status: "passed", TestRunID: 1, RunID: "r1", StartTime: now.Add(-2 * time.Hour)},
	)
	r, inc := gate.lookup("p", "TestA", "exact", "")
	if r == nil || r.Status != "passed" || r.TestRunID != 1 || r.Runs != 1 || r.Passed != 1 {
		t.Fatalf("latest = %+v, want the passed run 1", r)
	}
	if inc.count != 2 || !strings.HasPrefix(inc.reason, "gateway 502: ") || inc.newest == nil || inc.newest.RunID != "r3" {
		t.Fatalf("inconclusive = %+v", inc)
	}
	a := ct("a", "TestA", r)
	if v, _ := criterionVerdict([]*coverageTest{a}); v != VerdictPassing {
		t.Fatalf("criterion = %s, want passing", v)
	}

	// The same for a known gap: a 502 does not turn it into a failure.
	gate, _ = envRows(
		specRunRow{ID: 2, ProjectID: "p", SpecName: "TestB", Status: "failed", TestRunID: 2, StartTime: now, Detail: gateway502},
		specRunRow{ID: 1, ProjectID: "p", SpecName: "TestB", Status: "skipped", TestRunID: 1, StartTime: now.Add(-time.Hour), Detail: "KNOWN GAP ENG-1 S01: no route"},
	)
	if r := gate.match("p", "TestB", "exact", ""); r == nil || testVerdict("TestB", r) != VerdictKnownGap {
		t.Fatalf("known gap lost to a 502: %+v", r)
	}
}

// With nothing decided earlier in the window, the criterion is not_run.
func TestInconclusiveAloneIsNotRun(t *testing.T) {
	gate, _ := envRows(specRunRow{ID: 1, ProjectID: "p", SpecName: "TestA", Status: "failed", TestRunID: 1, StartTime: time.Now(), Detail: gateway502})
	r, inc := gate.lookup("p", "TestA", "exact", "")
	if r != nil || inc.count != 1 {
		t.Fatalf("latest = %+v, inconclusive = %+v", r, inc)
	}
	if v, _ := criterionVerdict([]*coverageTest{ct("a", "TestA", r)}); v != VerdictNotRun {
		t.Fatalf("criterion = %s, want not_run", v)
	}
}

// A product failure decides, even in a run that also had environment faults.
func TestProductFailureStillDecides(t *testing.T) {
	now := time.Now()
	gate, _ := envRows(
		specRunRow{ID: 3, ProjectID: "p", SpecName: "TestA", Status: "failed", TestRunID: 2, StartTime: now, Detail: "Expected: 200\nReceived: 404"},
		specRunRow{ID: 2, ProjectID: "p", SpecName: "TestA", Status: "failed", TestRunID: 2, StartTime: now, Detail: gateway502},
		specRunRow{ID: 1, ProjectID: "p", SpecName: "TestA", Status: "passed", TestRunID: 1, StartTime: now.Add(-time.Hour)},
	)
	r, inc := gate.lookup("p", "TestA", "exact", "")
	if r == nil || r.Status != "failed" || r.TestRunID != 2 || inc.count != 0 {
		t.Fatalf("latest = %+v, inconclusive = %+v", r, inc)
	}
}

// The run-level marker makes a failure inconclusive whatever it says.
func TestRunMarkerMakesFailureInconclusive(t *testing.T) {
	now := time.Now()
	gate, _ := envRows(
		specRunRow{ID: 2, ProjectID: "p", SpecName: "TestA", Status: "failed", TestRunID: 2, StartTime: now, Detail: "Expected: 200\nReceived: 404", EnvHealth: "unhealthy", EnvReason: "identity not ready"},
		specRunRow{ID: 1, ProjectID: "p", SpecName: "TestA", Status: "passed", TestRunID: 1, StartTime: now.Add(-time.Hour)},
	)
	r, inc := gate.lookup("p", "TestA", "exact", "")
	if r == nil || r.Status != "passed" || inc.count != 1 || inc.reason != "environment marked unhealthy: identity not ready" {
		t.Fatalf("latest = %+v, inconclusive = %+v", r, inc)
	}
}

// A defects-pass run that hit a 502 does not turn a defect into a fix or a
// fix into a defect: the earlier defects-pass result stands.
func TestInconclusiveDefectsPass(t *testing.T) {
	now := time.Now()
	_, defects := envRows(
		specRunRow{ID: 2, ProjectID: "p", SpecName: "defects/TestA/defect_1", Status: "failed", TestRunID: 2, StartTime: now, DefectsPass: true,
			Detail: "CHECK FAIL Equal want=200 got=502 msg=POST /me/pin; body=<title>502 Bad Gateway</title>"},
		specRunRow{ID: 1, ProjectID: "p", SpecName: "defects/TestA/defect_1", Status: "passed", TestRunID: 1, StartTime: now.Add(-time.Hour), DefectsPass: true},
	)
	d, inc := defects.lookup("p", "defects/TestA/defect 1", "exact", "")
	if d == nil || d.Status != "passed" || inc.count != 1 {
		t.Fatalf("defects latest = %+v, inconclusive = %+v", d, inc)
	}
	tc := &coverageTest{Key: "a", Name: "TestA/defect 1", DefectsLatest: d}
	if v, _ := tc.outcome(); v != VerdictPassing {
		t.Fatalf("outcome = %s, want passing", v)
	}
}

// In production a failure's full error is fetched lazily: the lookup queues
// it and answers nothing until it is classified.
func TestLookupWaitsForClassification(t *testing.T) {
	now := time.Now()
	gate, _ := envRows(
		specRunRow{ID: 3, ProjectID: "p", SpecName: "TestA", Status: "failed", TestRunID: 3, StartTime: now},
		specRunRow{ID: 2, ProjectID: "p", SpecName: "TestA", Status: "failed", TestRunID: 2, StartTime: now.Add(-time.Hour)},
		specRunRow{ID: 1, ProjectID: "p", SpecName: "TestA", Status: "passed", TestRunID: 1, StartTime: now.Add(-2 * time.Hour)},
	)
	gate.faults, gate.pending = map[int64]string{}, map[int64]bool{}
	if r, _ := gate.lookup("p", "TestA", "exact", ""); r != nil {
		t.Fatalf("answered before classification: %+v", r)
	}
	if !gate.pending[3] || !gate.pending[2] {
		t.Fatalf("pending = %v, want both failures queued", gate.pending)
	}
	gate.faults[3], gate.faults[2] = "gateway 502: x", "dns failure: y"
	gate.pending = map[int64]bool{}
	r, inc := gate.lookup("p", "TestA", "exact", "")
	if r == nil || r.TestRunID != 1 || inc.count != 2 || len(gate.pending) != 0 {
		t.Fatalf("latest = %+v, inconclusive = %+v, pending = %v", r, inc, gate.pending)
	}
}
