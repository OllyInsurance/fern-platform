package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"gorm.io/gorm"

	"github.com/guidewire-oss/fern-platform/pkg/logging"
)

// RunsStore loads CI runs from the test run tables and keeps the run summary
// tables (ci_runs, ci_run_nodes, ci_run_test_runs) current: ingest calls
// Touch, a sweep catches test runs changed any other way, and a backfill
// rebuilds everything when the summary version changes.
type RunsStore struct {
	db     *gorm.DB
	logger *logging.Logger

	mu      sync.Mutex
	pending map[uint]bool
	wake    chan struct{}
}

// NewRunsStore creates a runs store.
func NewRunsStore(db *gorm.DB, logger *logging.Logger) *RunsStore {
	return &RunsStore{db: db, logger: logger, pending: map[uint]bool{}, wake: make(chan struct{}, 1)}
}

// Touch queues a refresh of the CI run a test run belongs to.
func (s *RunsStore) Touch(testRunID uint) {
	if s == nil || testRunID == 0 {
		return
	}
	s.mu.Lock()
	s.pending[testRunID] = true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// TouchSuiteRun queues a refresh of the CI run a suite run belongs to.
func (s *RunsStore) TouchSuiteRun(suiteRunID uint) {
	if s == nil || suiteRunID == 0 {
		return
	}
	var id uint
	if s.db.Raw(`SELECT test_run_id FROM suite_runs WHERE id = ?`, suiteRunID).Scan(&id).Error == nil {
		s.Touch(id)
	}
}

// Start runs the refresh worker, the sweep, and the backfill when the summary
// is missing or was built by another version.
func (s *RunsStore) Start(ctx context.Context) {
	go s.worker(ctx)
	go s.sweep(ctx)
	go func() {
		var v string
		s.db.Raw(`SELECT value FROM ci_runs_state WHERE key = 'summary_version'`).Scan(&v)
		if v == strconv.Itoa(RunsSummaryVersion) {
			return
		}
		n, err := s.Backfill(ctx, 0)
		if err != nil {
			s.logger.WithError(err).Error("runs summary backfill failed")
			return
		}
		s.logger.WithFields(map[string]interface{}{"ci_runs": n}).Info("runs summary backfill done")
	}()
}

func (s *RunsStore) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		// Let a burst of reports for one CI run (one per package) settle.
		time.Sleep(2 * time.Second)
		s.mu.Lock()
		ids := make([]uint, 0, len(s.pending))
		for id := range s.pending {
			ids = append(ids, id)
		}
		s.pending = map[uint]bool{}
		s.mu.Unlock()
		if err := s.refreshTestRuns(ids); err != nil {
			s.logger.WithError(err).Error("runs summary refresh failed")
		}
	}
}

// sweep refreshes the CI runs of test runs changed since the last sweep, for
// writes that did not come through a handler that calls Touch.
func (s *RunsStore) sweep(ctx context.Context) {
	mark := time.Now().Add(-10 * time.Minute)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		var ids []uint
		if err := s.db.Raw(`SELECT id FROM test_runs WHERE updated_at >= ? OR created_at >= ?`, mark, mark).Scan(&ids).Error; err != nil {
			s.logger.WithError(err).Warn("runs summary sweep failed")
			continue
		}
		mark = now.Add(-5 * time.Second)
		for _, id := range ids {
			s.Touch(id)
		}
	}
}

type trRow struct {
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
	Metadata    []byte
	DeletedAt   *time.Time
}

func (r trRow) toRun() RunTestRun {
	return RunTestRun{ID: r.ID, ProjectID: r.ProjectID, RunID: r.RunID, Branch: r.Branch, CommitSHA: r.CommitSHA, Status: r.Status,
		StartTime: r.StartTime, EndTime: r.EndTime, DurationMS: r.DurationMS, Environment: r.Environment, Metadata: metadataMap(r.Metadata)}
}

const trCols = `id, project_id, run_id, COALESCE(branch, '') AS branch, COALESCE(commit_sha, '') AS commit_sha, COALESCE(status, '') AS status,
	start_time, end_time, COALESCE(duration_ms, 0) AS duration_ms, COALESCE(environment, '') AS environment, metadata, deleted_at`

// refreshTestRuns refreshes the CI runs of the given test runs.
func (s *RunsStore) refreshTestRuns(ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	var rows []trRow
	if err := s.db.Raw(`SELECT `+trCols+` FROM test_runs WHERE id IN ?`, ids).Scan(&rows).Error; err != nil {
		return err
	}
	ci := map[string]bool{}
	for _, r := range rows {
		id := CIRunIDOf(r.toRun())
		ci[id] = true
		if r.DeletedAt == nil {
			if err := s.db.Exec(`INSERT INTO ci_run_test_runs (test_run_id, ci_run_id) VALUES (?, ?)
				ON CONFLICT (test_run_id) DO UPDATE SET ci_run_id = EXCLUDED.ci_run_id`, r.ID, id).Error; err != nil {
				return err
			}
		}
	}
	// A test run that moved CI run (or was deleted) leaves its old one.
	var old []string
	s.db.Raw(`SELECT DISTINCT ci_run_id FROM ci_run_test_runs WHERE test_run_id IN ?`, ids).Scan(&old)
	for _, id := range old {
		ci[id] = true
	}
	for id := range ci {
		if err := s.Refresh(id); err != nil {
			return fmt.Errorf("refresh %s: %w", id, err)
		}
	}
	return nil
}

// LoadRun loads everything one CI run's tree is built from; nil when the CI
// run has no test runs.
func (s *RunsStore) LoadRun(ciRunID string) (*RunSource, error) {
	var rows []trRow
	q := `SELECT ` + trCols + ` FROM test_runs WHERE deleted_at IS NULL AND (
		id IN (SELECT test_run_id FROM ci_run_test_runs WHERE ci_run_id = ?) OR metadata->>'ci_run_id' = ?`
	args := []interface{}{ciRunID, ciRunID}
	if strings.HasPrefix(ciRunID, "tr-") {
		if n, err := strconv.ParseUint(strings.TrimPrefix(ciRunID, "tr-"), 10, 64); err == nil {
			q += ` OR id = ?`
			args = append(args, n)
		}
	}
	q += `)`
	if err := s.db.Raw(q, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	src := &RunSource{CIRunID: ciRunID}
	for _, r := range rows {
		tr := r.toRun()
		if CIRunIDOf(tr) != ciRunID {
			continue // mapped by an old ci_run_id; the refresh fixes the map
		}
		src.TestRuns = append(src.TestRuns, tr)
	}
	if len(src.TestRuns) == 0 {
		return nil, nil
	}
	ids := make([]uint, len(src.TestRuns))
	for i, t := range src.TestRuns {
		ids[i] = t.ID
	}
	if err := s.db.Raw(`SELECT id, test_run_id, suite_name AS name, COALESCE(status, '') AS status, start_time, end_time, COALESCE(duration_ms, 0) AS duration_ms
		FROM suite_runs WHERE test_run_id IN ? AND deleted_at IS NULL ORDER BY id`, ids).Scan(&src.Suites).Error; err != nil {
		return nil, err
	}
	if len(src.Suites) == 0 {
		return src, nil
	}
	sids := make([]uint, len(src.Suites))
	for i, su := range src.Suites {
		sids[i] = su.ID
	}
	if err := s.db.Raw(`SELECT id, suite_run_id, spec_name AS name, status, start_time, end_time, COALESCE(duration_ms, 0) AS duration_ms,
		CASE WHEN status = 'passed' THEN '' ELSE LEFT(COALESCE(NULLIF(error_message, ''), description, ''), 2000) END AS message,
		CASE WHEN status = 'passed' THEN '' ELSE LEFT(COALESCE(error_message, '') || E'\n' || COALESCE(description, '') || E'\n' || COALESCE(metadata::text, ''), 4000) END AS detail
		FROM spec_runs WHERE suite_run_id IN ? AND deleted_at IS NULL ORDER BY id`, sids).Scan(&src.Specs).Error; err != nil {
		return nil, err
	}
	return src, nil
}

// Refresh rebuilds one CI run's summary rows from its test runs.
func (s *RunsStore) Refresh(ciRunID string) error {
	src, err := s.LoadRun(ciRunID)
	if err != nil {
		return err
	}
	if src == nil {
		return s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(`DELETE FROM ci_run_nodes WHERE ci_run_id = ?`, ciRunID).Error; err != nil {
				return err
			}
			return tx.Exec(`DELETE FROM ci_runs WHERE ci_run_id = ?`, ciRunID).Error
		})
	}
	sum, root := BuildRun(*src)
	return s.save(sum, root, src)
}

func (s *RunsStore) save(sum *RunSummary, root *RunNode, src *RunSource) error {
	lanesJSON, _ := json.Marshal(sum.Lanes)
	var laneKeys, laneNames []string
	for _, l := range sum.Lanes {
		laneKeys = append(laneKeys, l.Key)
		laneNames = append(laneNames, l.Lane)
	}
	search := strings.ToLower(strings.Join(append(append([]string{sum.CIRunID, sum.SHA, sum.Branch, sum.Workflow, sum.Host, sum.Runner},
		sum.Projects...), laneKeys...), " "))
	started := time.Now()
	if sum.StartedAt != nil {
		started = *sum.StartedAt
	}
	c := sum.Counts
	return s.db.Transaction(func(tx *gorm.DB) error {
		for _, t := range src.TestRuns {
			if err := tx.Exec(`INSERT INTO ci_run_test_runs (test_run_id, ci_run_id) VALUES (?, ?)
				ON CONFLICT (test_run_id) DO UPDATE SET ci_run_id = EXCLUDED.ci_run_id`, t.ID, sum.CIRunID).Error; err != nil {
				return err
			}
		}
		if err := tx.Exec(`INSERT INTO ci_runs (ci_run_id, sha, branch, runner, host, workflow, url, projects, lane_keys, lane_names,
				started_at, ended_at, duration_ms, status, total, passed, failed, skipped, known_gap, defect, lanes, search, refreshed_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, now())
			ON CONFLICT (ci_run_id) DO UPDATE SET sha = EXCLUDED.sha, branch = EXCLUDED.branch, runner = EXCLUDED.runner, host = EXCLUDED.host,
				workflow = EXCLUDED.workflow, url = EXCLUDED.url, projects = EXCLUDED.projects, lane_keys = EXCLUDED.lane_keys,
				lane_names = EXCLUDED.lane_names, started_at = EXCLUDED.started_at, ended_at = EXCLUDED.ended_at,
				duration_ms = EXCLUDED.duration_ms, status = EXCLUDED.status, total = EXCLUDED.total, passed = EXCLUDED.passed,
				failed = EXCLUDED.failed, skipped = EXCLUDED.skipped, known_gap = EXCLUDED.known_gap, defect = EXCLUDED.defect,
				lanes = EXCLUDED.lanes, search = EXCLUDED.search, refreshed_at = now()`,
			sum.CIRunID, sum.SHA, sum.Branch, sum.Runner, sum.Host, sum.Workflow, sum.URL, pq.Array(nonNil(sum.Projects)), pq.Array(nonNil(laneKeys)),
			pq.Array(nonNil(laneNames)), started, sum.EndedAt, sum.DurationMS, sum.Status, c.Total, c.Passed, c.Failed, c.Skipped, c.KnownGap, c.Defect,
			string(lanesJSON), search).Error; err != nil {
			return err
		}
		if err := tx.Exec(`DELETE FROM ci_run_nodes WHERE ci_run_id = ?`, sum.CIRunID).Error; err != nil {
			return err
		}
		type nodeRow struct {
			level, laneKey, key, project string
			n                            *RunNode
		}
		var nodes []nodeRow
		for _, ln := range root.Children {
			nodes = append(nodes, nodeRow{"lane", ln.Name, ln.Name, ln.Project, ln})
			for _, b := range ln.Children {
				nodes = append(nodes, nodeRow{"bucket", ln.Name, b.Name, ln.Project, b})
			}
		}
		for i := 0; i < len(nodes); i += 200 {
			end := min(i+200, len(nodes))
			var sb strings.Builder
			var args []interface{}
			sb.WriteString(`INSERT INTO ci_run_nodes (ci_run_id, level, lane_key, key, project, branch, sha, started_at, duration_ms, status,
				total, passed, failed, skipped, known_gap, defect) VALUES `)
			for j, nr := range nodes[i:end] {
				if j > 0 {
					sb.WriteString(",")
				}
				sb.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
				st := started
				if nr.n.StartedAt != nil {
					st = *nr.n.StartedAt
				}
				nc := nr.n.Counts
				args = append(args, sum.CIRunID, nr.level, nr.laneKey, nr.key, nr.project, sum.Branch, sum.SHA, st, nr.n.DurationMS, nr.n.Status,
					nc.Total, nc.Passed, nc.Failed, nc.Skipped, nc.KnownGap, nc.Defect)
			}
			sb.WriteString(" ON CONFLICT DO NOTHING")
			if err := tx.Exec(sb.String(), args...).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Backfill rebuilds the summary of every CI run with a test run in the last
// days (0 = all), newest first, and records the summary version when it
// covered everything. It returns the number of CI runs built.
func (s *RunsStore) Backfill(ctx context.Context, days int) (int, error) {
	q := `SELECT ` + trCols + ` FROM test_runs WHERE deleted_at IS NULL`
	var args []interface{}
	if days > 0 {
		q += ` AND start_time > ?`
		args = append(args, time.Now().Add(-time.Duration(days)*24*time.Hour))
	}
	var rows []trRow
	if err := s.db.Raw(q, args...).Scan(&rows).Error; err != nil {
		return 0, err
	}
	latest := map[string]time.Time{}
	for _, r := range rows {
		id := CIRunIDOf(r.toRun())
		if r.StartTime.After(latest[id]) {
			latest[id] = r.StartTime
		}
	}
	ids := make([]string, 0, len(latest))
	for id := range latest {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return latest[ids[i]].After(latest[ids[j]]) })
	// Map every test run first so LoadRun finds build_url-grouped ones.
	for i := 0; i < len(rows); i += 1000 {
		end := min(i+1000, len(rows))
		var sb strings.Builder
		var a []interface{}
		sb.WriteString(`INSERT INTO ci_run_test_runs (test_run_id, ci_run_id) VALUES `)
		for j, r := range rows[i:end] {
			if j > 0 {
				sb.WriteString(",")
			}
			sb.WriteString("(?,?)")
			a = append(a, r.ID, CIRunIDOf(r.toRun()))
		}
		sb.WriteString(` ON CONFLICT (test_run_id) DO UPDATE SET ci_run_id = EXCLUDED.ci_run_id`)
		if err := s.db.Exec(sb.String(), a...).Error; err != nil {
			return 0, err
		}
	}
	start := time.Now()
	for i, id := range ids {
		if ctx.Err() != nil {
			return i, ctx.Err()
		}
		if err := s.Refresh(id); err != nil {
			s.logger.WithError(err).Warn("runs backfill: refresh " + id)
		}
		if (i+1)%500 == 0 {
			s.logger.WithFields(map[string]interface{}{"done": i + 1, "of": len(ids), "elapsed": time.Since(start).String()}).Info("runs summary backfill")
		}
	}
	if days == 0 {
		s.db.Exec(`INSERT INTO ci_runs_state (key, value, updated_at) VALUES ('summary_version', ?, now())
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, strconv.Itoa(RunsSummaryVersion))
	}
	return len(ids), nil
}
