package tasklib

import (
	"encoding/json"
	"testing"
)

func TestGroupTasksKey(t *testing.T) {
	got := GroupTasksKey("thread-1", "design-review")
	want := "thread:thread-1:group:design-review:tasks"
	if got != want {
		t.Errorf("GroupTasksKey = %q, want %q", got, want)
	}
}

func TestGroupResultJSON(t *testing.T) {
	r := GroupResult{
		ThreadID: "thread-1",
		Label:    "design-review",
		Status:   "complete",
		Tasks: map[string]string{
			"task-a": "done",
			"task-b": "done",
		},
	}

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var r2 GroupResult
	if err := json.Unmarshal(b, &r2); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if r2.ThreadID != r.ThreadID || r2.Label != r.Label || r2.Status != r.Status {
		t.Errorf("round-trip mismatch: %+v vs %+v", r, r2)
	}
	if r2.Tasks["task-a"] != "done" || r2.Tasks["task-b"] != "done" {
		t.Errorf("tasks not preserved: %v", r2.Tasks)
	}
}
