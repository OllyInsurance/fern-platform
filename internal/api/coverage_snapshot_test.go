package api

import (
	"testing"
	"time"
)

// A snapshot row carries the decided verdict, the registry gap and the spec
// title of each criterion, in registry order.
func TestSnapshotRows(t *testing.T) {
	pass := &coverageTest{Key: "a", Name: "TestA", Latest: &CoverageResult{Status: "passed", SpecName: "TestA"}}
	fail := &coverageTest{Key: "b", Name: "TestB", Latest: &CoverageResult{Status: "failed", SpecName: "TestB"}}
	gap := &coverageTest{Key: "c", Name: "TestC", Latest: &CoverageResult{Status: "skipped", SpecName: "TestC", GapMarker: "known_gap"}}
	byKey := map[string]*coverageTest{"a": pass, "b": fail, "c": gap}
	linkAncestors(byKey)
	specs := []coverageSpec{
		{Key: "ENG-1", Title: "Claims", Criteria: []coverageCriterion{
			{ID: "AC-01", Kind: "ac", BuildStatus: "built", Tests: []string{"a"}},
			{ID: "AC-02", Kind: "ac", Tests: []string{"a", "b"}},
			{ID: "NFR-01", Kind: "nfr", Tests: []string{"c"}, gapCategory: "test_infra", gapReason: "no harness"},
		}},
		{Key: "ENG-2", Title: "Billing", Criteria: []coverageCriterion{
			{ID: "AC-01", Kind: "scenario", gapCategory: "not_built"},
		}},
	}
	summarize(specs, byKey)
	got := snapshotRows(specs)
	want := []coverageSnapshotRow{
		{SpecKey: "ENG-1", CriterionID: "AC-01", Kind: "ac", SpecTitle: "Claims", Verdict: VerdictPassing, BuildStatus: "built"},
		{SpecKey: "ENG-1", CriterionID: "AC-02", Kind: "ac", SpecTitle: "Claims", Verdict: VerdictFailing},
		{SpecKey: "ENG-1", CriterionID: "NFR-01", Kind: "nfr", SpecTitle: "Claims", Verdict: VerdictKnownGap, GapCategory: "test_infra"},
		{SpecKey: "ENG-2", CriterionID: "AC-01", Kind: "scenario", SpecTitle: "Billing", Verdict: VerdictUntested, GapCategory: "not_built"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if rows := snapshotRows(nil); len(rows) != 0 {
		t.Fatalf("no specs gave %d rows", len(rows))
	}
}

type snapRow struct {
	Day, SpecKey, CriterionID, Kind, SpecTitle, Verdict, GapCategory string
}

func (e *covEnv) snapshotRows(day string) []snapRow {
	e.t.Helper()
	var rows []snapRow
	if err := e.db.Raw(`SELECT CAST(snapshot_date AS TEXT) AS day, spec_key, criterion_id, kind, spec_title, verdict, gap_category
		FROM coverage_snapshots WHERE snapshot_date = ? ORDER BY spec_key, criterion_id`, day).Scan(&rows).Error; err != nil {
		e.t.Fatal(err)
	}
	return rows
}

// An import writes today's snapshot; writing again the same day replaces the
// day's rows (a criterion the registry dropped is gone, nothing duplicates);
// other days are kept; the list endpoint counts per day.
func TestSnapshotOnImportReplacesTheDay(t *testing.T) {
	e := newCovEnv(t)
	today := time.Now().UTC().Format("2006-01-02")

	e.do("PUT", "/api/v1/requirements", registrySnapshot, 200, nil)
	rows := e.snapshotRows(today)
	if len(rows) != 2 {
		t.Fatalf("after import: %+v", rows)
	}
	if r := rows[0]; r.SpecKey != "ENG-465" || r.CriterionID != "AC-06" || r.Kind != "ac" || r.SpecTitle != "Claims" || r.Verdict != VerdictUntested || r.GapCategory != "not_built" {
		t.Fatalf("AC-06 row %+v", r)
	}
	if r := rows[1]; r.CriterionID != "AC-07" || r.Verdict != VerdictNotRun || r.GapCategory != "" {
		t.Fatalf("AC-07 row %+v", r)
	}

	// An older day stays when today is rewritten.
	if err := e.db.Exec(`INSERT INTO coverage_snapshots (snapshot_date, spec_key, criterion_id, verdict) VALUES ('2026-01-01','ENG-465','AC-06','passing'),('2026-01-01','ENG-465','AC-07','passing')`).Error; err != nil {
		t.Fatal(err)
	}
	h := NewCoverageHandler(e.db, nil)
	for i := 0; i < 2; i++ {
		if n, err := h.WriteSnapshot(time.Now()); err != nil || n != 2 {
			t.Fatalf("rewrite %d: n=%d err=%v", i, n, err)
		}
	}
	if rows := e.snapshotRows(today); len(rows) != 2 {
		t.Fatalf("rewrite duplicated: %+v", rows)
	}

	// The registry loses AC-07: today's rows follow it.
	one := `{"specs":[{"key":"ENG-465","title":"Claims","criteria":[{"id":"AC-06","quote":"q6"}]}],"tests":[]}`
	e.do("PUT", "/api/v1/requirements", one, 200, nil)
	if rows := e.snapshotRows(today); len(rows) != 1 || rows[0].CriterionID != "AC-06" || rows[0].GapCategory != "" {
		t.Fatalf("after re-import: %+v", rows)
	}
	if rows := e.snapshotRows("2026-01-01"); len(rows) != 2 {
		t.Fatalf("older day lost: %+v", rows)
	}

	var list struct {
		Days []snapshotDay `json:"days"`
	}
	e.do("GET", "/api/v1/requirements/coverage/snapshots?days=730", "", 200, &list)
	if len(list.Days) != 2 || list.Days[0].Date != "2026-01-01" || list.Days[0].Verdicts[VerdictPassing] != 2 ||
		list.Days[1].Date != today || list.Days[1].Criteria != 1 || list.Days[1].Verdicts[VerdictUntested] != 1 {
		t.Fatalf("list %+v", list.Days)
	}
	var made struct {
		Date string `json:"date"`
		Rows int    `json:"rows"`
	}
	e.do("POST", "/api/v1/requirements/coverage/snapshots", "", 200, &made)
	if made.Date != today || made.Rows != 1 {
		t.Fatalf("manual snapshot %+v", made)
	}
}

// A snapshot that cannot be written never fails the import.
func TestSnapshotFailureDoesNotFailImport(t *testing.T) {
	e := newCovEnv(t)
	if err := e.db.Exec(`DROP TABLE coverage_snapshots`).Error; err != nil {
		t.Fatal(err)
	}
	e.do("PUT", "/api/v1/requirements", registrySnapshot, 200, nil)
}
