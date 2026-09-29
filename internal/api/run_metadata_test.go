package api

import (
	"testing"
	"time"
)

func TestRunMetadataKeepsRequestAndBuildFields(t *testing.T) {
	req := &TestRunRequest{
		Metadata:          map[string]interface{}{"ci_run_id": "36539680188", "lane": "e2e"},
		BuildUrl:          "https://github.com/o/r/actions/runs/1",
		BuildTriggerActor: "someone",
	}
	md := runMetadata(req)
	for k, want := range map[string]string{"ci_run_id": "36539680188", "lane": "e2e", "build_url": req.BuildUrl, "build_trigger_actor": "someone"} {
		if md[k] != want {
			t.Errorf("metadata[%s] = %v, want %v", k, md[k], want)
		}
	}
	if len(runMetadata(&TestRunRequest{})) != 0 {
		t.Error("empty request must give empty metadata")
	}
}

func TestRunTimesUsesReportedSpan(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	s := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	e := s.Add(13 * time.Minute)
	start, end, dur := runTimes(&TestRunRequest{StartTime: s, EndTime: e}, now)
	if !start.Equal(s) || end == nil || !end.Equal(e) || dur != 13*time.Minute {
		t.Fatalf("got %v %v %v", start, end, dur)
	}
	start, end, dur = runTimes(&TestRunRequest{}, now)
	if !start.Equal(now) || end != nil || dur != 0 {
		t.Fatalf("fallback got %v %v %v", start, end, dur)
	}
	_, end, _ = runTimes(&TestRunRequest{StartTime: s, EndTime: s.Add(-time.Minute)}, now)
	if end != nil {
		t.Fatal("an end before the start must be ignored")
	}
}
