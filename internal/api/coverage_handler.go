// Package api provides domain-based REST API handlers
package api

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/guidewire-oss/fern-platform/pkg/logging"
)

// CoverageHandler serves requirements coverage: which spec (a Linear ticket)
// and which of its acceptance criteria each test proves, what the test does
// and how it is judged, joined to the latest result Fern holds for that test.
//
// The registry (specs, verbatim criteria, test cases, links) is imported as a
// whole snapshot from the repo that owns it (PUT /requirements); results are
// the spec_runs every reporter already sends, matched by name.
type CoverageHandler struct {
	*BaseHandler
	db *gorm.DB
}

// NewCoverageHandler creates a new requirements coverage handler.
func NewCoverageHandler(db *gorm.DB, logger *logging.Logger) *CoverageHandler {
	return &CoverageHandler{BaseHandler: NewBaseHandler(logger), db: db}
}

// RegisterRoutes registers the coverage routes on the user group: reading is
// what every viewer does, and importing is what CI does with the same token
// it already uses to submit test runs.
func (h *CoverageHandler) RegisterRoutes(userGroup *gin.RouterGroup) {
	userGroup.PUT("/requirements", h.importRegistry)
	userGroup.GET("/requirements/coverage", h.getCoverage)
}

// --- import -------------------------------------------------------------------

type registryCriterion struct {
	ID    string `json:"id" binding:"required"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Quote string `json:"quote"`
}

type registrySpec struct {
	Key       string              `json:"key" binding:"required"`
	Source    string              `json:"source"`
	Title     string              `json:"title"`
	URL       string              `json:"url"`
	State     string              `json:"state"`
	Author    string              `json:"author"`
	CreatedAt *time.Time          `json:"created_at"`
	SyncedAt  *time.Time          `json:"synced_at"`
	Criteria  []registryCriterion `json:"criteria"`
}

type registryLink struct {
	Spec     string   `json:"spec"`
	Criteria []string `json:"criteria"`
}

type registryTest struct {
	Key         string         `json:"key" binding:"required"`
	Repo        string         `json:"repo"`
	Framework   string         `json:"framework"`
	File        string         `json:"file"`
	Line        int            `json:"line"`
	Name        string         `json:"name"`
	FernProject string         `json:"fern_project"`
	MatchName   string         `json:"match_name"`
	MatchMode   string         `json:"match_mode"` // exact | suffix | regex
	MatchHint   string         `json:"match_hint"`
	What        string         `json:"what"`
	How         string         `json:"how"`
	Note        string         `json:"note"`
	Confidence  string         `json:"confidence"`
	Evidence    string         `json:"evidence"`
	URL         string         `json:"url"`
	Links       []registryLink `json:"links"`
}

type registryImport struct {
	Source string         `json:"source"`
	GitSHA string         `json:"git_sha"`
	Specs  []registrySpec `json:"specs"`
	Tests  []registryTest `json:"tests"`
}

// importRegistry replaces the whole registry with the posted snapshot, in one
// transaction, so a reader never sees half of two registries. A link to a spec
// or criterion the snapshot does not define is rejected, not dropped: a
// silently lost link reads as an untested criterion.
func (h *CoverageHandler) importRegistry(c *gin.Context) {
	var req registryImport
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	known := map[string]map[string]bool{}
	nCrit := 0
	for _, s := range req.Specs {
		// binding:"required" does not reach into slices, so check keys here.
		if s.Key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "a spec has no Key"})
			return
		}
		if known[s.Key] != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate spec " + s.Key})
			return
		}
		known[s.Key] = map[string]bool{}
		for _, cr := range s.Criteria {
			if cr.ID == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "a criterion of " + s.Key + " has no id"})
				return
			}
			if known[s.Key][cr.ID] {
				c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate criterion " + s.Key + " " + cr.ID})
				return
			}
			known[s.Key][cr.ID] = true
			nCrit++
		}
	}
	seenTests := map[string]bool{}
	nLinks := 0
	for _, t := range req.Tests {
		if t.Key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "a test has no Key"})
			return
		}
		if seenTests[t.Key] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate test " + t.Key})
			return
		}
		seenTests[t.Key] = true
		for _, l := range t.Links {
			if known[l.Spec] == nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "test " + t.Key + " links unknown spec " + l.Spec})
				return
			}
			for _, id := range l.Criteria {
				if !known[l.Spec][id] {
					c.JSON(http.StatusBadRequest, gin.H{"error": "test " + t.Key + " links unknown criterion " + l.Spec + " " + id})
					return
				}
			}
			nLinks += max(1, len(l.Criteria))
		}
	}

	err := h.db.Transaction(func(tx *gorm.DB) error {
		for _, t := range []string{"requirement_links", "requirement_test_cases", "requirement_criteria", "requirement_specs"} {
			if err := tx.Exec("DELETE FROM " + t).Error; err != nil {
				return err
			}
		}
		for i, s := range req.Specs {
			src := s.Source
			if src == "" {
				src = "linear"
			}
			if err := tx.Exec(`INSERT INTO requirement_specs (spec_key, source, title, url, state, author, source_created_at, synced_at, position)
				VALUES (?,?,?,?,?,?,?,?,?)`, s.Key, src, s.Title, s.URL, s.State, s.Author, s.CreatedAt, s.SyncedAt, i).Error; err != nil {
				return err
			}
			for j, cr := range s.Criteria {
				kind := cr.Kind
				if kind == "" {
					kind = "ac"
				}
				if err := tx.Exec(`INSERT INTO requirement_criteria (spec_key, criterion_id, kind, title, quote, position) VALUES (?,?,?,?,?,?)`,
					s.Key, cr.ID, kind, cr.Title, cr.Quote, j).Error; err != nil {
					return err
				}
			}
		}
		for _, t := range req.Tests {
			mode := t.MatchMode
			if mode == "" {
				mode = "exact"
			}
			if err := tx.Exec(`INSERT INTO requirement_test_cases (test_key, repo, framework, file, line, name, fern_project, match_name, match_mode, match_hint, what, how, note, confidence, evidence, url)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, t.Key, t.Repo, t.Framework, t.File, t.Line, t.Name, t.FernProject, t.MatchName, mode, t.MatchHint,
				t.What, t.How, t.Note, t.Confidence, t.Evidence, t.URL).Error; err != nil {
				return err
			}
			for _, l := range t.Links {
				ids := l.Criteria
				if len(ids) == 0 {
					ids = []string{""}
				}
				for _, id := range ids {
					if err := tx.Exec(`INSERT INTO requirement_links (test_key, spec_key, criterion_id) VALUES (?,?,?) ON CONFLICT DO NOTHING`,
						t.Key, l.Spec, id).Error; err != nil {
						return err
					}
				}
			}
		}
		return tx.Exec(`INSERT INTO requirement_imports (source, git_sha, specs, criteria, tests, links) VALUES (?,?,?,?,?,?)`,
			req.Source, req.GitSHA, len(req.Specs), nCrit, len(req.Tests), nLinks).Error
	})
	if err != nil {
		h.logger.WithError(err).Error("requirements import failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "import failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"specs": len(req.Specs), "criteria": nCrit, "tests": len(req.Tests), "links": nLinks})
}

// --- read -------------------------------------------------------------------

// CoverageResult is the latest result Fern holds for one test case.
type CoverageResult struct {
	Status    string    `json:"status"`
	SpecName  string    `json:"spec_name"`
	SuiteName string    `json:"suite_name"`
	TestRunID uint      `json:"test_run_id"`
	RunID     string    `json:"run_id"`
	Branch    string    `json:"branch"`
	GitSHA    string    `json:"git_sha"`
	StartTime time.Time `json:"start_time"`
	Message   string    `json:"message,omitempty"`
	Runs      int       `json:"runs"`   // matching runs in the window
	Passed    int       `json:"passed"` // of those, passed
}

type coverageTest struct {
	Key         string          `json:"key"`
	Repo        string          `json:"repo"`
	Framework   string          `json:"framework"`
	File        string          `json:"file"`
	Line        int             `json:"line"`
	Name        string          `json:"name"`
	FernProject string          `json:"fern_project"`
	What        string          `json:"what"`
	How         string          `json:"how"`
	Note        string          `json:"note"`
	Confidence  string          `json:"confidence"`
	Evidence    string          `json:"evidence"`
	URL         string          `json:"url"`
	Links       []registryLink  `json:"links"`
	Latest      *CoverageResult `json:"latest"`
	matchName   string          `json:"-"`
	matchMode   string          `json:"-"`
	matchHint   string          `json:"-"`
}

type coverageCriterion struct {
	ID    string   `json:"id"`
	Kind  string   `json:"kind"`
	Title string   `json:"title"`
	Quote string   `json:"quote"`
	Tests []string `json:"tests"`
}

type coverageSpec struct {
	Key       string              `json:"key"`
	Source    string              `json:"source"`
	Title     string              `json:"title"`
	URL       string              `json:"url"`
	State     string              `json:"state"`
	Author    string              `json:"author"`
	SyncedAt  *time.Time          `json:"synced_at"`
	Criteria  []coverageCriterion `json:"criteria"`
	SpecTests []string            `json:"spec_tests"` // linked to the spec, no single criterion
}

type specRunRow struct {
	ProjectID string
	SpecName  string
	SuiteName string
	Status    string
	TestRunID uint
	RunID     string
	Branch    string
	GitSHA    string
	StartTime time.Time
	Message   string
}

// getCoverage returns the registry with each test's latest result.
// Query: branch (default main; "any" for every branch), days (default 30).
func (h *CoverageHandler) getCoverage(c *gin.Context) {
	branch := c.DefaultQuery("branch", "main")
	days, _ := strconv.Atoi(c.DefaultQuery("days", "30"))
	if days <= 0 || days > 365 {
		days = 30
	}

	var specs []coverageSpec
	if err := h.db.Raw(`SELECT spec_key AS key, source, title, url, state, author, synced_at FROM requirement_specs ORDER BY position`).Scan(&specs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var crits []struct {
		SpecKey, CriterionID, Kind, Title, Quote string
	}
	h.db.Raw(`SELECT spec_key, criterion_id, kind, title, quote FROM requirement_criteria ORDER BY spec_key, position`).Scan(&crits)
	var links []struct{ TestKey, SpecKey, CriterionID string }
	h.db.Raw(`SELECT test_key, spec_key, criterion_id FROM requirement_links ORDER BY test_key, spec_key, criterion_id`).Scan(&links)
	var rows []struct {
		TestKey, Repo, Framework, File, Name, FernProject, MatchName, MatchMode, MatchHint, What, How, Note, Confidence, Evidence, URL string
		Line                                                                                                                           int
	}
	h.db.Raw(`SELECT test_key, repo, framework, file, line, name, fern_project, match_name, match_mode, match_hint, what, how, note, confidence, evidence, url
		FROM requirement_test_cases ORDER BY repo, file, line`).Scan(&rows)

	tests := make([]*coverageTest, 0, len(rows))
	byKey := map[string]*coverageTest{}
	projects := map[string]bool{}
	for _, r := range rows {
		t := &coverageTest{Key: r.TestKey, Repo: r.Repo, Framework: r.Framework, File: r.File, Line: r.Line, Name: r.Name,
			FernProject: r.FernProject, What: r.What, How: r.How, Note: r.Note, Confidence: r.Confidence, Evidence: r.Evidence,
			URL: r.URL, Links: []registryLink{}, matchName: r.MatchName, matchMode: r.MatchMode, matchHint: r.MatchHint}
		tests = append(tests, t)
		byKey[t.Key] = t
		if r.FernProject != "" {
			projects[r.FernProject] = true
		}
	}

	specIdx := map[string]int{}
	for i := range specs {
		specIdx[specs[i].Key] = i
		specs[i].Criteria = []coverageCriterion{}
		specs[i].SpecTests = []string{}
	}
	critIdx := map[string]int{}
	for _, cr := range crits {
		i, ok := specIdx[cr.SpecKey]
		if !ok {
			continue
		}
		critIdx[cr.SpecKey+"\x00"+cr.CriterionID] = len(specs[i].Criteria)
		specs[i].Criteria = append(specs[i].Criteria, coverageCriterion{ID: cr.CriterionID, Kind: cr.Kind, Title: cr.Title, Quote: cr.Quote, Tests: []string{}})
	}
	for _, l := range links {
		i, ok := specIdx[l.SpecKey]
		t := byKey[l.TestKey]
		if !ok || t == nil {
			continue
		}
		if l.CriterionID == "" {
			specs[i].SpecTests = append(specs[i].SpecTests, l.TestKey)
		} else if j, ok := critIdx[l.SpecKey+"\x00"+l.CriterionID]; ok {
			specs[i].Criteria[j].Tests = append(specs[i].Criteria[j].Tests, l.TestKey)
		}
		n := len(t.Links)
		if n == 0 || t.Links[n-1].Spec != l.SpecKey {
			t.Links = append(t.Links, registryLink{Spec: l.SpecKey, Criteria: []string{}})
			n++
		}
		if l.CriterionID != "" {
			t.Links[n-1].Criteria = append(t.Links[n-1].Criteria, l.CriterionID)
		}
	}

	if err := h.attachResults(tests, projects, branch, days); err != nil {
		h.logger.WithError(err).Error("coverage results lookup failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var imp struct {
		Source     string
		GitSHA     string
		ImportedAt *time.Time
	}
	h.db.Raw(`SELECT source, git_sha, imported_at FROM requirement_imports ORDER BY id DESC LIMIT 1`).Scan(&imp)

	c.JSON(http.StatusOK, gin.H{
		"generated_at": time.Now().UTC(),
		"branch":       branch,
		"days":         days,
		"import":       gin.H{"source": imp.Source, "git_sha": imp.GitSHA, "imported_at": imp.ImportedAt},
		"specs":        specs,
		"tests":        tests,
	})
}

// attachResults finds, for every test case, the newest spec run in its Fern
// project within the window whose name matches it, plus how many matching
// runs passed. Go tests match their top-level name exactly (a subtest is
// "TestX/sub", a separate row); Playwright tests match the end of the
// reported title path, and the file name must appear in it too, because
// reporters prefix the project and file differently.
func (h *CoverageHandler) attachResults(tests []*coverageTest, projects map[string]bool, branch string, days int) error {
	if len(projects) == 0 {
		return nil
	}
	ps := make([]string, 0, len(projects))
	for p := range projects {
		ps = append(ps, p)
	}
	q := `SELECT tr.project_id, sp.spec_name, sr.suite_name, sp.status, tr.id AS test_run_id, tr.run_id, tr.branch, tr.commit_sha AS git_sha,
	             COALESCE(sp.start_time, tr.start_time) AS start_time, LEFT(COALESCE(NULLIF(sp.error_message, ''), sp.description, ''), 400) AS message
	      FROM spec_runs sp
	      JOIN suite_runs sr ON sr.id = sp.suite_run_id
	      JOIN test_runs tr ON tr.id = sr.test_run_id
	      WHERE tr.project_id IN ? AND tr.start_time > ? AND sp.deleted_at IS NULL AND tr.deleted_at IS NULL`
	args := []interface{}{ps, time.Now().Add(-time.Duration(days) * 24 * time.Hour)}
	if branch != "any" {
		q += ` AND tr.branch = ?`
		args = append(args, branch)
	}
	q += ` ORDER BY tr.start_time DESC, sp.id DESC`
	var rows []specRunRow
	if err := h.db.Raw(q, args...).Scan(&rows).Error; err != nil {
		return err
	}

	exact := map[string][]int{} // project\x00name -> row indexes, newest first
	byProject := map[string][]int{}
	for i, r := range rows {
		k := r.ProjectID + "\x00" + r.SpecName
		exact[k] = append(exact[k], i)
		byProject[r.ProjectID] = append(byProject[r.ProjectID], i)
	}
	for _, t := range tests {
		if t.FernProject == "" || t.matchName == "" {
			continue
		}
		var hits []int
		if t.matchMode == "regex" {
			re, err := regexp.Compile(t.matchName)
			if err != nil {
				continue
			}
			for _, i := range byProject[t.FernProject] {
				if re.MatchString(rows[i].SpecName) {
					hits = append(hits, i)
				}
			}
		} else if t.matchMode == "suffix" {
			for _, i := range byProject[t.FernProject] {
				n := rows[i].SpecName
				if strings.HasSuffix(n, t.matchName) && (t.matchHint == "" || strings.Contains(n+" "+rows[i].SuiteName, t.matchHint)) {
					hits = append(hits, i)
				}
			}
		} else {
			hits = exact[t.FernProject+"\x00"+t.matchName]
		}
		if len(hits) == 0 {
			continue
		}
		sort.SliceStable(hits, func(a, b int) bool { return rows[hits[a]].StartTime.After(rows[hits[b]].StartTime) })
		r := rows[hits[0]]
		// A parametrised test reports several instances per run: the newest
		// run failed if any of its instances failed.
		status := r.Status
		for _, i := range hits {
			if rows[i].TestRunID != r.TestRunID {
				break
			}
			if rows[i].Status == "failed" {
				status, r = "failed", rows[i]
				break
			}
		}
		res := &CoverageResult{Status: status, SpecName: r.SpecName, SuiteName: r.SuiteName, TestRunID: r.TestRunID, RunID: r.RunID,
			Branch: r.Branch, GitSHA: r.GitSHA, StartTime: r.StartTime, Message: r.Message}
		// Per run: passed only when every matching instance in it passed.
		perRun := map[uint]string{}
		order := []uint{}
		for _, i := range hits {
			id := rows[i].TestRunID
			prev, ok := perRun[id]
			if !ok {
				order = append(order, id)
				perRun[id] = rows[i].Status
			} else if prev == "passed" && rows[i].Status != "passed" {
				perRun[id] = rows[i].Status
			}
		}
		for _, id := range order {
			res.Runs++
			if perRun[id] == "passed" {
				res.Passed++
			}
		}
		t.Latest = res
	}
	return nil
}
