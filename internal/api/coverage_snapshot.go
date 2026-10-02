package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Coverage snapshots: the verdict and gap of every criterion, kept per day,
// so coverage can be drawn over time. Verdicts are decided on request from
// the latest results; a snapshot records what the coverage page showed that
// day (branch main, last 30 days). A day's rows are replaced whole on each
// write, so the last write of a day wins and a rerun never duplicates.

// SnapshotInterval is how often the background job rewrites today's snapshot.
const SnapshotInterval = 6 * time.Hour

// coverageSnapshotRow is one criterion on one day.
type coverageSnapshotRow struct {
	SpecKey     string
	CriterionID string
	Kind        string
	SpecTitle   string
	Verdict     string
	GapCategory string
	BuildStatus string
}

// snapshotRows flattens decided specs into one row per criterion. The gap
// category is the registry's, whatever the verdict, so a chart can show what
// a passing criterion used to be blocked by as well as what blocks the rest.
func snapshotRows(specs []coverageSpec) []coverageSnapshotRow {
	var out []coverageSnapshotRow
	for _, s := range specs {
		for _, cr := range s.Criteria {
			gap := ""
			if cr.Gap != nil {
				gap = cr.Gap.Category
			}
			out = append(out, coverageSnapshotRow{SpecKey: s.Key, CriterionID: cr.ID, Kind: cr.Kind, SpecTitle: s.Title,
				Verdict: cr.Verdict, GapCategory: gap, BuildStatus: cr.BuildStatus})
		}
	}
	return out
}

// WriteSnapshot decides today's coverage (UTC) with the default window and
// replaces that day's rows. It returns the number of rows written.
func (h *CoverageHandler) WriteSnapshot(now time.Time) (int, error) {
	h.snapMu.Lock()
	defer h.snapMu.Unlock()
	rep, err := h.computeCoverage(DefaultCoverageBranch, DefaultCoverageDays)
	if err != nil {
		return 0, err
	}
	rows := snapshotRows(rep.specs)
	day := now.UTC().Format("2006-01-02")
	taken := now.UTC()
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`DELETE FROM coverage_snapshots WHERE snapshot_date = ?`, day).Error; err != nil {
			return err
		}
		for _, r := range rows {
			if err := tx.Exec(`INSERT INTO coverage_snapshots (snapshot_date, spec_key, criterion_id, kind, spec_title, verdict, gap_category, build_status, taken_at)
				VALUES (?,?,?,?,?,?,?,?,?)`, day, r.SpecKey, r.CriterionID, r.Kind, r.SpecTitle, r.Verdict, r.GapCategory, r.BuildStatus, taken).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// snapshotLogged writes today's snapshot and logs the outcome; a failure never
// reaches the caller (an import that succeeded stays a success).
func (h *CoverageHandler) snapshotLogged(why string) {
	n, err := h.WriteSnapshot(time.Now())
	if h.logger == nil {
		return
	}
	if err != nil {
		h.logger.WithError(err).Error("coverage snapshot failed (" + why + ")")
		return
	}
	h.logger.WithFields(map[string]interface{}{"rows": n, "trigger": why}).Info("coverage snapshot written")
}

// StartSnapshots writes today's snapshot now (the backfill of today; rerunning
// it is harmless) and again every SnapshotInterval until ctx ends.
func (h *CoverageHandler) StartSnapshots(ctx context.Context) {
	go func() {
		h.snapshotLogged("startup")
		t := time.NewTicker(SnapshotInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.snapshotLogged("schedule")
			}
		}
	}()
}

// snapshotDay is one day's criteria per verdict.
type snapshotDay struct {
	Date     string         `json:"date"`
	Criteria int            `json:"criteria"`
	Verdicts map[string]int `json:"verdicts"`
	Gaps     map[string]int `json:"gaps"`
}

// listSnapshots returns verdict and gap counts per day, oldest first.
// Query: days (default 90, at most 730).
func (h *CoverageHandler) listSnapshots(c *gin.Context) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "90"))
	if days <= 0 || days > 730 {
		days = 90
	}
	since := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	var rows []struct {
		Day, Verdict, GapCategory string
		N                         int
	}
	if err := h.db.Raw(`SELECT CAST(snapshot_date AS TEXT) AS day, verdict, gap_category, COUNT(*) AS n FROM coverage_snapshots
		WHERE snapshot_date >= ? GROUP BY snapshot_date, verdict, gap_category ORDER BY snapshot_date, verdict, gap_category`, since).Scan(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := []snapshotDay{}
	for _, r := range rows {
		if len(out) == 0 || out[len(out)-1].Date != r.Day {
			out = append(out, snapshotDay{Date: r.Day, Verdicts: newVerdictCounts(), Gaps: map[string]int{}})
		}
		d := &out[len(out)-1]
		d.Criteria += r.N
		d.Verdicts[r.Verdict] += r.N
		if r.Verdict != VerdictPassing && r.GapCategory != "" {
			d.Gaps[r.GapCategory] += r.N
		}
	}
	c.JSON(http.StatusOK, gin.H{"days": out})
}

// postSnapshot writes today's snapshot now.
func (h *CoverageHandler) postSnapshot(c *gin.Context) {
	n, err := h.WriteSnapshot(time.Now())
	if err != nil {
		h.logger.WithError(err).Error("coverage snapshot failed (manual)")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"date": time.Now().UTC().Format("2006-01-02"), "rows": n})
}
