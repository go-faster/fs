// Command s3tests gates a ceph/s3-tests run against the known-failures list.
//
// The suite runs in full on every pull request and the gate is a *deny*-list:
// every test not named in it must pass. That direction is the point. An
// allow-list only grows, so a test that starts passing stays invisible until
// someone runs the suite by hand and promotes it, and a test that starts
// failing outside the list is never noticed at all. A deny-list surfaces both
// on the pull request that causes them.
//
// It therefore fails on two things, not one:
//
//   - a test outside the list that failed — a regression;
//   - a test inside the list that passed — the list is stale, and leaving it
//     stale is how a deny-list quietly turns back into an allow-list.
//
// Usage:
//
//	s3tests check  --ratchet <file> [--known <file>] [--report <junit.xml> ...]
//	s3tests update --ratchet <file> [--report <junit.xml> ...]
//
// --ratchet names the list this run is responsible for: a failure in it is
// expected, a *pass* in it is an error, and that is what forces it to shrink.
// --known names lists that merely permit failure — the cluster job holds itself
// to the single-node list without owning it, because a test that fails on one
// node and passes on three is not a cluster bug to fix.
package main

import (
	"bufio"
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "s3tests:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: s3tests <check|update> --known <file> --report <junit.xml>")
	}

	command, args := args[0], args[1:]

	var (
		fs             = flag.NewFlagSet(command, flag.ContinueOnError)
		ratchet        = fs.String("ratchet", ".github/s3tests/known-failures.txt", "list this run owns: failures expected, passes are errors")
		known, reports multiFlag
	)

	fs.Var(&known, "known", "list that merely permits failure (repeatable)")
	fs.Var(&reports, "report", "JUnit XML report (repeatable)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if len(reports) == 0 {
		return fmt.Errorf("at least one --report is required")
	}

	results, err := loadReports(reports)
	if err != nil {
		return err
	}

	owned, err := loadKnown(*ratchet)
	if err != nil {
		return err
	}

	permitted := make(map[string]bool, len(owned))
	for id := range owned {
		permitted[id] = true
	}

	for _, path := range known {
		extra, err := loadKnown(path)
		if err != nil {
			return err
		}

		for id := range extra {
			permitted[id] = true
		}
	}

	switch command {
	case "check":
		return check(results, owned, permitted)
	case "update":
		return update(*ratchet, results)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// outcome is what a run reported for one test.
type outcome int

const (
	passed outcome = iota
	failed
	skipped
)

// junit is the subset of the JUnit schema pytest emits that matters here.
type junit struct {
	Cases []struct {
		Name      string    `xml:"name"`
		File      string    `xml:"file,attr"`
		NameAttr  string    `xml:"name,attr"`
		Failure   *struct{} `xml:"failure"`
		Error     *struct{} `xml:"error"`
		Skipped   *struct{} `xml:"skipped"`
		ClassName string    `xml:"classname,attr"`
	} `xml:"testsuite>testcase"`
}

// loadReports reads every report into one map of node ID to outcome.
func loadReports(paths []string) (map[string]outcome, error) {
	results := make(map[string]outcome)

	for _, path := range paths {
		data, err := os.ReadFile(path) //nolint:gosec // Path comes from the invoking workflow.
		if err != nil {
			return nil, fmt.Errorf("read report %s: %w", path, err)
		}

		var doc junit
		if err := xml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse report %s: %w", path, err)
		}

		for _, c := range doc.Cases {
			id := nodeID(c.ClassName, c.NameAttr)
			if id == "" {
				continue
			}

			switch {
			case c.Skipped != nil:
				results[id] = skipped
			case c.Failure != nil || c.Error != nil:
				results[id] = failed
			default:
				results[id] = passed
			}
		}
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("reports contain no test cases")
	}

	return results, nil
}

// nodeID rebuilds the pytest node ID from a JUnit case, so the list reads the
// way a developer would run one test.
func nodeID(className, name string) string {
	if name == "" {
		return ""
	}

	// pytest writes the module path as a dotted class name.
	file := strings.ReplaceAll(className, ".", "/")
	if file == "" {
		return name
	}

	return file + ".py::" + name
}

// loadKnown reads the known-failures list, ignoring comments and blank lines.
func loadKnown(path string) (map[string]bool, error) {
	f, err := os.Open(path) //nolint:gosec // Path comes from the invoking workflow.
	if err != nil {
		return nil, fmt.Errorf("read known failures: %w", err)
	}
	defer func() { _ = f.Close() }()

	known := make(map[string]bool)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}

		if line != "" {
			known[line] = true
		}
	}

	return known, scanner.Err()
}

// check reports regressions and stale entries, and fails on either.
//
// owned is the list this run is responsible for; permitted is every list that
// allows a failure, owned included.
func check(results map[string]outcome, owned, permitted map[string]bool) error {
	var (
		regressions []string
		stale       []string
		passing     int
	)

	for id, got := range results {
		switch {
		case got == passed && owned[id]:
			stale = append(stale, id)
			passing++
		case got == passed:
			passing++
		case got == failed && !permitted[id]:
			regressions = append(regressions, id)
		}
	}

	sort.Strings(regressions)
	sort.Strings(stale)

	fmt.Printf("%d passing, %d permitted failures (%d owned), %d collected\n",
		passing, len(permitted), len(owned), len(results))

	for _, id := range regressions {
		fmt.Printf("::error::%s failed and is not a known failure\n", id)
	}

	for _, id := range stale {
		fmt.Printf("::error::%s passes now — remove it from the known-failures list\n", id)
	}

	switch {
	case len(regressions) > 0 && len(stale) > 0:
		return fmt.Errorf("%d regressions and %d stale entries", len(regressions), len(stale))
	case len(regressions) > 0:
		return fmt.Errorf("%d tests regressed", len(regressions))
	case len(stale) > 0:
		return fmt.Errorf("%d known failures pass now; the list may only shrink", len(stale))
	}

	return nil
}

// update rewrites the known-failures list from a run, preserving the file's
// header comment. It is for re-baselining after a deliberate change (an
// S3TESTS_REF bump); routine work removes lines by hand.
func update(path string, results map[string]outcome) error {
	header, err := headerOf(path)
	if err != nil {
		return err
	}

	failures := make([]string, 0, len(results))

	for id, got := range results {
		if got == failed {
			failures = append(failures, id)
		}
	}

	sort.Strings(failures)

	body := header + strings.Join(failures, "\n") + "\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write known failures: %w", err)
	}

	fmt.Printf("wrote %d known failures to %s\n", len(failures), path)

	return nil
}

// headerOf returns the leading comment block of the list, which explains what
// the file is and must survive a regeneration.
func headerOf(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // Path comes from the invoking workflow.
	if err != nil {
		return "", fmt.Errorf("read known failures: %w", err)
	}
	defer func() { _ = f.Close() }()

	var header strings.Builder

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			break
		}

		header.WriteString(line + "\n")
	}

	return header.String(), scanner.Err()
}
