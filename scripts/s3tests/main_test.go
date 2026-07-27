package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// report writes a JUnit document with the given outcomes and returns its path.
// The shape matches what pytest emits: a dotted module path as the class name.
func report(t *testing.T, cases map[string]string) string {
	t.Helper()

	var b strings.Builder

	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><testsuites><testsuite name="pytest">`)

	for name, outcome := range cases {
		b.WriteString(`<testcase classname="s3tests.functional.test_s3" name="` + name + `">`)

		switch outcome {
		case "fail":
			b.WriteString(`<failure message="boom">boom</failure>`)
		case "error":
			b.WriteString(`<error message="boom">boom</error>`)
		case "skip":
			b.WriteString(`<skipped message="nope"/>`)
		}

		b.WriteString(`</testcase>`)
	}

	b.WriteString(`</testsuite></testsuites>`)

	path := filepath.Join(t.TempDir(), "report.xml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// list writes a known-failures file and returns its path.
func list(t *testing.T, ids ...string) string {
	t.Helper()

	body := "# a header that must survive an update\n\n" + strings.Join(ids, "\n") + "\n"

	path := filepath.Join(t.TempDir(), "known.txt")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

const (
	idA = "s3tests/functional/test_s3.py::test_a"
	idB = "s3tests/functional/test_s3.py::test_b"
	idC = "s3tests/functional/test_s3.py::test_c"
)

// TestCheck_Clean covers the case CI sees on a green run: the failures are
// exactly the ones the list names.
func TestCheck_Clean(t *testing.T) {
	rep := report(t, map[string]string{"test_a": "pass", "test_b": "fail", "test_c": "skip"})

	if err := run([]string{"check", "--ratchet", list(t, idB), "--report", rep}); err != nil {
		t.Fatalf("expected a clean check, got %v", err)
	}
}

// TestCheck_Regression covers a failure the list does not permit.
func TestCheck_Regression(t *testing.T) {
	rep := report(t, map[string]string{"test_a": "fail", "test_b": "fail"})

	err := run([]string{"check", "--ratchet", list(t, idB), "--report", rep})
	if err == nil || !strings.Contains(err.Error(), "regressed") {
		t.Fatalf("expected a regression, got %v", err)
	}
}

// TestCheck_StaleEntry is the half that makes the list shrink: a test named as
// a known failure that now passes fails the build.
func TestCheck_StaleEntry(t *testing.T) {
	rep := report(t, map[string]string{"test_a": "pass", "test_b": "pass"})

	err := run([]string{"check", "--ratchet", list(t, idB), "--report", rep})
	if err == nil || !strings.Contains(err.Error(), "may only shrink") {
		t.Fatalf("expected a stale entry, got %v", err)
	}
}

// TestCheck_ErrorCountsAsFailure covers a test that errored in setup or
// teardown rather than failing an assertion — still not passing.
func TestCheck_ErrorCountsAsFailure(t *testing.T) {
	rep := report(t, map[string]string{"test_a": "error"})

	err := run([]string{"check", "--ratchet", list(t), "--report", rep})
	if err == nil || !strings.Contains(err.Error(), "regressed") {
		t.Fatalf("expected an error to gate like a failure, got %v", err)
	}
}

// TestCheck_SkipIsNeither pins that a skipped test is not a failure to permit
// nor a pass to celebrate: the suite skips ~94 that need a Ceph extension.
func TestCheck_SkipIsNeither(t *testing.T) {
	rep := report(t, map[string]string{"test_a": "skip"})

	if err := run([]string{"check", "--ratchet", list(t), "--report", rep}); err != nil {
		t.Fatalf("a skip should gate as neither, got %v", err)
	}
}

// TestCheck_PermittedButNotOwned covers the cluster job's split: it honors the
// single-node list without owning it, so a test that fails on one node and
// passes on three is not reported as a stale cluster entry.
func TestCheck_PermittedButNotOwned(t *testing.T) {
	rep := report(t, map[string]string{"test_a": "pass", "test_b": "fail"})

	singleNode := list(t, idA, idB)
	cluster := list(t)

	if err := run([]string{
		"check", "--known", singleNode, "--ratchet", cluster, "--report", rep,
	}); err != nil {
		t.Fatalf("expected the borrowed list not to ratchet, got %v", err)
	}

	// The cluster's own list still ratchets.
	err := run([]string{
		"check", "--known", singleNode, "--ratchet", list(t, idA), "--report", rep,
	})
	if err == nil || !strings.Contains(err.Error(), "may only shrink") {
		t.Fatalf("expected the owned list to ratchet, got %v", err)
	}
}

// TestUpdate_RewritesListAndKeepsHeader covers re-baselining after a ref bump:
// the failures are rewritten and the explanation at the top survives.
func TestUpdate_RewritesListAndKeepsHeader(t *testing.T) {
	rep := report(t, map[string]string{"test_a": "pass", "test_b": "fail", "test_c": "fail"})
	path := list(t, idA)

	if err := run([]string{"update", "--ratchet", path, "--report", rep}); err != nil {
		t.Fatalf("update failed: %v", err)
	}

	body, err := os.ReadFile(path) //nolint:gosec // Test-owned temp path.
	if err != nil {
		t.Fatal(err)
	}

	got := string(body)

	if !strings.Contains(got, "# a header that must survive an update") {
		t.Errorf("header was lost:\n%s", got)
	}

	for _, want := range []string{idB, idC} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}

	if strings.Contains(got, idA) {
		t.Errorf("a passing test was written into the list:\n%s", got)
	}
}

// TestCheck_EmptyReportIsAnError pins the failure mode that would otherwise
// look like success: a run that produced nothing must not gate green.
func TestCheck_EmptyReportIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.xml")
	if err := os.WriteFile(path, []byte(`<testsuites></testsuites>`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"check", "--ratchet", list(t), "--report", path}); err == nil {
		t.Fatal("an empty report must not gate green")
	}
}
