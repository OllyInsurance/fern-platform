package api

import (
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

func ptr(t time.Time) *time.Time { return &t }

func TestCIRunIDOfAndRunner(t *testing.T) {
	cases := []struct {
		tr   RunTestRun
		want string
	}{
		{RunTestRun{ID: 1, Metadata: map[string]interface{}{"ci_run_id": "36953421037"}}, "36953421037"},
		{RunTestRun{ID: 2, Metadata: map[string]interface{}{"ci_run_id": float64(42)}}, "42"},
		{RunTestRun{ID: 3, Metadata: map[string]interface{}{"build_url": "https://github.com/OllyInsurance/olly/actions/runs/36953421037/job/9"}}, "36953421037"},
		{RunTestRun{ID: 4, Metadata: map[string]interface{}{"build_url": "https://ci.example/x"}}, "tr-4"},
		{RunTestRun{ID: 5}, "tr-5"},
		{RunTestRun{ID: 6, Metadata: map[string]interface{}{"ci_run_id": "local-1790906000"}}, "local-1790906000"},
	}
	for _, c := range cases {
		if got := CIRunIDOf(c.tr); got != c.want {
			t.Errorf("CIRunIDOf(%d) = %q, want %q", c.tr.ID, got, c.want)
		}
	}
	if r := RunnerOf("local-1", nil); r != "manual" {
		t.Errorf("local- ci id: runner %q", r)
	}
	if r := RunnerOf("tr-9", []RunTestRun{{RunID: "local-77"}}); r != "manual" {
		t.Errorf("local- run id: runner %q", r)
	}
	if r := RunnerOf("123", []RunTestRun{{Metadata: map[string]interface{}{"runner": "manual"}}}); r != "manual" {
		t.Errorf("metadata runner=manual: runner %q", r)
	}
	if r := RunnerOf("123", []RunTestRun{{RunID: "998"}}); r != "github" {
		t.Errorf("github run: runner %q", r)
	}
}

func TestLaneOf(t *testing.T) {
	k, l := laneOf(RunTestRun{ProjectID: "olly-go-packages"})
	if k != "olly-go-packages" || l != "olly-go-packages" {
		t.Errorf("no lane: %q %q", k, l)
	}
	k, l = laneOf(RunTestRun{ProjectID: "olly-balance", Metadata: map[string]interface{}{"lane": "pytest"}})
	if k != "pytest · olly-balance" || l != "pytest" {
		t.Errorf("lane: %q %q", k, l)
	}
}

func TestSplitTestName(t *testing.T) {
	cases := map[string][]string{
		"TestAtomicity_Billing/E1_HappyPath": {"TestAtomicity_Billing", "E1_HappyPath"},
		"TestA/B/C":                          {"TestA", "B", "C"},
		"TestA":                              {"TestA"},
		"mind › mind/eng481.spec.ts › Today › x": {"mind", "mind/eng481.spec.ts", "Today", "x"},
		"Charges [charges] › list view renders":  {"Charges [charges]", "list view renders"},
		"test_band[a/b-4-minimal]":               {"test_band[a/b-4-minimal]"},
		"defects/TestX/sub":                      {"defects", "TestX", "sub"},
	}
	for in, want := range cases {
		if got := SplitTestName(in); !reflect.DeepEqual(got, want) {
			t.Errorf("SplitTestName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassifySpec(t *testing.T) {
	cases := []struct {
		name, status, detail string
		defectsPass          bool
		want                 string
	}{
		{"TestX", "passed", "", false, ClassPassed},
		{"TestX", "failed", "boom", false, ClassFailed},
		{"TestX/defect_olly2348", "failed", "boom", false, ClassDefect},
		{"TestX/S01", "failed", "boom", true, ClassDefect},
		{"defects/TestX/S01", "failed", "", false, ClassDefect},
		{"defects/TestX/S01", "passed", "", false, ClassPassed},
		{"TestX/S01", "skipped", "x_test.go:12: KNOWN GAP ENG-1: no route", false, ClassKnownGap},
		{"a › b", "skipped", `{"annotations":[{"type":"fixme"}]}`, false, ClassKnownGap},
		{"TestX/S01", "skipped", "KNOWN DEFECT olly#2348", false, ClassDefect},
		{"a › defect app#511: mic", "skipped", "", false, ClassDefect},
		{"TestX/S01", "skipped", "skipped 3s", false, ClassSkipped},
	}
	for _, c := range cases {
		if got := ClassifySpec(c.name, c.status, c.detail, c.defectsPass); got != c.want {
			t.Errorf("ClassifySpec(%q, %q, %q, %v) = %q, want %q", c.name, c.status, c.detail, c.defectsPass, got, c.want)
		}
	}
}

// sampleRun is a CI run with a Go e2e lane (setup phases, two buckets), a
// go-unit project reported as two test runs, and a Playwright lane.
func sampleRun() RunSource {
	meta := func(kv ...string) map[string]interface{} {
		m := map[string]interface{}{"ci_run_id": "100", "ci_workflow": "CI", "build_url": "https://github.com/o/r/actions/runs/100/job/7"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	return RunSource{
		CIRunID: "100",
		TestRuns: []RunTestRun{
			{ID: 1, ProjectID: "olly-platform-e2e", Branch: "main", CommitSHA: "abc", StartTime: at(0), DurationMS: 600000, Metadata: meta("lane", "e2e")},
			{ID: 2, ProjectID: "olly-go-packages", Branch: "main", CommitSHA: "abc", StartTime: at(10), DurationMS: 1000, Metadata: meta()},
			{ID: 3, ProjectID: "olly-go-packages", Branch: "main", CommitSHA: "abc", StartTime: at(12), DurationMS: 2000, Metadata: meta()},
			{ID: 4, ProjectID: "olly-journeys", Branch: "main", CommitSHA: "abc", StartTime: at(20), DurationMS: 0, Metadata: meta("lane", "journeys")},
		},
		Suites: []RunSuiteRow{
			{ID: 10, TestRunID: 1, Name: "ci-phases", Status: "passed", StartTime: at(0), DurationMS: 300000},
			{ID: 11, TestRunID: 1, Name: "github.com/olly/e2e · bucket0", Status: "failed", StartTime: at(300), DurationMS: 100000},
			{ID: 12, TestRunID: 1, Name: "github.com/olly/e2e · bucket1", Status: "passed", StartTime: at(300), DurationMS: 0},
			{ID: 13, TestRunID: 1, Name: "github.com/olly/e2e/adapters", Status: "unknown", StartTime: at(300)},
			{ID: 20, TestRunID: 2, Name: "github.com/olly/access", Status: "passed", StartTime: at(10), DurationMS: 1000},
			{ID: 21, TestRunID: 3, Name: "github.com/olly/db", Status: "passed", StartTime: at(12), DurationMS: 2000},
			{ID: 30, TestRunID: 4, Name: "Journey: mind", Status: "failed", StartTime: at(20)},
		},
		Specs: []RunSpecRow{
			{ID: 1, SuiteRunID: 10, Name: "00 provisioning olly-r-x", Status: "passed", StartTime: at(0), DurationMS: 3000},
			{ID: 2, SuiteRunID: 10, Name: "01 waiting for SSH", Status: "passed", StartTime: at(3), DurationMS: 200000},
			// bucket0: a parent with a passing and a failing subtest, a defect subtest, a known gap
			{ID: 3, SuiteRunID: 11, Name: "TestClaims", Status: "failed", StartTime: at(300), DurationMS: 50000, Message: "subtest failed"},
			{ID: 4, SuiteRunID: 11, Name: "TestClaims/S01_submit", Status: "passed", StartTime: at(300), DurationMS: 20000},
			{ID: 5, SuiteRunID: 11, Name: "TestClaims/S02_approve", Status: "failed", StartTime: at(320), DurationMS: 30000, Message: "want 200 got 500"},
			{ID: 6, SuiteRunID: 11, Name: "TestClaims/S03_defect_olly2348", Status: "failed", StartTime: at(350), DurationMS: 100},
			{ID: 7, SuiteRunID: 11, Name: "TestBilling/S01", Status: "skipped", StartTime: at(360), DurationMS: 0, Message: "KNOWN GAP ENG-9", Detail: "KNOWN GAP ENG-9: no invoices"},
			// bucket1: no durations recorded at any level
			{ID: 8, SuiteRunID: 12, Name: "TestCare/S01", Status: "passed", StartTime: at(400), EndTime: ptr(at(410))},
			{ID: 9, SuiteRunID: 12, Name: "TestCare/S02", Status: "passed", StartTime: at(410), EndTime: ptr(at(430))},
			{ID: 10, SuiteRunID: 20, Name: "TestAccess", Status: "passed", StartTime: at(10), DurationMS: 900},
			{ID: 11, SuiteRunID: 21, Name: "TestDB", Status: "passed", StartTime: at(12), DurationMS: 1900},
			{ID: 12, SuiteRunID: 30, Name: "mind › mind/eng481.spec.ts › Today › opens on Today", Status: "passed", StartTime: at(20), DurationMS: 1000},
			{ID: 13, SuiteRunID: 30, Name: "mind › mind/eng481.spec.ts › Today › defect app#505: tab", Status: "failed", StartTime: at(21), DurationMS: 33000},
			{ID: 14, SuiteRunID: 30, Name: "mind › mind/eng481.spec.ts › Today › reflect", Status: "failed", StartTime: at(60), DurationMS: 7000, Message: "timeout"},
		},
	}
}

func names(ns []*RunNode) []string {
	out := []string{}
	for _, n := range ns {
		out = append(out, n.Name)
	}
	return out
}

func TestBuildRunGrouping(t *testing.T) {
	sum, root := BuildRun(sampleRun())
	if got, want := names(root.Children), []string{"setup", "e2e · olly-platform-e2e", "olly-go-packages", "journeys · olly-journeys"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lanes = %q, want %q", got, want)
	}
	if sum.URL != "https://github.com/o/r/actions/runs/100" || sum.Workflow != "CI" || sum.Runner != "github" || sum.SHA != "abc" || sum.Branch != "main" {
		t.Errorf("summary fields: %+v", sum)
	}
	gp := root.index["olly-go-packages"]
	if !reflect.DeepEqual(gp.TestRunIDs, []uint{2, 3}) || gp.Lane != "olly-go-packages" {
		t.Errorf("go-packages lane test runs %v lane %q", gp.TestRunIDs, gp.Lane)
	}
	// two test runs: the lane spans both (10s .. 14s)
	if gp.DurationMS != 4000 {
		t.Errorf("go-packages duration %d, want 4000", gp.DurationMS)
	}
	setup := root.index["setup"]
	if got := names(setup.Children); !reflect.DeepEqual(got, []string{"e2e · olly-platform-e2e"}) {
		t.Errorf("setup buckets %q", got)
	}
	ph := setup.Children[0]
	if ph.Children[0].Kind != KindPhase || ph.Counts.Passed != 2 {
		t.Errorf("phases: kind %q counts %+v", ph.Children[0].Kind, ph.Counts)
	}
	e2e := root.index["e2e · olly-platform-e2e"]
	if got := names(e2e.Children); !reflect.DeepEqual(got, []string{"github.com/olly/e2e · bucket0", "github.com/olly/e2e · bucket1"}) {
		t.Errorf("e2e buckets %q (the empty package is dropped, ci-phases moved to setup)", got)
	}
	if e2e.DurationMS != 600000 {
		t.Errorf("e2e lane keeps its test run's duration: %d", e2e.DurationMS)
	}
}

func TestBuildRunTreeAndCounts(t *testing.T) {
	sum, root := BuildRun(sampleRun())
	b0 := root.index["e2e · olly-platform-e2e"].index["github.com/olly/e2e · bucket0"]
	claims := b0.index["TestClaims"]
	if claims.Kind != KindTest || claims.index["S02_approve"].Kind != KindSubtest {
		t.Fatalf("kinds: %q %q", claims.Kind, claims.index["S02_approve"].Kind)
	}
	// The failing parent is carried by its failing subtest, not counted twice.
	if want := (RunCounts{Total: 3, Passed: 1, Failed: 1, Defect: 1}); claims.Counts != want {
		t.Errorf("TestClaims counts %+v, want %+v", claims.Counts, want)
	}
	if claims.Status != ClassFailed || claims.DurationMS != 50000 {
		t.Errorf("TestClaims status %q duration %d", claims.Status, claims.DurationMS)
	}
	// A test with no own row takes its child's class and span.
	billing := b0.index["TestBilling"]
	if billing.Status != StatusPartial || billing.Counts.KnownGap != 1 || billing.index["S01"].Status != ClassKnownGap {
		t.Errorf("TestBilling %q %+v", billing.Status, billing.Counts)
	}
	if m := billing.index["S01"].Message; m != "KNOWN GAP ENG-9: no invoices" {
		t.Errorf("known gap message %q", m)
	}
	// Duration falls back to the span of the children at every level.
	b1 := root.index["e2e · olly-platform-e2e"].index["github.com/olly/e2e · bucket1"]
	care := b1.index["TestCare"]
	if care.DurationMS != 30000 || b1.DurationMS != 30000 {
		t.Errorf("span fallback: TestCare %d bucket1 %d, want 30000", care.DurationMS, b1.DurationMS)
	}
	if care.index["S02"].DurationMS != 20000 {
		t.Errorf("end_time duration %d", care.index["S02"].DurationMS)
	}
	// Playwright title path
	j := root.index["journeys · olly-journeys"].index["Journey: mind"]
	today := NodeAtPath(j, []string{"mind", "mind/eng481.spec.ts", "Today"})
	if today == nil || today.Kind != KindSubtest || len(today.Children) != 3 {
		t.Fatalf("playwright path: %+v", today)
	}
	if want := (RunCounts{Total: 3, Passed: 1, Failed: 1, Defect: 1}); today.Counts != want {
		t.Errorf("Today counts %+v", today.Counts)
	}
	// The journeys test run recorded no duration: span of its specs (20s .. 67s).
	if d := root.index["journeys · olly-journeys"].DurationMS; d != 47000 {
		t.Errorf("journeys lane %d, want 47000", d)
	}
	want := RunCounts{Total: 2 + 3 + 1 + 2 + 2 + 3, Passed: 2 + 1 + 2 + 2 + 1, Failed: 2, KnownGap: 1, Defect: 2}
	if sum.Counts != want || sum.Status != ClassFailed {
		t.Errorf("run counts %+v status %q, want %+v failed", sum.Counts, sum.Status, want)
	}
	if len(sum.Lanes) != 4 || sum.Lanes[0].Key != "setup" || sum.Lanes[0].Status != ClassPassed {
		t.Errorf("summary lanes %+v", sum.Lanes)
	}
	// Node ids are stable and distinct.
	_, again := BuildRun(sampleRun())
	if again.index["olly-go-packages"].ID != root.index["olly-go-packages"].ID {
		t.Error("node ids are not stable")
	}
	if FindNode(root, care.ID) != care {
		t.Error("FindNode")
	}
}

func TestParentFailingAloneIsCounted(t *testing.T) {
	src := RunSource{CIRunID: "1",
		TestRuns: []RunTestRun{{ID: 1, ProjectID: "p", StartTime: at(0)}},
		Suites:   []RunSuiteRow{{ID: 1, TestRunID: 1, Name: "s", StartTime: at(0)}},
		Specs: []RunSpecRow{
			{SuiteRunID: 1, Name: "TestX", Status: "failed", StartTime: at(0), DurationMS: 10, Message: "cleanup failed"},
			{SuiteRunID: 1, Name: "TestX/a", Status: "passed", StartTime: at(0), DurationMS: 5},
		}}
	_, root := BuildRun(src)
	x := root.Children[0].Children[0].Children[0]
	if want := (RunCounts{Total: 2, Passed: 1, Failed: 1}); x.Counts != want || x.Status != ClassFailed || x.Message != "cleanup failed" {
		t.Errorf("TestX %+v %q %q", x.Counts, x.Status, x.Message)
	}
}

func TestRunningAndPartialStatus(t *testing.T) {
	src := RunSource{CIRunID: "1",
		TestRuns: []RunTestRun{{ID: 1, ProjectID: "p", StartTime: at(0), Status: "running"}},
		Suites:   []RunSuiteRow{{ID: 1, TestRunID: 1, Name: "s", StartTime: at(0)}},
		Specs:    []RunSpecRow{{SuiteRunID: 1, Name: "T", Status: "passed", StartTime: at(0)}}}
	sum, _ := BuildRun(src)
	if sum.Status != StatusRunning || sum.Lanes[0].Status != StatusRunning {
		t.Errorf("running: %q %q", sum.Status, sum.Lanes[0].Status)
	}
	if s := statusFromCounts(RunCounts{Total: 2, Passed: 1, KnownGap: 1}); s != StatusPartial {
		t.Errorf("known gap: %q", s)
	}
	if s := statusFromCounts(RunCounts{Total: 2, Passed: 2}); s != ClassPassed {
		t.Errorf("passed: %q", s)
	}
	if s := statusFromCounts(RunCounts{Total: 1, Skipped: 1}); s != ClassSkipped {
		t.Errorf("skipped: %q", s)
	}
}

func TestDepthAndPrune(t *testing.T) {
	_, root := BuildRun(sampleRun())
	TruncateDepth(root, 1)
	for _, ln := range root.Children {
		if len(ln.Children) != 0 || ln.ChildCount == 0 || ln.Counts.Total == 0 {
			t.Errorf("depth 1: lane %q children %d child_count %d counts %+v", ln.Name, len(ln.Children), ln.ChildCount, ln.Counts)
		}
	}

	_, root = BuildRun(sampleRun())
	if !PruneByClass(root, []string{ClassFailed}) {
		t.Fatal("run has failures")
	}
	if got := names(root.Children); !reflect.DeepEqual(got, []string{"e2e · olly-platform-e2e", "journeys · olly-journeys"}) {
		t.Errorf("failed lanes %q", got)
	}
	claims := NodeAtPath(root.Children[0], []string{"github.com/olly/e2e · bucket0", "TestClaims"})
	if claims == nil || !reflect.DeepEqual(names(claims.Children), []string{"S02_approve"}) {
		t.Errorf("failing path under TestClaims: %v", claims)
	}
	if NodeAtPath(root.Children[0], []string{"github.com/olly/e2e · bucket1"}) != nil {
		t.Error("passing bucket kept")
	}
	// counts are kept on pruned nodes
	if claims.Counts.Total != 3 {
		t.Errorf("pruned counts %+v", claims.Counts)
	}
}

func TestCompareRuns(t *testing.T) {
	a := sampleRun()
	b := sampleRun()
	// b: S02 fixed, S01_submit newly fails, TestAccess much slower, TestDB gone,
	// a new bucket appears, the setup phase text changes (not a diff).
	b.Specs[4].Status, b.Specs[4].Message = "passed", ""
	b.Specs[3].Status = "failed"
	b.Specs[9].DurationMS = 9000
	b.Suites[0].DurationMS = 300000
	b.Specs[0].Name = "00 provisioning olly-r-y"
	b.Specs = append(b.Specs[:10], b.Specs[11:]...)
	b.Suites = append(b.Suites, RunSuiteRow{ID: 40, TestRunID: 1, Name: "github.com/olly/e2e · bucket2", Status: "passed", StartTime: at(300), DurationMS: 5})
	b.Specs = append(b.Specs, RunSpecRow{ID: 40, SuiteRunID: 40, Name: "TestNew", Status: "passed", StartTime: at(300), DurationMS: 5},
		RunSpecRow{ID: 41, SuiteRunID: 40, Name: "TestNew/a", Status: "passed", StartTime: at(300), DurationMS: 5})
	_, ra := BuildRun(a)
	_, rb := BuildRun(b)
	diff := CompareRuns(ra, rb)
	type k struct{ change, last string }
	var got []k
	for _, d := range diff {
		got = append(got, k{d.Change, d.Path[len(d.Path)-1]})
	}
	want := []k{
		{ChangeNewFailure, "S01_submit"},
		{ChangeFixed, "S02_approve"},
		{ChangeRemoved, "github.com/olly/db"}, // its only test went: the highest removed node
		{ChangeAdded, "github.com/olly/e2e · bucket2"},
		{ChangeSlower, "TestAccess"},
		{ChangeFaster, "olly-go-packages"}, // the lane span lost the db package
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("diff:\n got %v\nwant %v", got, want)
	}
	for _, d := range diff {
		if d.Change == ChangeFixed && (d.A.Status != ClassFailed || d.B.Status != ClassPassed) {
			t.Errorf("fixed sides %+v %+v", d.A, d.B)
		}
		if d.Change == ChangeAdded && (d.A != nil || d.B == nil) {
			t.Errorf("added sides")
		}
	}
	if len(CompareRuns(ra, ra)) != 0 {
		t.Error("a run differs from itself")
	}
}

func TestBuildTestsForTrends(t *testing.T) {
	rows := []RunSpecRow{
		{ID: 1, Name: "TestAtomicity_Billing", Status: "passed", StartTime: at(0), DurationMS: 100},
		{ID: 2, Name: "TestAtomicity_Billing/E1", Status: "passed", StartTime: at(0), DurationMS: 60},
		{ID: 3, Name: "TestAtomicity_Care/E1", Status: "failed", StartTime: at(0), DurationMS: 60},
	}
	root := BuildTests(rows, nil)
	got := nodesAtDepth(root, 1)
	if len(got) != 2 || got[0].node.Name != "TestAtomicity_Billing" || got[0].node.DurationMS != 100 || got[1].node.Status != ClassFailed {
		t.Errorf("trend nodes %+v", got)
	}
}
