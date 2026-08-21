package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecutionHandlers_ListAndGet(t *testing.T) {
	hist, err := OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	t.Cleanup(func() { hist.Close() })

	start := ExecutionStart{ExecutionID: "exec-1", ProcessID: "proc-1", DeviceID: "pine", Mode: "write", Command: "echo hi", StartedAt: time.Now().Truncate(time.Second)}
	if err := hist.RecordStart(start); err != nil {
		t.Fatalf("RecordStart: %v", err)
	}
	exitCode := 0
	if err := hist.RecordEnd("exec-1", start.StartedAt.Add(time.Second), &exitCode); err != nil {
		t.Fatalf("RecordEnd: %v", err)
	}

	h := NewExecutionHandlers(hist)

	listParams, _ := json.Marshal(ExecutionListParams{Limit: 10})
	listResultAny, rpcErr := h.List(context.Background(), listParams)
	if rpcErr != nil {
		t.Fatalf("List: %+v", rpcErr)
	}
	listResult := listResultAny.(ExecutionListResult)
	if len(listResult.Executions) != 1 || listResult.Executions[0].ExecutionID != "exec-1" {
		t.Fatalf("list = %+v", listResult.Executions)
	}

	getParams, _ := json.Marshal(ExecutionGetParams{ExecutionID: "exec-1"})
	getResultAny, rpcErr := h.Get(context.Background(), getParams)
	if rpcErr != nil {
		t.Fatalf("Get: %+v", rpcErr)
	}
	getResult := getResultAny.(ExecutionGetResult)
	if getResult.Execution.ExitCode == nil || *getResult.Execution.ExitCode != 0 {
		t.Fatalf("get = %+v", getResult.Execution)
	}
}

func TestExecutionHandlers_GetUnknownIsInvalidRequest(t *testing.T) {
	hist, err := OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	t.Cleanup(func() { hist.Close() })

	h := NewExecutionHandlers(hist)
	getParams, _ := json.Marshal(ExecutionGetParams{ExecutionID: "does-not-exist"})
	_, rpcErr := h.Get(context.Background(), getParams)
	if rpcErr == nil {
		t.Fatal("expected an error for an unknown execution_id")
	}
}

// TestExecutionHandlers_ListAndGetLogSQLiteFailure covers base spec §24's
// SQLite-failure Agent logging category for the read path: only the
// write path (RecordStart/RecordEnd) was logged before this fix.
func TestExecutionHandlers_ListAndGetLogSQLiteFailure(t *testing.T) {
	hist, err := OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	hist.Close() // force every subsequent query to fail

	h := NewExecutionHandlers(hist)

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	if _, rpcErr := h.List(context.Background(), mustMarshal(t, ExecutionListParams{})); rpcErr == nil {
		t.Fatal("expected List to fail against a closed store")
	}
	if _, rpcErr := h.Get(context.Background(), mustMarshal(t, ExecutionGetParams{ExecutionID: "exec-1"})); rpcErr == nil {
		t.Fatal("expected Get to fail against a closed store")
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "history List failed") || !strings.Contains(logOutput, "history Get failed") {
		t.Fatalf("log output = %q, want it to mention both List and Get failures", logOutput)
	}
}
