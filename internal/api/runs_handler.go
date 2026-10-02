package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/guidewire-oss/fern-platform/pkg/logging"
)

// RunsHandler serves CI runs: the list, one run's tree, trends, compare and
// filter facets (see runs_model.go for what a CI run is).
type RunsHandler struct {
	*BaseHandler
	db    *gorm.DB
	store *RunsStore
}

// NewRunsHandler creates the runs handler over a runs store.
func NewRunsHandler(db *gorm.DB, store *RunsStore, logger *logging.Logger) *RunsHandler {
	return &RunsHandler{BaseHandler: NewBaseHandler(logger), db: db, store: store}
}

// RegisterRoutes registers the runs routes (reads on the user group, the
// backfill on the admin group).
func (h *RunsHandler) RegisterRoutes(userGroup, adminGroup *gin.RouterGroup) {
	userGroup.GET("/runs", h.listRuns)
	userGroup.GET("/runs/trends", h.trends)
	userGroup.GET("/runs/compare", h.compare)
	userGroup.GET("/runs/facets", h.facets)
	userGroup.GET("/runs/:ciRunId", h.getRun)
	adminGroup.POST("/runs/backfill", h.backfill)
}

func parseTimeParam(c *gin.Context, name string) (*time.Time, bool) {
	v := c.Query(name)
	if v == "" {
		return nil, true
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return &t, true
		}
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": name + " must be RFC3339 or YYYY-MM-DD"})
	return nil, false
}

func intParam(c *gin.Context, name string, def, lo, hi int) int {
	n, err := strconv.Atoi(c.Query(name))
	if err != nil {
		return def
	}
	return max(lo, min(hi, n))
}

type ciRunRow struct {
	CIRunID    string
	SHA        string
	Branch     string
	Runner     string
	Host       string
	Workflow   string
	URL        string
	StartedAt  time.Time
	EndedAt    *time.Time
	DurationMS int64
	Status     string
	Total      int
	Passed     int
	Failed     int
	Skipped    int
	KnownGap   int
	Defect     int
	Lanes      []byte
	Projects   string
}

func (r ciRunRow) toSummary() RunSummary {
	s := RunSummary{CIRunID: r.CIRunID, SHA: r.SHA, Branch: r.Branch, Runner: r.Runner, Host: r.Host, Workflow: r.Workflow, URL: r.URL,
		EndedAt: r.EndedAt, DurationMS: r.DurationMS, Status: r.Status,
		Counts: RunCounts{Total: r.Total, Passed: r.Passed, Failed: r.Failed, Skipped: r.Skipped, KnownGap: r.KnownGap, Defect: r.Defect}}
	st := r.StartedAt
	s.StartedAt = &st
	s.Lanes = []RunLane{}
	_ = json.Unmarshal(r.Lanes, &s.Lanes)
	s.Projects = []string{}
	if p := strings.Trim(r.Projects, "{}"); p != "" {
		s.Projects = strings.Split(p, ",")
	}
	return s
}

const ciRunCols = `ci_run_id, sha, branch, runner, host, workflow, url, started_at, ended_at, duration_ms, status,
	total, passed, failed, skipped, known_gap, defect, lanes, projects::text AS projects`

// listRuns: GET /runs?branch=&sha=&sha_prefix=&project=&lane=&runner=&status=&ci_run_id=&since=&until=&q=&limit=&offset=
func (h *RunsHandler) listRuns(c *gin.Context) {
	where := []string{"TRUE"}
	var args []interface{}
	eq := func(param, col string) {
		if v := strings.TrimSpace(c.Query(param)); v != "" {
			where = append(where, col+" = ?")
			args = append(args, v)
		}
	}
	eq("branch", "branch")
	eq("sha", "sha")
	eq("runner", "runner")
	eq("ci_run_id", "ci_run_id")
	if v := strings.TrimSpace(c.Query("status")); v != "" {
		where = append(where, "status IN ?")
		args = append(args, strings.Split(v, ","))
	}
	if v := strings.TrimSpace(c.Query("sha_prefix")); v != "" {
		where = append(where, "sha LIKE ?")
		args = append(args, likePrefix(v))
	}
	if v := strings.TrimSpace(c.Query("project")); v != "" {
		where = append(where, "projects @> ARRAY[?]::text[]")
		args = append(args, v)
	}
	if v := strings.TrimSpace(c.Query("lane")); v != "" {
		where = append(where, "(lane_names @> ARRAY[?]::text[] OR lane_keys @> ARRAY[?]::text[])")
		args = append(args, v, v)
	}
	if v := strings.TrimSpace(c.Query("q")); v != "" {
		where = append(where, "search LIKE ?")
		args = append(args, "%"+escapeLike(strings.ToLower(v))+"%")
	}
	since, ok := parseTimeParam(c, "since")
	if !ok {
		return
	}
	until, ok := parseTimeParam(c, "until")
	if !ok {
		return
	}
	if since != nil {
		where = append(where, "started_at >= ?")
		args = append(args, *since)
	}
	if until != nil {
		where = append(where, "started_at < ?")
		args = append(args, *until)
	}
	limit := intParam(c, "limit", 50, 1, 500)
	offset := intParam(c, "offset", 0, 0, 1<<30)
	w := strings.Join(where, " AND ")

	var total int64
	if err := h.db.Raw(`SELECT COUNT(*) FROM ci_runs WHERE `+w, args...).Scan(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var rows []ciRunRow
	if err := h.db.Raw(`SELECT `+ciRunCols+` FROM ci_runs WHERE `+w+` ORDER BY started_at DESC, ci_run_id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...).Scan(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	runs := make([]RunSummary, 0, len(rows))
	for _, r := range rows {
		runs = append(runs, r.toSummary())
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "runs": runs})
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func likePrefix(s string) string { return escapeLike(s) + "%" }

// runDetail is a run with its tree.
type runDetail struct {
	RunSummary
	Tree *RunNode `json:"tree"`
}

// getRun: GET /runs/{ci_run_id}?depth=N&status=failed[,defect]&node=<id>
// Built live from the test run tables, so it is current even before the
// summary refresh lands.
func (h *RunsHandler) getRun(c *gin.Context) {
	id := c.Param("ciRunId")
	src, err := h.store.LoadRun(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if src == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no CI run " + id})
		return
	}
	sum, root := BuildRun(*src)
	tree := root
	if nid := c.Query("node"); nid != "" {
		if tree = FindNode(root, nid); tree == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "no node " + nid + " in run " + id})
			return
		}
	}
	if st := strings.TrimSpace(c.Query("status")); st != "" {
		if !PruneByClass(tree, strings.Split(st, ",")) {
			tree.Children, tree.ChildCount = []*RunNode{}, 0
		}
	}
	if d := c.Query("depth"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n >= 0 {
			TruncateDepth(tree, n)
		}
	}
	c.JSON(http.StatusOK, runDetail{RunSummary: *sum, Tree: tree})
}

// TrendPoint is one run of a trend series.
type TrendPoint struct {
	CIRunID    string     `json:"ci_run_id"`
	SHA        string     `json:"sha"`
	Branch     string     `json:"branch"`
	StartedAt  *time.Time `json:"started_at"`
	DurationMS int64      `json:"duration_ms"`
	Status     string     `json:"status"`
	Counts     RunCounts  `json:"counts"`
}

// TrendSeries is one lane, bucket or test over runs, oldest first.
type TrendSeries struct {
	Key    string       `json:"key"`
	Lane   string       `json:"lane,omitempty"` // the lane key of a bucket series
	Points []TrendPoint `json:"points"`
}

// trends: GET /runs/trends?level=lane|bucket|test&key=<name or prefix>&branch=main&since=&until=&limit=100
func (h *RunsHandler) trends(c *gin.Context) {
	level := c.DefaultQuery("level", "lane")
	key := c.Query("key")
	branch := c.DefaultQuery("branch", "main")
	limit := intParam(c, "limit", 100, 1, 1000)
	since, ok := parseTimeParam(c, "since")
	if !ok {
		return
	}
	until, ok := parseTimeParam(c, "until")
	if !ok {
		return
	}
	if since == nil {
		t := time.Now().Add(-90 * 24 * time.Hour)
		since = &t
	}
	if until == nil {
		t := time.Now().Add(time.Hour)
		until = &t
	}
	var series []TrendSeries
	var err error
	switch level {
	case "lane", "bucket":
		series, err = h.nodeTrends(level, key, branch, *since, *until, limit)
	case "test":
		if key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "level=test needs a key (a test name or prefix)"})
			return
		}
		series, err = h.testTrends(key, branch, *since, *until, limit)
		if len(series) > testTrendSeriesCap {
			// a prefix as broad as "Test" names thousands of tests
			c.JSON(http.StatusOK, gin.H{"level": level, "key": key, "branch": branch, "series": series[:testTrendSeriesCap],
				"truncated": true, "series_total": len(series)})
			return
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "level must be lane, bucket or test"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if series == nil {
		series = []TrendSeries{}
	}
	c.JSON(http.StatusOK, gin.H{"level": level, "key": key, "branch": branch, "series": series})
}

func (h *RunsHandler) nodeTrends(level, key, branch string, since, until time.Time, limit int) ([]TrendSeries, error) {
	q := `SELECT * FROM (
		SELECT ci_run_id, lane_key, key, sha, branch, started_at, duration_ms, status, total, passed, failed, skipped, known_gap, defect,
		       row_number() OVER (PARTITION BY lane_key, key ORDER BY started_at DESC) AS rn
		FROM ci_run_nodes WHERE level = ? AND started_at >= ? AND started_at < ?`
	args := []interface{}{level, since, until}
	if branch != "any" && branch != "" {
		q += ` AND branch = ?`
		args = append(args, branch)
	}
	if key != "" {
		// a lane's key is its lane key; a bucket's is the suite name
		q += ` AND key LIKE ?`
		args = append(args, likePrefix(key))
	}
	q += `) x WHERE rn <= ? ORDER BY lane_key, key, started_at`
	args = append(args, limit)
	var rows []struct {
		CIRunID, LaneKey, Key, SHA, Branch, Status       string
		StartedAt                                        time.Time
		DurationMS                                       int64
		Total, Passed, Failed, Skipped, KnownGap, Defect int
	}
	if err := h.db.Raw(q, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	var out []TrendSeries
	for _, r := range rows {
		n := len(out)
		if n == 0 || out[n-1].Key != r.Key || (level == "bucket" && out[n-1].Lane != r.LaneKey) {
			s := TrendSeries{Key: r.Key}
			if level == "bucket" {
				s.Lane = r.LaneKey
			}
			out = append(out, s)
			n++
		}
		st := r.StartedAt
		out[n-1].Points = append(out[n-1].Points, TrendPoint{CIRunID: r.CIRunID, SHA: r.SHA, Branch: r.Branch, StartedAt: &st, DurationMS: r.DurationMS,
			Status: r.Status, Counts: RunCounts{Total: r.Total, Passed: r.Passed, Failed: r.Failed, Skipped: r.Skipped, KnownGap: r.KnownGap, Defect: r.Defect}})
	}
	return out, nil
}

// testTrendSeriesCap bounds how many series one test trend returns.
const testTrendSeriesCap = 200

// testTrendRowCap bounds how many spec runs one test trend reads.
const testTrendRowCap = 200000

// testTrends reads the spec runs whose name starts with key and builds, per CI
// run, the test node at the key's depth: key "TestX" gives one series per
// top-level test starting TestX; a full Playwright title gives that test.
func (h *RunsHandler) testTrends(key, branch string, since, until time.Time, limit int) ([]TrendSeries, error) {
	q := `SELECT sp.id, sp.suite_run_id, sp.spec_name AS name, sp.status, sp.start_time, sp.end_time, COALESCE(sp.duration_ms, 0) AS duration_ms,
		CASE WHEN sp.status = 'passed' THEN '' ELSE LEFT(COALESCE(NULLIF(sp.error_message, ''), sp.description, ''), 500) END AS message,
		CASE WHEN sp.status = 'passed' THEN '' ELSE LEFT(COALESCE(sp.error_message, '') || E'\n' || COALESCE(sp.description, '') || E'\n' || COALESCE(sp.metadata::text, ''), 4000) END AS detail,
		tr.id AS test_run_id, COALESCE(m.ci_run_id, '') AS ci_run_id, tr.metadata AS tr_metadata, COALESCE(tr.commit_sha, '') AS sha, COALESCE(tr.branch, '') AS branch,
		tr.start_time AS tr_start
		FROM spec_runs sp
		JOIN suite_runs sr ON sr.id = sp.suite_run_id
		JOIN test_runs tr ON tr.id = sr.test_run_id
		LEFT JOIN ci_run_test_runs m ON m.test_run_id = tr.id
		WHERE (sp.spec_name LIKE ? OR sp.spec_name LIKE ?) AND tr.start_time >= ? AND tr.start_time < ?
		  AND sp.deleted_at IS NULL AND sr.deleted_at IS NULL AND tr.deleted_at IS NULL`
	args := []interface{}{likePrefix(key), likePrefix(GoDefectsPrefix + key), since, until}
	if branch != "any" && branch != "" {
		q += ` AND tr.branch = ?`
		args = append(args, branch)
	}
	q += ` ORDER BY tr.start_time DESC LIMIT ?`
	args = append(args, testTrendRowCap)
	var rows []struct {
		RunSpecRow
		TestRunID  uint
		CIRunID    string
		TrMetadata []byte
		SHA        string
		Branch     string
		TrStart    time.Time
	}
	if err := h.db.Raw(q, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	keyDepth := len(SplitTestName(key))
	type runAcc struct {
		id, sha, branch string
		start           time.Time
		specs           []RunSpecRow
		defects         map[uint]bool // spec id -> from the defects pass
	}
	runs := map[string]*runAcc{}
	var order []string
	for _, r := range rows {
		ci := r.CIRunID
		if ci == "" {
			ci = CIRunIDOf(RunTestRun{ID: r.TestRunID, Metadata: metadataMap(r.TrMetadata)})
		}
		a := runs[ci]
		if a == nil {
			a = &runAcc{id: ci, sha: r.SHA, branch: r.Branch, start: r.TrStart, defects: map[uint]bool{}}
			runs[ci] = a
			order = append(order, ci)
		}
		if r.TrStart.Before(a.start) {
			a.start = r.TrStart
		}
		spec := r.RunSpecRow
		// The defects pass reports a defect test as defects/<name>: it joins
		// the gate's test of the same name.
		if strings.HasPrefix(spec.Name, GoDefectsPrefix) {
			spec.Name = strings.TrimPrefix(spec.Name, GoDefectsPrefix)
			a.defects[spec.ID] = true
		}
		if (RunTestRun{Metadata: metadataMap(r.TrMetadata)}).meta("pass") == "defects" {
			a.defects[spec.ID] = true
		}
		a.specs = append(a.specs, spec)
	}
	bySeries := map[string]*TrendSeries{}
	var seriesOrder []string
	for _, ci := range order {
		a := runs[ci]
		root := BuildTests(a.specs, func(r RunSpecRow) bool { return a.defects[r.ID] })
		for _, n := range nodesAtDepth(root, keyDepth) {
			name := strings.Join(n.namePath, pathSep(key))
			if !strings.HasPrefix(name, key) && !strings.HasPrefix(strings.ReplaceAll(name, " ", "_"), strings.ReplaceAll(key, " ", "_")) {
				continue
			}
			s := bySeries[name]
			if s == nil {
				s = &TrendSeries{Key: name}
				bySeries[name] = s
				seriesOrder = append(seriesOrder, name)
			}
			st := a.start
			if n.node.StartedAt != nil {
				st = *n.node.StartedAt
			}
			s.Points = append(s.Points, TrendPoint{CIRunID: a.id, SHA: a.sha, Branch: a.branch, StartedAt: &st, DurationMS: n.node.DurationMS,
				Status: n.node.Status, Counts: n.node.Counts})
		}
	}
	sort.Strings(seriesOrder)
	out := make([]TrendSeries, 0, len(seriesOrder))
	for _, k := range seriesOrder {
		s := bySeries[k]
		sort.SliceStable(s.Points, func(i, j int) bool { return s.Points[i].StartedAt.Before(*s.Points[j].StartedAt) })
		if len(s.Points) > limit {
			s.Points = s.Points[len(s.Points)-limit:]
		}
		out = append(out, *s)
	}
	return out, nil
}

func pathSep(key string) string {
	if strings.Contains(key, PlaywrightSep) {
		return PlaywrightSep
	}
	return "/"
}

type pathNode struct {
	node     *RunNode
	namePath []string
}

// nodesAtDepth lists the nodes depth levels below root (1 = top-level tests),
// with their name paths.
func nodesAtDepth(root *RunNode, depth int) []pathNode {
	cur := []pathNode{{node: root}}
	for d := 0; d < depth; d++ {
		var next []pathNode
		for _, p := range cur {
			for _, c := range p.node.Children {
				next = append(next, pathNode{node: c, namePath: append(append([]string{}, p.namePath...), c.Name)})
			}
		}
		cur = next
	}
	return cur
}

// compare: GET /runs/compare?a=<ci_run_id>&b=<ci_run_id>
func (h *RunsHandler) compare(c *gin.Context) {
	a, b := c.Query("a"), c.Query("b")
	if a == "" || b == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a and b (CI run ids) are required"})
		return
	}
	var roots [2]*RunNode
	var sums [2]*RunSummary
	for i, id := range []string{a, b} {
		src, err := h.store.LoadRun(id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if src == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "no CI run " + id})
			return
		}
		sums[i], roots[i] = BuildRun(*src)
	}
	diff := CompareRuns(roots[0], roots[1])
	if diff == nil {
		diff = []RunDiff{}
	}
	c.JSON(http.StatusOK, gin.H{"a": sums[0], "b": sums[1], "diff": diff})
}

// facets: GET /runs/facets?since= (default 30 days)
func (h *RunsHandler) facets(c *gin.Context) {
	since, ok := parseTimeParam(c, "since")
	if !ok {
		return
	}
	if since == nil {
		t := time.Now().Add(-30 * 24 * time.Hour)
		since = &t
	}
	type branchRow struct {
		Branch string    `json:"branch"`
		Runs   int       `json:"runs"`
		Latest time.Time `json:"latest"`
	}
	var branches []branchRow
	var shas []struct {
		SHA    string    `json:"sha"`
		Branch string    `json:"branch"`
		Latest time.Time `json:"latest"`
	}
	var projects, lanes, laneKeys, runners, statuses []string
	steps := []struct {
		q   string
		dst interface{}
	}{
		{`SELECT branch, COUNT(*) AS runs, MAX(started_at) AS latest FROM ci_runs WHERE started_at >= ? AND branch <> '' GROUP BY branch ORDER BY latest DESC`, &branches},
		{`SELECT sha, MAX(branch) AS branch, MAX(started_at) AS latest FROM ci_runs WHERE started_at >= ? AND sha <> '' GROUP BY sha ORDER BY latest DESC LIMIT 100`, &shas},
		{`SELECT DISTINCT unnest(projects) AS p FROM ci_runs WHERE started_at >= ? ORDER BY 1`, &projects},
		{`SELECT DISTINCT unnest(lane_names) AS l FROM ci_runs WHERE started_at >= ? ORDER BY 1`, &lanes},
		{`SELECT DISTINCT unnest(lane_keys) AS l FROM ci_runs WHERE started_at >= ? ORDER BY 1`, &laneKeys},
		{`SELECT DISTINCT runner FROM ci_runs WHERE started_at >= ? ORDER BY 1`, &runners},
		{`SELECT DISTINCT status FROM ci_runs WHERE started_at >= ? ORDER BY 1`, &statuses},
	}
	for _, s := range steps {
		if err := h.db.Raw(s.q, *since).Scan(s.dst).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	nn := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	if branches == nil {
		branches = []branchRow{}
	}
	shaOut := make([]gin.H, 0, len(shas))
	for _, s := range shas {
		shaOut = append(shaOut, gin.H{"sha": s.SHA, "branch": s.Branch, "latest": s.Latest})
	}
	c.JSON(http.StatusOK, gin.H{"since": since, "branches": branches, "shas": shaOut, "projects": nn(projects), "lanes": nn(lanes),
		"lane_keys": nn(laneKeys), "runners": nn(runners), "statuses": nn(statuses)})
}

// backfill: POST /admin/runs/backfill?days=N (0 or absent = every run).
func (h *RunsHandler) backfill(c *gin.Context) {
	days := intParam(c, "days", 0, 0, 3650)
	start := time.Now()
	n, err := h.store.Backfill(c.Request.Context(), days)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "ci_runs": n})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ci_runs": n, "days": days, "took_ms": time.Since(start).Milliseconds()})
}
