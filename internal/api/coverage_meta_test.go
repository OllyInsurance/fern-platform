package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/guidewire-oss/fern-platform/pkg/config"
	"github.com/guidewire-oss/fern-platform/pkg/logging"
)

// SQLite twins of migrations 000027-000029: the routes use only SQL both
// databases speak, so they run here against a real database with no server.
var coverageTestDDL = []string{
	`CREATE TABLE requirement_specs (spec_key TEXT PRIMARY KEY, source TEXT NOT NULL DEFAULT 'linear', title TEXT NOT NULL, url TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL DEFAULT '', author TEXT NOT NULL DEFAULT '', source_created_at DATETIME, synced_at DATETIME, position INT NOT NULL DEFAULT 0)`,
	`CREATE TABLE requirement_criteria (spec_key TEXT NOT NULL, criterion_id TEXT NOT NULL, kind TEXT NOT NULL DEFAULT 'ac', title TEXT NOT NULL DEFAULT '',
		quote TEXT NOT NULL DEFAULT '', build_status TEXT NOT NULL DEFAULT '', build_evidence TEXT NOT NULL DEFAULT '',
		gap_category TEXT NOT NULL DEFAULT '', gap_reason TEXT NOT NULL DEFAULT '', gap_pathway TEXT NOT NULL DEFAULT '',
		gap_ticket TEXT NOT NULL DEFAULT '', gap_source TEXT NOT NULL DEFAULT '', position INT NOT NULL DEFAULT 0, PRIMARY KEY (spec_key, criterion_id))`,
	`CREATE TABLE requirement_test_cases (test_key TEXT PRIMARY KEY, repo TEXT NOT NULL, framework TEXT NOT NULL, file TEXT NOT NULL, line INT NOT NULL DEFAULT 0,
		name TEXT NOT NULL, fern_project TEXT NOT NULL DEFAULT '', match_name TEXT NOT NULL DEFAULT '', match_mode TEXT NOT NULL DEFAULT 'exact',
		match_hint TEXT NOT NULL DEFAULT '', what TEXT NOT NULL DEFAULT '', how TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '',
		confidence TEXT NOT NULL DEFAULT '', evidence TEXT NOT NULL DEFAULT '', url TEXT NOT NULL DEFAULT '', parent_key TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE requirement_links (test_key TEXT NOT NULL, spec_key TEXT NOT NULL, criterion_id TEXT NOT NULL DEFAULT '', PRIMARY KEY (test_key, spec_key, criterion_id))`,
	`CREATE TABLE requirement_imports (id INTEGER PRIMARY KEY AUTOINCREMENT, source TEXT NOT NULL DEFAULT '', git_sha TEXT NOT NULL DEFAULT '', specs INT NOT NULL DEFAULT 0,
		criteria INT NOT NULL DEFAULT 0, tests INT NOT NULL DEFAULT 0, links INT NOT NULL DEFAULT 0, imported_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
	`CREATE TABLE requirement_meta (obj_kind TEXT NOT NULL, obj_key TEXT NOT NULL, data TEXT NOT NULL DEFAULT '{}', updated_at DATETIME, updated_by TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (obj_kind, obj_key))`,
	`CREATE TABLE coverage_boards (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, kind TEXT NOT NULL, definition TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME, updated_at DATETIME, updated_by TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE coverage_board_items (id INTEGER PRIMARY KEY AUTOINCREMENT, board_id INTEGER NOT NULL REFERENCES coverage_boards(id), title TEXT NOT NULL DEFAULT '',
		start_date TEXT NOT NULL DEFAULT '', end_date TEXT NOT NULL DEFAULT '', depends_on TEXT NOT NULL DEFAULT '[]', bucket TEXT NOT NULL DEFAULT '',
		refs TEXT NOT NULL DEFAULT '[]', metadata TEXT NOT NULL DEFAULT '{}', sort REAL NOT NULL DEFAULT 0, created_at DATETIME, updated_at DATETIME,
		updated_by TEXT NOT NULL DEFAULT '')`,
}

type covEnv struct {
	t      *testing.T
	router *gin.Engine
	db     *gorm.DB
}

func newCovEnv(t *testing.T) *covEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	for _, q := range coverageTestDDL {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	lg, _ := logging.NewLogger(&config.LoggingConfig{Level: "error", Format: "json"})
	r := gin.New()
	NewCoverageHandler(db, lg).RegisterRoutes(r.Group("/api/v1"))
	return &covEnv{t: t, router: r, db: db}
}

// do sends a request as user "ana" and decodes a JSON reply into out.
func (e *covEnv) do(method, path, body string, wantCode int, out interface{}) string {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Olly-User", "ana")
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	if w.Code != wantCode {
		e.t.Fatalf("%s %s: got %d %s, want %d", method, path, w.Code, w.Body.String(), wantCode)
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			e.t.Fatalf("%s %s: %v in %s", method, path, err, w.Body.String())
		}
	}
	return w.Body.String()
}

func dataOf(t *testing.T, raw json.RawMessage) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not an object: %s", raw)
	}
	return m
}

func TestMetaGetPutPatchList(t *testing.T) {
	e := newCovEnv(t)
	var rec metaRecord

	// Nothing stored reads as an empty object.
	e.do("GET", "/api/v1/meta/spec/ENG-465", "", 200, &rec)
	if string(rec.Data) != "{}" || rec.UpdatedAt != nil {
		t.Fatalf("empty get = %+v", rec)
	}

	e.do("PUT", "/api/v1/meta/criterion/ENG-465:AC-06", `{"data":{"owner":"sam","notes":"first","due":"2026-11-01"}}`, 200, &rec)
	if rec.UpdatedBy != "ana" || rec.UpdatedAt == nil || rec.Kind != "criterion" || rec.Key != "ENG-465:AC-06" {
		t.Fatalf("put = %+v", rec)
	}

	// PATCH: shallow merge, null deletes, other keys kept.
	e.do("PATCH", "/api/v1/meta/criterion/ENG-465:AC-06", `{"data":{"owner":"kim","due":null,"tags":["a"]}}`, 200, &rec)
	d := dataOf(t, rec.Data)
	if d["owner"] != "kim" || d["notes"] != "first" || d["due"] != nil || len(d) != 3 {
		t.Fatalf("patch = %v", d)
	}
	e.do("GET", "/api/v1/meta/criterion/ENG-465:AC-06", "", 200, &rec)
	if d := dataOf(t, rec.Data); d["owner"] != "kim" || len(d) != 3 {
		t.Fatalf("get after patch = %v", d)
	}

	// PUT replaces the whole object.
	e.do("PUT", "/api/v1/meta/criterion/ENG-465:AC-06", `{"data":{"only":1}}`, 200, &rec)
	if d := dataOf(t, rec.Data); len(d) != 1 || d["only"] != float64(1) {
		t.Fatalf("put replace = %v", d)
	}

	// PATCH on nothing creates it; a test key with slashes and spaces works.
	key := "app:e2e/journeys/account/email-live.spec.ts:ENG-525 · update › S02 closes"
	e.do("PATCH", "/api/v1/meta/test/"+strings.ReplaceAll(key, " ", "%20"), `{"data":{"flaky":true,"gone":null}}`, 200, &rec)
	if rec.Key != key || len(dataOf(t, rec.Data)) != 1 {
		t.Fatalf("patch new test key = %+v", rec)
	}

	var list []metaRecord
	e.do("GET", "/api/v1/meta/criterion", "", 200, &list)
	if len(list) != 1 || list[0].Key != "ENG-465:AC-06" {
		t.Fatalf("list = %+v", list)
	}
	e.do("GET", "/api/v1/meta/test", "", 200, &list)
	if len(list) != 1 || list[0].Key != key {
		t.Fatalf("list tests = %+v", list)
	}

	// Refusals.
	e.do("GET", "/api/v1/meta/owner/x", "", 400, nil)
	e.do("GET", "/api/v1/meta/owner", "", 400, nil)
	e.do("PUT", "/api/v1/meta/spec/ENG-1", `{"data":[1,2]}`, 400, nil)
	e.do("PATCH", "/api/v1/meta/spec/ENG-1", `{"data":"x"}`, 400, nil)
	e.do("PUT", "/api/v1/meta/spec/ENG-1", `not json`, 400, nil)
}

func TestMetaBulk(t *testing.T) {
	e := newCovEnv(t)
	e.do("PUT", "/api/v1/meta/spec/ENG-1", `{"data":{"owner":"sam","keep":1}}`, 200, nil)
	var out struct {
		Updated int          `json:"updated"`
		Items   []metaRecord `json:"items"`
	}
	e.do("POST", "/api/v1/meta:bulk", `{"items":[
		{"kind":"spec","key":"ENG-1","data":{"owner":"kim"}},
		{"kind":"gap","key":"ENG-1:S01","data":{"eta":"Q4"},"mode":"replace"},
		{"kind":"spec","key":"ENG-2","data":{"x":1}}]}`, 200, &out)
	if out.Updated != 3 {
		t.Fatalf("bulk = %+v", out)
	}
	var rec metaRecord
	e.do("GET", "/api/v1/meta/spec/ENG-1", "", 200, &rec)
	if d := dataOf(t, rec.Data); d["owner"] != "kim" || d["keep"] != float64(1) {
		t.Fatalf("bulk merge default = %v", d)
	}
	e.do("POST", "/api/v1/meta:bulk", `{"items":[{"kind":"spec","key":"ENG-1","data":{"keep":null}},{"kind":"spec","key":"ENG-1","data":{},"mode":"replace"}]}`, 200, nil)
	e.do("GET", "/api/v1/meta/spec/ENG-1", "", 200, &rec)
	if string(rec.Data) != "{}" {
		t.Fatalf("bulk replace = %s", rec.Data)
	}

	// One bad item refuses the whole batch: nothing is written.
	e.do("POST", "/api/v1/meta:bulk", `{"items":[{"kind":"spec","key":"ENG-9","data":{"a":1}},{"kind":"nope","key":"k","data":{}}]}`, 400, nil)
	e.do("POST", "/api/v1/meta:bulk", `{"items":[{"kind":"spec","key":"ENG-9","data":{"a":1}},{"kind":"spec","key":"k","data":{},"mode":"upsert"}]}`, 400, nil)
	e.do("GET", "/api/v1/meta/spec/ENG-9", "", 200, &rec)
	if string(rec.Data) != "{}" {
		t.Fatalf("refused batch wrote %s", rec.Data)
	}
}

const registrySnapshot = `{"source":"t","git_sha":"abc","specs":[{"key":"ENG-465","title":"Claims","criteria":[
	{"id":"AC-06","quote":"q6","gap":{"category":"not_built","reason":"no code","pathway":"build it","ticket":"OllyInsurance/olly#1","source":"x_test.go:9"}},
	{"id":"AC-07","quote":"q7"}]}],
	"tests":[{"key":"olly:t.go:TestA","repo":"olly","framework":"go","file":"t.go","line":3,"name":"TestA","links":[{"spec":"ENG-465","criteria":["AC-07"]}]},
		{"key":"olly:t.go:TestA/S07","repo":"olly","framework":"go","file":"t.go","line":9,"name":"TestA/S07","parent":"olly:t.go:TestA","links":[]}]}`

// Metadata lives beside the registry: a re-import replaces specs, criteria
// and tests but never the metadata, and the coverage read merges it in.
func TestMetadataSurvivesReimportAndJoinsCoverage(t *testing.T) {
	e := newCovEnv(t)
	e.do("PUT", "/api/v1/requirements", registrySnapshot, 200, nil)
	e.do("PUT", "/api/v1/meta/spec/ENG-465", `{"data":{"owner":"sam"}}`, 200, nil)
	e.do("PUT", "/api/v1/meta/criterion/ENG-465:AC-06", `{"data":{"priority":"high"}}`, 200, nil)
	e.do("PUT", "/api/v1/meta/gap/ENG-465:AC-06", `{"data":{"eta":"2026-11-01"}}`, 200, nil)
	e.do("PUT", "/api/v1/meta/test/olly:t.go:TestA", `{"data":{"flaky":false}}`, 200, nil)

	e.do("PUT", "/api/v1/requirements", registrySnapshot, 200, nil)
	e.do("PUT", "/api/v1/requirements", registrySnapshot, 200, nil)

	var cov struct {
		Summary coverageSummary `json:"summary"`
		Specs   []struct {
			Key      string          `json:"key"`
			Metadata json.RawMessage `json:"metadata"`
			Summary  coverageSummary `json:"summary"`
			Criteria []struct {
				ID          string          `json:"id"`
				Verdict     string          `json:"verdict"`
				VerdictFrom *verdictSource  `json:"verdict_from"`
				Gap         *coverageGap    `json:"gap"`
				Metadata    json.RawMessage `json:"metadata"`
				GapMetadata json.RawMessage `json:"gap_metadata"`
				Tests       []string        `json:"tests"`
			} `json:"criteria"`
		} `json:"specs"`
		Tests []struct {
			Key      string          `json:"key"`
			Parent   string          `json:"parent"`
			Verdict  string          `json:"verdict"`
			Metadata json.RawMessage `json:"metadata"`
		} `json:"tests"`
	}
	e.do("GET", "/api/v1/requirements/coverage", "", 200, &cov)
	if len(cov.Specs) != 1 || dataOf(t, cov.Specs[0].Metadata)["owner"] != "sam" {
		t.Fatalf("spec metadata lost: %+v", cov.Specs)
	}
	ac6, ac7 := cov.Specs[0].Criteria[0], cov.Specs[0].Criteria[1]
	if dataOf(t, ac6.Metadata)["priority"] != "high" || dataOf(t, ac6.GapMetadata)["eta"] != "2026-11-01" {
		t.Fatalf("criterion metadata lost: %s %s", ac6.Metadata, ac6.GapMetadata)
	}
	if ac6.Gap == nil || ac6.Gap.Category != "not_built" || ac6.Gap.Pathway != "build it" || ac6.Gap.Source != "x_test.go:9" || ac7.Gap != nil {
		t.Fatalf("gap = %+v / %+v", ac6.Gap, ac7.Gap)
	}
	if string(ac7.Metadata) != "{}" || string(ac7.GapMetadata) != "{}" {
		t.Fatalf("missing metadata should be {}: %s %s", ac7.Metadata, ac7.GapMetadata)
	}
	// AC-06 has no test; AC-07's test has no Fern project, so no result.
	if ac6.Verdict != VerdictUntested || ac6.VerdictFrom != nil || ac7.Verdict != VerdictNotRun || ac7.VerdictFrom == nil || ac7.VerdictFrom.TestKey != "olly:t.go:TestA" {
		t.Fatalf("verdicts %s %+v / %s %+v", ac6.Verdict, ac6.VerdictFrom, ac7.Verdict, ac7.VerdictFrom)
	}
	if cov.Summary.Verdicts[VerdictUntested] != 1 || cov.Summary.Verdicts[VerdictNotRun] != 1 || cov.Summary.Gaps["not_built"] != 1 || cov.Summary.Unexplained != 1 {
		t.Fatalf("summary %+v", cov.Summary)
	}
	if cov.Specs[0].Summary.Criteria != 2 || cov.Specs[0].Summary.Verdicts[VerdictPassing] != 0 {
		t.Fatalf("spec summary %+v", cov.Specs[0].Summary)
	}
	if len(cov.Tests) != 2 || dataOf(t, cov.Tests[0].Metadata)["flaky"] != false || cov.Tests[0].Verdict != VerdictNotRun {
		t.Fatalf("tests %+v", cov.Tests)
	}
	if cov.Tests[0].Parent != "" || cov.Tests[1].Parent != "olly:t.go:TestA" {
		t.Fatalf("parent not stored: %+v", cov.Tests)
	}
}

func TestBoardsCRUD(t *testing.T) {
	e := newCovEnv(t)
	var b board
	e.do("POST", "/api/v1/boards", `{"name":"Q4 gaps","kind":"buckets","definition":{"group_by":"metadata.owner","filters":{"verdict":["known_gap"]}}}`, 201, &b)
	if b.ID == 0 || b.UpdatedBy != "ana" || dataOf(t, b.Definition)["group_by"] != "metadata.owner" {
		t.Fatalf("create = %+v", b)
	}
	id := fmt.Sprint(b.ID)
	e.do("GET", "/api/v1/boards/"+id, "", 200, &b)
	if b.Name != "Q4 gaps" || b.Kind != "buckets" {
		t.Fatalf("get = %+v", b)
	}
	e.do("PUT", "/api/v1/boards/"+id, `{"name":"Plan","kind":"gantt","definition":{"columns":["a"]}}`, 200, &b)
	e.do("GET", "/api/v1/boards/"+id, "", 200, &b)
	if b.Name != "Plan" || b.Kind != "gantt" || dataOf(t, b.Definition)["group_by"] != nil {
		t.Fatalf("put = %+v", b)
	}
	var list []board
	e.do("POST", "/api/v1/boards", `{"name":"Other","kind":"gantt"}`, 201, nil)
	e.do("GET", "/api/v1/boards", "", 200, &list)
	if len(list) != 2 || string(list[1].Definition) != "{}" {
		t.Fatalf("list = %+v", list)
	}
	e.do("POST", "/api/v1/boards", `{"name":"x","kind":"kanban"}`, 400, nil)
	e.do("POST", "/api/v1/boards", `{"name":" ","kind":"gantt"}`, 400, nil)
	e.do("POST", "/api/v1/boards", `{"name":"x","kind":"gantt","definition":[1]}`, 400, nil)
	e.do("GET", "/api/v1/boards/999", "", 404, nil)
	e.do("GET", "/api/v1/boards/abc", "", 400, nil)
	e.do("PUT", "/api/v1/boards/999", `{"name":"x","kind":"gantt"}`, 404, nil)
}

func TestBoardItemsCRUD(t *testing.T) {
	e := newCovEnv(t)
	var b board
	e.do("POST", "/api/v1/boards", `{"name":"Plan","kind":"gantt"}`, 201, &b)
	base := fmt.Sprintf("/api/v1/boards/%d/items", b.ID)

	var a, c boardItem
	e.do("POST", base, `{"title":"Build chips","start":"2026-10-05","end":"2026-10-20","bucket":"now","refs":[{"kind":"criterion","key":"ENG-630:NFR-02"}],"metadata":{"owner":"sam","size":"M"},"sort":1}`, 201, &a)
	if a.ID == 0 || a.BoardID != b.ID || a.UpdatedBy != "ana" || len(a.Refs) != 1 || len(a.DependsOn) != 0 {
		t.Fatalf("create = %+v", a)
	}
	e.do("POST", base, fmt.Sprintf(`{"title":"Test chips","depends_on":[%d,%d],"sort":2}`, a.ID, a.ID), 201, &c)
	if len(c.DependsOn) != 1 || c.DependsOn[0] != a.ID || string(c.Metadata) != "{}" {
		t.Fatalf("create dependent = %+v", c)
	}

	var got boardItem
	e.do("GET", fmt.Sprintf("%s/%d", base, a.ID), "", 200, &got)
	if got.Title != "Build chips" || got.Start != "2026-10-05" || got.Refs[0].Key != "ENG-630:NFR-02" {
		t.Fatalf("get = %+v", got)
	}

	// PATCH: only the sent fields; metadata merged with null-deletes.
	e.do("PATCH", fmt.Sprintf("%s/%d", base, a.ID), `{"end":"2026-10-25","metadata":{"size":null,"risk":"low"}}`, 200, &got)
	e.do("GET", fmt.Sprintf("%s/%d", base, a.ID), "", 200, &got)
	m := dataOf(t, got.Metadata)
	if got.End != "2026-10-25" || got.Title != "Build chips" || got.Bucket != "now" || m["owner"] != "sam" || m["risk"] != "low" || m["size"] != nil {
		t.Fatalf("patch = %+v %v", got, m)
	}

	// PUT replaces every field.
	e.do("PUT", fmt.Sprintf("%s/%d", base, a.ID), `{"title":"Chips","sort":5}`, 200, &got)
	e.do("GET", fmt.Sprintf("%s/%d", base, a.ID), "", 200, &got)
	if got.Title != "Chips" || got.Start != "" || got.Bucket != "" || len(got.Refs) != 0 || string(got.Metadata) != "{}" || got.Sort != 5 {
		t.Fatalf("put = %+v", got)
	}

	var items []boardItem
	e.do("GET", base, "", 200, &items)
	if len(items) != 2 || items[0].ID != c.ID { // sort 2 before sort 5
		t.Fatalf("list = %+v", items)
	}

	// Refusals.
	e.do("POST", base, `{"title":"x","start":"10/05/2026"}`, 400, nil)
	e.do("POST", base, `{"title":"x","start":"2026-10-10","end":"2026-10-01"}`, 400, nil)
	e.do("POST", base, `{"title":"x","depends_on":[999]}`, 400, nil)
	e.do("POST", base, `{"title":"x","refs":[{"kind":"owner","key":"k"}]}`, 400, nil)
	e.do("POST", base, `{"title":"x","metadata":[1]}`, 400, nil)
	e.do("PATCH", fmt.Sprintf("%s/%d", base, a.ID), fmt.Sprintf(`{"depends_on":[%d]}`, a.ID), 400, nil)
	e.do("PATCH", fmt.Sprintf("%s/%d", base, a.ID), `{"colour":"red"}`, 400, nil)
	e.do("GET", base+"/999", "", 404, nil)
	e.do("POST", "/api/v1/boards/999/items", `{"title":"x"}`, 404, nil)

	// An item on another board is not reachable through this one.
	var other board
	e.do("POST", "/api/v1/boards", `{"name":"Other","kind":"buckets"}`, 201, &other)
	e.do("GET", fmt.Sprintf("/api/v1/boards/%d/items/%d", other.ID, a.ID), "", 404, nil)
	e.do("POST", fmt.Sprintf("/api/v1/boards/%d/items", other.ID), fmt.Sprintf(`{"title":"x","depends_on":[%d]}`, a.ID), 400, nil)

	// Deleting an item drops it from the others' depends_on.
	e.do("DELETE", fmt.Sprintf("%s/%d", base, a.ID), "", 200, nil)
	e.do("GET", fmt.Sprintf("%s/%d", base, a.ID), "", 404, nil)
	e.do("GET", fmt.Sprintf("%s/%d", base, c.ID), "", 200, &got)
	if len(got.DependsOn) != 0 {
		t.Fatalf("dependency not dropped: %+v", got)
	}
}

func TestBoardDeleteCascadesItems(t *testing.T) {
	e := newCovEnv(t)
	var b, keep board
	e.do("POST", "/api/v1/boards", `{"name":"Plan","kind":"gantt"}`, 201, &b)
	e.do("POST", "/api/v1/boards", `{"name":"Keep","kind":"gantt"}`, 201, &keep)
	for i := 0; i < 3; i++ {
		e.do("POST", fmt.Sprintf("/api/v1/boards/%d/items", b.ID), `{"title":"x"}`, 201, nil)
	}
	e.do("POST", fmt.Sprintf("/api/v1/boards/%d/items", keep.ID), `{"title":"kept"}`, 201, nil)

	var del struct {
		Deleted      int64 `json:"deleted"`
		ItemsDeleted int64 `json:"items_deleted"`
	}
	e.do("DELETE", fmt.Sprintf("/api/v1/boards/%d", b.ID), "", 200, &del)
	if del.Deleted != b.ID || del.ItemsDeleted != 3 {
		t.Fatalf("delete = %+v", del)
	}
	e.do("GET", fmt.Sprintf("/api/v1/boards/%d", b.ID), "", 404, nil)
	e.do("GET", fmt.Sprintf("/api/v1/boards/%d/items", b.ID), "", 404, nil)
	var n int64
	e.db.Raw(`SELECT count(*) FROM coverage_board_items WHERE board_id = ?`, b.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d items left behind", n)
	}
	var items []boardItem
	e.do("GET", fmt.Sprintf("/api/v1/boards/%d/items", keep.ID), "", 200, &items)
	if len(items) != 1 {
		t.Fatalf("other board lost items: %+v", items)
	}
	e.do("DELETE", fmt.Sprintf("/api/v1/boards/%d", b.ID), "", 404, nil)
}
