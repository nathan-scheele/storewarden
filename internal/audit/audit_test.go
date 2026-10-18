package audit_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nathan-scheele/storewarden/internal/audit"
)

func TestFileLogAppendsAndCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path, []byte("{\"earlier\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	log, err := audit.Open(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Write(audit.Record{RequestID: "REQ1", Phase: audit.Denied}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"request_id":"REQ1"`) {
		t.Errorf("audit log = %q, want the earlier line then REQ1's record", got)
	}

	// Nothing more can be recorded once the log is closed.
	if err := log.Write(audit.Record{RequestID: "REQ2"}); err == nil {
		t.Error("Write after Close succeeded")
	}
	if log.Healthy() {
		t.Error("Log is healthy after a failed write")
	}
}
