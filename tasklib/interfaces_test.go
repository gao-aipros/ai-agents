package tasklib

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeTaskStore is an in-memory implementation of TaskStore for testing.
type fakeTaskStore struct {
	mu    sync.Mutex
	tasks map[string]*Task
}

func newFakeTaskStore() *fakeTaskStore {
	return &fakeTaskStore{tasks: make(map[string]*Task)}
}

func (f *fakeTaskStore) Enqueue(ctx context.Context, worker, threadID, instruction string) (*Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	taskID := fmt.Sprintf("task-%d", len(f.tasks)+1)
	t := &Task{
		TaskID:      taskID,
		ThreadID:    threadID,
		Instruction: instruction,
		Worker:      worker,
		Status:      "pending",
		EnqueuedAt:  time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	f.tasks[taskID] = t
	return t, nil
}

func (f *fakeTaskStore) EnqueueGroup(ctx context.Context, worker, threadID, groupLabel, instruction string) (*Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	taskID := fmt.Sprintf("task-%d", len(f.tasks)+1)
	t := &Task{
		TaskID:      taskID,
		ThreadID:    threadID,
		Instruction: instruction,
		Worker:      worker,
		Status:      "pending",
		EnqueuedAt:  time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	f.tasks[taskID] = t
	return t, nil
}

func (f *fakeTaskStore) GetTask(ctx context.Context, taskID string) (*Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[taskID]
	if !ok {
		return &Task{TaskID: taskID}, nil
	}
	return t, nil
}

func (f *fakeTaskStore) GetTaskResult(ctx context.Context, taskID string, tail int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[taskID]
	if !ok {
		return "", nil
	}
	return t.Result, nil
}

func (f *fakeTaskStore) ListTasks(ctx context.Context, worker, status, threadID string, limit, offset int, sortBy, sortDir string) ([]*Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*Task
	for _, t := range f.tasks {
		if worker != "" && t.Worker != worker {
			continue
		}
		if status != "" && t.Status != status {
			continue
		}
		if threadID != "" && t.ThreadID != threadID {
			continue
		}
		result = append(result, t)
	}
	if offset > 0 && offset < len(result) {
		result = result[offset:]
	}
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (f *fakeTaskStore) WaitTask(ctx context.Context, taskID, threadID string, timeout time.Duration) (*Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("task %s not found", taskID)
	}
	return t, nil
}

func (f *fakeTaskStore) GroupWait(ctx context.Context, threadID, groupLabel string, timeout time.Duration) (*GroupResult, error) {
	return &GroupResult{
		ThreadID: threadID,
		Label:    groupLabel,
		Status:   "complete",
	}, nil
}

func (f *fakeTaskStore) CancelTask(ctx context.Context, taskID, cancelledBy string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	t.Status = "cancelled"
	t.CancelledBy = cancelledBy
	return nil
}

func (f *fakeTaskStore) RequeueStale(ctx context.Context, worker string, olderThan time.Duration) ([]string, error) {
	return nil, nil
}

// Compile-time assertion: fakeTaskStore satisfies TaskStore.
var _ TaskStore = (*fakeTaskStore)(nil)

func TestFakeTaskStore(t *testing.T) {
	store := newFakeTaskStore()
	ctx := context.Background()

	// Enqueue a task
	task, err := store.Enqueue(ctx, "claude", "thread-1", "write tests")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if task.Status != "pending" {
		t.Errorf("expected status pending, got %s", task.Status)
	}
	if task.Worker != "claude" {
		t.Errorf("expected worker claude, got %s", task.Worker)
	}

	// Get the task
	got, err := store.GetTask(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.TaskID != task.TaskID {
		t.Errorf("expected task ID %s, got %s", task.TaskID, got.TaskID)
	}

	// List tasks
	tasks, err := store.ListTasks(ctx, "claude", "", "", 10, 0, "", "")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(tasks))
	}

	// Cancel task
	if err := store.CancelTask(ctx, task.TaskID, "user"); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	cancelled, _ := store.GetTask(ctx, task.TaskID)
	if cancelled.Status != "cancelled" {
		t.Errorf("expected cancelled status, got %s", cancelled.Status)
	}

	// Verify the fake satisfies the interface at compile time
	var _ TaskStore = store
	_ = store // use store to suppress unused variable warning
}
