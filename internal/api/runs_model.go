package api

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A CI run is every Fern test run that shares metadata ci_run_id (the GitHub
// run id, or local-<ts> for a manual run from the box). A test run without one
// is its own CI run, "tr-<test_run_id>". The run is a tree:
//
//	run > lane > bucket (a suite run) > test (top-level spec name) > subtest ...
//
// Setup phases (the ci-phases suite every replica lane reports) are moved to a
// lane of their own, "setup", with one bucket per lane they set up.
//
// Every count in the tree comes from classifying spec runs the way the
// coverage verdicts do (see coverage_verdict.go), so a run's known gaps and
// defects agree with the coverage board.

// Node kinds.
const (
	KindRun     = "run"
	KindLane    = "lane"
	KindBucket  = "bucket"
	KindSuite   = "suite"
	KindTest    = "test"
	KindSubtest = "subtest"
	KindPhase   = "phase"
)

// Result classes of a spec run, and node statuses.
const (
	ClassPassed   = "passed"
	ClassFailed   = "failed"
	ClassSkipped  = "skipped"
	ClassKnownGap = "known_gap"
	ClassDefect   = "defect"

	StatusPartial = "partial" // nothing failed, but known gaps or defects are open
	StatusRunning = "running"
)

// SetupLane is the lane setup phases are moved to.
const SetupLane = "setup"

// RunsSummaryVersion changes whenever classification or grouping changes, so
// the summary tables are rebuilt on the next start.
const RunsSummaryVersion = 1

// RunCounts are the counts every node carries.
type RunCounts struct {
	Total    int `json:"total"`
	Passed   int `json:"passed"`
	Failed   int `json:"failed"`
	Skipped  int `json:"skipped"`
	KnownGap int `json:"known_gap"`
	Defect   int `json:"defect"`
}

func (c *RunCounts) addClass(class string) {
	c.Total++
	switch class {
	case ClassPassed:
		c.Passed++
	case ClassFailed:
		c.Failed++
	case ClassKnownGap:
		c.KnownGap++
	case ClassDefect:
		c.Defect++
	default:
		c.Skipped++
	}
}

func (c *RunCounts) add(o RunCounts) {
	c.Total += o.Total
	c.Passed += o.Passed
	c.Failed += o.Failed
	c.Skipped += o.Skipped
	c.KnownGap += o.KnownGap
	c.Defect += o.Defect
}

// get returns the count of one class.
func (c RunCounts) get(class string) int {
	switch class {
	case ClassPassed:
		return c.Passed
	case ClassFailed:
		return c.Failed
	case ClassSkipped:
		return c.Skipped
	case ClassKnownGap:
		return c.KnownGap
	case ClassDefect:
		return c.Defect
	}
	return 0
}

// statusFromCounts is an aggregate node's status: failed when anything
// failed, partial when nothing failed but a known gap or a defect is open,
// skipped when nothing ran, else passed.
func statusFromCounts(c RunCounts) string {
	switch {
	case c.Failed > 0:
		return ClassFailed
	case c.KnownGap+c.Defect > 0:
		return StatusPartial
	case c.Total > 0 && c.Passed == 0:
		return ClassSkipped
	}
	return ClassPassed
}

// --- source rows ---------------------------------------------------------------

// RunTestRun is one Fern test run of a CI run.
type RunTestRun struct {
	ID          uint
	ProjectID   string
	RunID       string
	Branch      string
	CommitSHA   string
	Status      string
	StartTime   time.Time
	EndTime     *time.Time
	DurationMS  int64
	Environment string
	Metadata    map[string]interface{}
}

// RunSuiteRow is one suite run.
type RunSuiteRow struct {
	ID         uint
	TestRunID  uint
	Name       string
	Status     string
	StartTime  time.Time
	EndTime    *time.Time
	DurationMS int64
}

// RunSpecRow is one spec run. Detail is the error message, description and
// metadata of a result that did not pass (what the gap markers are read from).
type RunSpecRow struct {
	ID         uint
	SuiteRunID uint
	Name       string
	Status     string
	StartTime  time.Time
	EndTime    *time.Time
	DurationMS int64
	Message    string
	Detail     string
}

func (m RunTestRun) meta(key string) string {
	if m.Metadata == nil {
		return ""
	}
	switch v := m.Metadata[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func endOf(start time.Time, end *time.Time, durMS int64) time.Time {
	if end != nil && !end.IsZero() && !end.Before(start) {
		return *end
	}
	return start.Add(time.Duration(durMS) * time.Millisecond)
}

// --- grouping ----------------------------------------------------------------

var reActionsRun = regexp.MustCompile(`/actions/runs/(\d+)`)

// CIRunIDOf is the CI run a test run belongs to: metadata ci_run_id, else the
// GitHub run id in metadata build_url (older reporters sent only that), else
// the test run alone ("tr-<id>").
func CIRunIDOf(tr RunTestRun) string {
	if id := tr.meta("ci_run_id"); id != "" {
		return id
	}
	if m := reActionsRun.FindStringSubmatch(tr.meta("build_url")); m != nil {
		return m[1]
	}
	return fmt.Sprintf("tr-%d", tr.ID)
}

// RunnerOf is "manual" for a run from the box (metadata runner=manual, or a
// local- run id), else "github".
func RunnerOf(ciRunID string, trs []RunTestRun) string {
	if strings.HasPrefix(ciRunID, "local-") {
		return "manual"
	}
	for _, tr := range trs {
		if strings.EqualFold(tr.meta("runner"), "manual") || strings.HasPrefix(tr.RunID, "local-") {
			return "manual"
		}
	}
	return "github"
}

// laneOf is a test run's lane name (metadata lane, else the project) and the
// lane key: the project alone, or "<lane> · <project>" so one lane name run by
// several projects (pytest) stays apart.
func laneOf(tr RunTestRun) (key, lane string) {
	lane = tr.meta("lane")
	if lane == "" || lane == tr.ProjectID {
		return tr.ProjectID, tr.ProjectID
	}
	return lane + " · " + tr.ProjectID, lane
}

// isSetupSuite says whether a suite run holds CI setup phases.
func isSetupSuite(project, suite string) bool {
	return project == "olly-ci-phases" || strings.Contains(suite, "ci-phases")
}

// --- classification ----------------------------------------------------------

// ClassifySpec is one spec run's class, by the rules of the coverage
// verdicts: a failed or skipped test named "defect" (or skipped as a KNOWN
// DEFECT) is a defect; a skip marked KNOWN GAP or fixme is a known gap; any
// other skip is a skip. In the Go defects pass (test run metadata
// pass=defects, names prefixed defects/) a failure means the defect still
// reproduces.
func ClassifySpec(name, status, detail string, defectsPass bool) string {
	defectNamed := reDefectName.MatchString(name)
	inDefects := defectsPass || strings.HasPrefix(name, GoDefectsPrefix)
	switch status {
	case "passed":
		return ClassPassed
	case "failed":
		if defectNamed || inDefects {
			return ClassDefect
		}
		return ClassFailed
	}
	switch gapMarker(detail) {
	case "known_defect":
		return ClassDefect
	case "known_gap", "fixme":
		if defectNamed {
			return ClassDefect
		}
		return ClassKnownGap
	}
	if defectNamed {
		return ClassDefect
	}
	return ClassSkipped
}

// --- name splitting ----------------------------------------------------------

// PlaywrightSep separates a Playwright title path.
const PlaywrightSep = " › "

// SplitTestName splits a reported test name into its path: on " › " for a
// Playwright title path, else on "/" for a Go subtest (a "/" inside a pytest
// parameter's brackets does not split).
func SplitTestName(name string) []string {
	if strings.Contains(name, PlaywrightSep) {
		var out []string
		for _, p := range strings.Split(name, PlaywrightSep) {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) == 0 {
			return []string{name}
		}
		return out
	}
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(name); i++ {
		switch name[i] {
		case '[', '(':
			depth++
		case ']', ')':
			if depth > 0 {
				depth--
			}
		case '/':
			if depth == 0 {
				if i > start {
					out = append(out, name[start:i])
				}
				start = i + 1
			}
		}
	}
	if start < len(name) {
		out = append(out, name[start:])
	}
	if len(out) == 0 {
		return []string{name}
	}
	return out
}

// --- tree ----------------------------------------------------------------------

// RunNode is one node of a run tree.
type RunNode struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	DurationMS int64      `json:"duration_ms"`
	Counts     RunCounts  `json:"counts"`
	Message    string     `json:"message,omitempty"`
	// ChildCount is the number of children, also when depth truncation
	// omitted them.
	ChildCount int        `json:"child_count"`
	Children   []*RunNode `json:"children"`

	// Lane fields, set on lane nodes only.
	Project    string `json:"project,omitempty"`
	Lane       string `json:"lane,omitempty"`
	TestRunID  uint   `json:"test_run_id,omitempty"`
	TestRunIDs []uint `json:"test_run_ids,omitempty"`

	path     []string
	index    map[string]*RunNode
	ownRows  []ownResult
	ownStart time.Time
	ownEnd   time.Time
	recorded bool   // the node's own span was recorded (not derived)
	class    string // own class, when the node has its own result
	counted  bool   // the node's own result is in the counts
	running  bool
}

type ownResult struct {
	class   string
	start   time.Time
	end     time.Time
	durMS   int64
	message string
}

func nodeID(path []string) string {
	h := fnv.New64a()
	for _, p := range path {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("n%016x", h.Sum64())
}

func newNode(parent *RunNode, name, kind string) *RunNode {
	var path []string
	if parent != nil {
		path = append(append([]string{}, parent.path...), kind+":"+name)
	} else {
		path = []string{kind + ":" + name}
	}
	n := &RunNode{Name: name, Kind: kind, path: path, ID: nodeID(path), Children: []*RunNode{}}
	if parent != nil {
		if parent.index == nil {
			parent.index = map[string]*RunNode{}
		}
		parent.index[name] = n
		parent.Children = append(parent.Children, n)
	}
	return n
}

func (n *RunNode) child(name, kind string) *RunNode {
	if c := n.index[name]; c != nil {
		return c
	}
	return newNode(n, name, kind)
}

// setSpan records the node's own span (its own start, end and duration).
func (n *RunNode) setSpan(start, end time.Time) {
	if n.ownStart.IsZero() || start.Before(n.ownStart) {
		n.ownStart = start
	}
	if end.After(n.ownEnd) {
		n.ownEnd = end
	}
}

// addSpecRows places spec runs under parent as tests and subtests.
func addSpecRows(parent *RunNode, rows []RunSpecRow, defectsPass bool, phase bool) {
	for _, r := range rows {
		segs := []string{r.Name}
		if !phase {
			segs = SplitTestName(r.Name)
		}
		n := parent
		for i, s := range segs {
			kind := KindSubtest
			if phase {
				kind = KindPhase
			} else if i == 0 {
				kind = KindTest
			}
			n = n.child(s, kind)
		}
		end := endOf(r.StartTime, r.EndTime, r.DurationMS)
		dur := r.DurationMS
		if dur <= 0 && r.EndTime != nil {
			dur = end.Sub(r.StartTime).Milliseconds()
		}
		msg := ""
		if r.Status != "passed" {
			msg = r.Message
			if l := markerLine(r.Detail); l != "" && r.Status == "skipped" {
				msg = l
			}
			msg = truncate(msg, 500)
		}
		n.ownRows = append(n.ownRows, ownResult{class: ClassifySpec(r.Name, r.Status, r.Detail, defectsPass),
			start: r.StartTime, end: end, durMS: dur, message: msg})
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// classRank orders own results of one node (several instances of one name):
// the worst decides.
var classRank = map[string]int{ClassSkipped: 1, ClassPassed: 2, ClassKnownGap: 3, ClassDefect: 4, ClassFailed: 5}

// finish computes counts, status and times bottom up. A node's own result is
// counted when it has no child with a result, or when it failed and no child
// did (a parent that failed on its own); otherwise its children carry it.
func (n *RunNode) finish() {
	for _, c := range n.Children {
		c.finish()
	}
	sortChildren(n)
	var childCounts RunCounts
	childStart, childEnd := time.Time{}, time.Time{}
	running := n.running
	for _, c := range n.Children {
		childCounts.add(c.Counts)
		if c.StartedAt != nil && (childStart.IsZero() || c.StartedAt.Before(childStart)) {
			childStart = *c.StartedAt
		}
		if c.EndedAt != nil && c.EndedAt.After(childEnd) {
			childEnd = *c.EndedAt
		}
		running = running || c.running
	}
	n.running = running
	n.Counts = childCounts

	if len(n.ownRows) > 0 {
		best := ""
		var msgs []string
		for _, r := range n.ownRows {
			if classRank[r.class] > classRank[best] {
				best = r.class
			}
			n.setSpan(r.start, r.end)
			if r.durMS > 0 {
				n.recorded = true
			}
			if r.message != "" && len(msgs) < 3 {
				msgs = append(msgs, r.message)
			}
		}
		n.class = best
		n.Message = truncate(strings.Join(msgs, "\n"), 1000)
		childFailed := childCounts.Failed+childCounts.Defect > 0
		if childCounts.Total == 0 || ((best == ClassFailed || best == ClassDefect) && !childFailed) {
			n.counted = true
			n.Counts.addClass(best)
		}
	}

	// Duration: the node's own when recorded, else the span of its children.
	switch {
	case n.recorded:
		s, e := n.ownStart, n.ownEnd
		n.StartedAt, n.EndedAt = &s, &e
		if len(n.ownRows) == 1 && n.ownRows[0].durMS > 0 {
			n.DurationMS = n.ownRows[0].durMS
		} else {
			n.DurationMS = e.Sub(s).Milliseconds()
		}
	case !childStart.IsZero():
		s, e := childStart, childEnd
		if e.Before(s) {
			e = s
		}
		n.StartedAt, n.EndedAt = &s, &e
		n.DurationMS = e.Sub(s).Milliseconds()
	case !n.ownStart.IsZero():
		s, e := n.ownStart, n.ownEnd
		if e.Before(s) {
			e = s
		}
		n.StartedAt, n.EndedAt = &s, &e
		n.DurationMS = e.Sub(s).Milliseconds()
	}

	switch {
	case n.running:
		n.Status = StatusRunning
	case len(n.Children) == 0 && n.class != "":
		n.Status = n.class
	default:
		n.Status = statusFromCounts(n.Counts)
	}
	n.ChildCount = len(n.Children)
}

func sortChildren(n *RunNode) {
	sort.SliceStable(n.Children, func(i, j int) bool {
		a, b := n.Children[i], n.Children[j]
		if (a.Name == SetupLane && a.Kind == KindLane) != (b.Name == SetupLane && b.Kind == KindLane) {
			return a.Kind == KindLane && a.Name == SetupLane
		}
		if a.Kind == KindPhase && b.Kind == KindPhase {
			return a.Name < b.Name
		}
		as, bs := firstStart(a), firstStart(b)
		if !as.Equal(bs) {
			return as.Before(bs)
		}
		return a.Name < b.Name
	})
}

func firstStart(n *RunNode) time.Time {
	if n.StartedAt != nil {
		return *n.StartedAt
	}
	return n.ownStart
}

// RunSource is everything a CI run's tree is built from.
type RunSource struct {
	CIRunID  string
	TestRuns []RunTestRun
	Suites   []RunSuiteRow
	Specs    []RunSpecRow
}

// RunSummary is a CI run without its tree (a row of the run list).
type RunSummary struct {
	CIRunID    string     `json:"ci_run_id"`
	SHA        string     `json:"sha"`
	Branch     string     `json:"branch"`
	Runner     string     `json:"runner"`
	Host       string     `json:"host"`
	Workflow   string     `json:"workflow"`
	URL        string     `json:"url"`
	StartedAt  *time.Time `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	DurationMS int64      `json:"duration_ms"`
	Status     string     `json:"status"`
	Counts     RunCounts  `json:"counts"`
	Lanes      []RunLane  `json:"lanes"`
	Projects   []string   `json:"projects"`
}

// RunLane is a lane of the run list.
type RunLane struct {
	Key        string     `json:"key"`
	Project    string     `json:"project"`
	Lane       string     `json:"lane"`
	TestRunID  uint       `json:"test_run_id"`
	TestRunIDs []uint     `json:"test_run_ids"`
	StartedAt  *time.Time `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	DurationMS int64      `json:"duration_ms"`
	Status     string     `json:"status"`
	Counts     RunCounts  `json:"counts"`
}

// BuildRun builds the tree of one CI run and its summary.
func BuildRun(src RunSource) (*RunSummary, *RunNode) {
	root := newNode(nil, src.CIRunID, KindRun)
	trByID := map[uint]RunTestRun{}
	laneOfTR := map[uint]*RunNode{}
	defectsTR := map[uint]bool{}
	laneTRs := map[*RunNode][]RunTestRun{}
	var setup *RunNode

	trs := append([]RunTestRun{}, src.TestRuns...)
	sort.SliceStable(trs, func(i, j int) bool { return trs[i].StartTime.Before(trs[j].StartTime) })
	for _, tr := range trs {
		trByID[tr.ID] = tr
		defectsTR[tr.ID] = tr.meta("pass") == "defects"
		key, lane := laneOf(tr)
		ln := root.child(key, KindLane)
		if ln.Project == "" {
			ln.Project, ln.Lane, ln.TestRunID = tr.ProjectID, lane, tr.ID
		}
		ln.TestRunIDs = append(ln.TestRunIDs, tr.ID)
		laneOfTR[tr.ID] = ln
		laneTRs[ln] = append(laneTRs[ln], tr)
		if tr.Status == StatusRunning && tr.EndTime == nil {
			ln.running = true
		}
	}

	specsBySuite := map[uint][]RunSpecRow{}
	for _, s := range src.Specs {
		specsBySuite[s.SuiteRunID] = append(specsBySuite[s.SuiteRunID], s)
	}
	setupTRs := map[uint]bool{}
	for _, su := range src.Suites {
		ln := laneOfTR[su.TestRunID]
		if ln == nil {
			continue
		}
		rows := specsBySuite[su.ID]
		if len(rows) == 0 && su.Status != "failed" {
			continue // an empty package or file: nothing ran
		}
		tr := trByID[su.TestRunID]
		phase := isSetupSuite(tr.ProjectID, su.Name)
		var b *RunNode
		if phase {
			if setup == nil {
				setup = root.child(SetupLane, KindLane)
				setup.Lane, setup.Project, setup.TestRunID = SetupLane, tr.ProjectID, tr.ID
			}
			if !setupTRs[tr.ID] {
				setupTRs[tr.ID] = true
				setup.TestRunIDs = append(setup.TestRunIDs, tr.ID)
			}
			name := ln.Name
			if ln.Project == "olly-ci-phases" && tr.meta("lane") == "" {
				name = su.Name
			}
			b = setup.child(name, KindBucket)
		} else {
			b = ln.child(su.Name, KindBucket)
		}
		end := endOf(su.StartTime, su.EndTime, su.DurationMS)
		if su.DurationMS > 0 && len(rows) > 0 {
			// several suite runs of one name (one per reporter) join spans
			b.setSpan(su.StartTime, end)
			b.recorded = true
		}
		if len(rows) == 0 {
			// a failed suite with no spec runs (a build failure): one failure
			b.ownRows = append(b.ownRows, ownResult{class: ClassFailed, start: su.StartTime, end: end, durMS: su.DurationMS,
				message: "suite failed with no test results"})
		}
		addSpecRows(b, rows, defectsTR[su.TestRunID], phase)
	}

	// A lane's own span: its test run's, when it is the only one and its
	// duration was recorded; else the span of its buckets.
	for ln, list := range laneTRs {
		if len(list) == 1 && list[0].DurationMS > 0 {
			tr := list[0]
			ln.setSpan(tr.StartTime, endOf(tr.StartTime, tr.EndTime, tr.DurationMS))
			ln.recorded = true
		}
	}
	// Lanes with no results left (every suite of them was setup) go away.
	kept := root.Children[:0]
	for _, ln := range root.Children {
		if len(ln.Children) > 0 || ln.running || ln.Name == SetupLane {
			kept = append(kept, ln)
		}
	}
	root.Children = kept
	root.finish()
	// A lane with a recorded span keeps the bucket-derived duration when its
	// buckets reach past it (several reporters for one test run).
	for _, ln := range root.Children {
		if ln.recorded && ln.StartedAt != nil {
			for _, b := range ln.Children {
				if b.EndedAt != nil && b.EndedAt.After(*ln.EndedAt) {
					e := *b.EndedAt
					ln.EndedAt = &e
					ln.DurationMS = e.Sub(*ln.StartedAt).Milliseconds()
				}
			}
		}
	}

	sum := &RunSummary{CIRunID: src.CIRunID, Runner: RunnerOf(src.CIRunID, src.TestRuns), Status: root.Status, Counts: root.Counts,
		StartedAt: root.StartedAt, EndedAt: root.EndedAt, DurationMS: root.DurationMS, Lanes: []RunLane{}, Projects: []string{}}
	sum.SHA = mostCommon(trs, func(t RunTestRun) string { return t.CommitSHA })
	sum.Branch = mostCommon(trs, func(t RunTestRun) string { return t.Branch })
	sum.Workflow = mostCommon(trs, func(t RunTestRun) string { return t.meta("ci_workflow") })
	sum.Host = mostCommon(trs, func(t RunTestRun) string {
		for _, k := range []string{"host", "runner_host", "hostname", "runner_name"} {
			if v := t.meta(k); v != "" {
				return v
			}
		}
		return ""
	})
	sum.URL = runURL(trs)
	seen := map[string]bool{}
	for _, tr := range trs {
		if !seen[tr.ProjectID] {
			seen[tr.ProjectID] = true
			sum.Projects = append(sum.Projects, tr.ProjectID)
		}
	}
	sort.Strings(sum.Projects)
	if sum.StartedAt == nil && len(trs) > 0 {
		s := trs[0].StartTime
		sum.StartedAt, sum.EndedAt = &s, &s
	}
	for _, ln := range root.Children {
		sum.Lanes = append(sum.Lanes, RunLane{Key: ln.Name, Project: ln.Project, Lane: ln.Lane, TestRunID: ln.TestRunID,
			TestRunIDs: ln.TestRunIDs, StartedAt: ln.StartedAt, EndedAt: ln.EndedAt, DurationMS: ln.DurationMS, Status: ln.Status, Counts: ln.Counts})
	}
	return sum, root
}

func mostCommon(trs []RunTestRun, f func(RunTestRun) string) string {
	n := map[string]int{}
	best, bestN := "", 0
	for _, t := range trs {
		v := f(t)
		if v == "" {
			continue
		}
		n[v]++
		if n[v] > bestN {
			best, bestN = v, n[v]
		}
	}
	return best
}

// runURL is the CI run's page: build_url cut to the run (a job URL names one
// job of it), else built from ci_repo / ci_server_url when sent.
func runURL(trs []RunTestRun) string {
	for _, t := range trs {
		u := t.meta("build_url")
		if u == "" {
			continue
		}
		if loc := reActionsRun.FindStringIndex(u); loc != nil {
			return u[:loc[1]]
		}
		return u
	}
	for _, t := range trs {
		if repo, id := t.meta("ci_repo"), t.meta("ci_run_id"); repo != "" && id != "" && !strings.HasPrefix(id, "local-") {
			server := t.meta("ci_server_url")
			if server == "" {
				server = "https://github.com"
			}
			return strings.TrimRight(server, "/") + "/" + repo + "/actions/runs/" + id
		}
	}
	return ""
}

// --- tree views ----------------------------------------------------------------

// FindNode finds a node by id.
func FindNode(n *RunNode, id string) *RunNode {
	if n.ID == id {
		return n
	}
	for _, c := range n.Children {
		if f := FindNode(c, id); f != nil {
			return f
		}
	}
	return nil
}

// PruneByClass keeps only the paths to nodes holding one of the classes
// (?status=failed keeps the failing paths). It returns false when nothing
// under n holds them.
func PruneByClass(n *RunNode, classes []string) bool {
	has := false
	for _, c := range classes {
		if n.Counts.get(c) > 0 || n.Status == c {
			has = true
		}
	}
	if !has {
		return false
	}
	kept := n.Children[:0]
	n.index = map[string]*RunNode{}
	for _, c := range n.Children {
		if PruneByClass(c, classes) {
			kept = append(kept, c)
			n.index[c.Name] = c
		}
	}
	n.Children = kept
	n.ChildCount = len(kept)
	return true
}

// TruncateDepth drops the children of nodes depth levels below n (n itself is
// level 0); counts and child_count stay.
func TruncateDepth(n *RunNode, depth int) {
	if depth <= 0 {
		n.Children = []*RunNode{}
		return
	}
	for _, c := range n.Children {
		TruncateDepth(c, depth-1)
	}
}

// --- compare -------------------------------------------------------------------

// Diff changes, in order of significance.
const (
	ChangeNewFailure = "new_failure"
	ChangeFixed      = "fixed"
	ChangeRemoved    = "removed"
	ChangeAdded      = "added"
	ChangeSlower     = "slower"
	ChangeFaster     = "faster"
)

var changeRank = map[string]int{ChangeNewFailure: 0, ChangeFixed: 1, ChangeRemoved: 2, ChangeAdded: 3, ChangeSlower: 4, ChangeFaster: 5}

// RunDiffSide is one side of a diff entry.
type RunDiffSide struct {
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
}

// RunDiff is one difference between two runs.
type RunDiff struct {
	Path   []string     `json:"path"`
	Kind   string       `json:"kind"`
	A      *RunDiffSide `json:"a"`
	B      *RunDiffSide `json:"b"`
	Change string       `json:"change"`
}

// A duration change is reported when the slower side takes at least
// slowRatio times as long and the change is at least slowMinMS.
const (
	slowRatio = 1.5
	slowMinMS = 1000
)

type flatNode struct {
	n    *RunNode
	path []string
}

// compareKey names a node for matching across runs: phases by their step
// number (their text names the replica and times, which change every run).
func compareKey(n *RunNode) string {
	if n.Kind == KindPhase {
		if f := strings.Fields(n.Name); len(f) > 0 {
			if _, err := strconv.Atoi(f[0]); err == nil {
				return f[0]
			}
		}
	}
	return n.Name
}

func flatten(n *RunNode, prefix []string, keyPrefix string, out map[string]flatNode, order *[]string) {
	for _, c := range n.Children {
		p := append(append([]string{}, prefix...), c.Name)
		k := keyPrefix + "\x00" + c.Kind + ":" + compareKey(c)
		out[k] = flatNode{n: c, path: p}
		*order = append(*order, k)
		flatten(c, p, k, out, order)
	}
}

func isFailing(class string) bool { return class == ClassFailed }

// CompareRuns lists what changed from run a to run b: tests that newly fail or
// were fixed (by their own result), lanes, buckets and tests added or removed
// (only the highest such node), and lanes, buckets and tests that got slower
// or faster.
func CompareRuns(a, b *RunNode) []RunDiff {
	fa, fb := map[string]flatNode{}, map[string]flatNode{}
	var oa, ob []string
	flatten(a, nil, "", fa, &oa)
	flatten(b, nil, "", fb, &ob)
	var diffs []RunDiff
	parentOf := func(k string) string {
		if i := strings.LastIndex(k, "\x00"); i > 0 {
			return k[:i]
		}
		return ""
	}
	side := func(n *RunNode) *RunDiffSide { return &RunDiffSide{Status: n.Status, DurationMS: n.DurationMS} }

	for _, k := range ob {
		nb := fb[k]
		na, ok := fa[k]
		if !ok {
			if p := parentOf(k); p != "" {
				if _, parentInA := fa[p]; !parentInA {
					continue // reported at the highest added node
				}
			}
			if nb.n.Kind == KindPhase {
				continue
			}
			diffs = append(diffs, RunDiff{Path: nb.path, Kind: nb.n.Kind, B: side(nb.n), Change: ChangeAdded})
			continue
		}
		// Status changes on the nodes whose own result is counted.
		if nb.n.counted || na.n.counted {
			ca, cb := na.n.class, nb.n.class
			if !na.n.counted {
				ca = na.n.Status
			}
			if !nb.n.counted {
				cb = nb.n.Status
			}
			switch {
			case isFailing(cb) && !isFailing(ca):
				diffs = append(diffs, RunDiff{Path: nb.path, Kind: nb.n.Kind, A: side(na.n), B: side(nb.n), Change: ChangeNewFailure})
				continue
			case isFailing(ca) && cb == ClassPassed:
				diffs = append(diffs, RunDiff{Path: nb.path, Kind: nb.n.Kind, A: side(na.n), B: side(nb.n), Change: ChangeFixed})
				continue
			}
		}
		switch nb.n.Kind {
		case KindLane, KindBucket, KindTest:
			da, db := na.n.DurationMS, nb.n.DurationMS
			switch {
			case db-da >= slowMinMS && float64(db) >= slowRatio*float64(da):
				diffs = append(diffs, RunDiff{Path: nb.path, Kind: nb.n.Kind, A: side(na.n), B: side(nb.n), Change: ChangeSlower})
			case da-db >= slowMinMS && float64(da) >= slowRatio*float64(db):
				diffs = append(diffs, RunDiff{Path: nb.path, Kind: nb.n.Kind, A: side(na.n), B: side(nb.n), Change: ChangeFaster})
			}
		}
	}
	for _, k := range oa {
		if _, ok := fb[k]; ok {
			continue
		}
		if p := parentOf(k); p != "" {
			if _, parentInB := fb[p]; !parentInB {
				continue
			}
		}
		na := fa[k]
		if na.n.Kind == KindPhase {
			continue
		}
		diffs = append(diffs, RunDiff{Path: na.path, Kind: na.n.Kind, A: side(na.n), Change: ChangeRemoved})
	}
	kindRank := map[string]int{KindLane: 0, KindBucket: 1, KindPhase: 2, KindTest: 2, KindSubtest: 3}
	delta := func(d RunDiff) int64 {
		var x, y int64
		if d.A != nil {
			x = d.A.DurationMS
		}
		if d.B != nil {
			y = d.B.DurationMS
		}
		if y > x {
			return y - x
		}
		return x - y
	}
	sort.SliceStable(diffs, func(i, j int) bool {
		a, b := diffs[i], diffs[j]
		if changeRank[a.Change] != changeRank[b.Change] {
			return changeRank[a.Change] < changeRank[b.Change]
		}
		if kindRank[a.Kind] != kindRank[b.Kind] {
			return kindRank[a.Kind] < kindRank[b.Kind]
		}
		if da, db := delta(a), delta(b); da != db {
			return da > db
		}
		return strings.Join(a.Path, "\x00") < strings.Join(b.Path, "\x00")
	})
	return diffs
}

// --- trends --------------------------------------------------------------------

// NodeAtPath descends from n by names (the path below n), or nil.
func NodeAtPath(n *RunNode, names []string) *RunNode {
	for _, s := range names {
		if n = n.index[s]; n == nil {
			return nil
		}
	}
	return n
}

// BuildTests builds the test nodes of spec rows alone (no lanes or buckets),
// under a synthetic root, for test-level trends.
func BuildTests(rows []RunSpecRow, defectsPass func(RunSpecRow) bool) *RunNode {
	root := newNode(nil, "", KindRun)
	for _, r := range rows {
		addSpecRows(root, []RunSpecRow{r}, defectsPass != nil && defectsPass(r), false)
	}
	root.finish()
	return root
}

// metadataMap decodes a JSONB metadata column.
func metadataMap(raw []byte) map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]interface{}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}
