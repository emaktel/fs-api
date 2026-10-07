package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Golden fixtures for the cross-repo worker policy harness (Loi 5 U3).
//
// Tests record the exact broadcast bodies they capture on the wire with
// recordFixture. When WS_FIXTURE_DIR is set and every test passed, TestMain
// writes one JSON object per file ({"<case name>": <body>}) into that
// directory. Nothing is written in a normal `go test` run.

var (
	fixtureMu sync.Mutex
	fixtures  = map[string]map[string]json.RawMessage{}
)

// recordFixture stores a captured wire body under file/name. A rerun
// (-count > 1) overwrites the same case with an identical shape.
func recordFixture(t *testing.T, file, name string, body []byte) {
	t.Helper()
	if !json.Valid(body) {
		t.Fatalf("fixture %s/%s is not valid JSON: %s", file, name, body)
	}
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	if fixtures[file] == nil {
		fixtures[file] = map[string]json.RawMessage{}
	}
	fixtures[file][name] = append(json.RawMessage(nil), body...)
}

func writeFixtures(dir string) error {
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	for file, cases := range fixtures {
		out, err := json.MarshalIndent(cases, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal %s: %w", file, err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), append(out, '\n'), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", file, err)
		}
	}
	return nil
}

func TestMain(m *testing.M) {
	code := m.Run()
	if dir := os.Getenv("WS_FIXTURE_DIR"); dir != "" && code == 0 {
		if err := writeFixtures(dir); err != nil {
			fmt.Fprintf(os.Stderr, "WS_FIXTURE_DIR: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}
