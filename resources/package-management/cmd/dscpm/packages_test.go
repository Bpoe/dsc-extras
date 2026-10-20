package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/apt"
)

type failedPackageExecutor struct{ err error }

func (e failedPackageExecutor) Execute(context.Context, string, []string, []string) ([]byte, []byte, error) {
	return nil, []byte("package-manager lock unavailable"), e.err
}

type packageExitError int

func (e packageExitError) Error() string { return "package command failed" }
func (e packageExitError) ExitCode() int { return int(e) }

func TestPackagesPreservesExitCodeAndDiagnostics(t *testing.T) {
	manager := apt.NewPackageManager(failedPackageExecutor{packageExitError(100)})
	var stdout, stderr strings.Builder
	code := runPackages(t.Context(), []string{"set"}, strings.NewReader(`{"name":"git"}`), &stdout, &stderr, manager)
	if code != 100 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "lock unavailable") {
		t.Fatalf("command failure: (%d, %q, %q)", code, stdout.String(), stderr.String())
	}
}

func TestPackagesCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr strings.Builder
	code := runPackages(ctx, []string{"set"}, strings.NewReader(`{"name":"git"}`), &stdout, &stderr, apt.NewPackageManager(nil))
	if code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "context canceled") {
		t.Fatalf("canceled command: (%d, %q, %q)", code, stdout.String(), stderr.String())
	}
}

func TestPackagesDispatchAndStateAndDiff(t *testing.T) {
	manager := newDSCPackageManager(t.TempDir())
	for _, operation := range []string{"get", "test", "set", "test"} {
		var stdout, stderr strings.Builder
		code := runWithPackages(t.Context(), []string{"packages", operation},
			strings.NewReader(`{"name":["vim","git"]}`), &stdout, &stderr, makeStore(t), manager)
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("%s: (%d, %q, %q)", operation, code, stdout.String(), stderr.String())
		}
		lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
		var actual struct {
			apt.Packages
			InDesiredState bool `json:"_inDesiredState"`
		}
		if err := json.Unmarshal([]byte(lines[0]), &actual); err != nil {
			t.Fatal(err)
		}
		if strings.Join(actual.Name.Values, ",") != "git,vim" || len(actual.Packages.Packages) != 2 {
			t.Fatalf("unexpected observed state: %s", stdout.String())
		}
		if operation == "test" {
			if len(lines) != 2 || !json.Valid([]byte(lines[1])) {
				t.Fatalf("invalid stateAndDiff: %s", stdout.String())
			}
			if actual.InDesiredState != (lines[1] == "[]") {
				t.Fatalf("inconsistent compliance: %s", stdout.String())
			}
		} else if len(lines) != 1 {
			t.Fatalf("unexpected stdout: %s", stdout.String())
		}
	}
}

func TestPackagesInputErrors(t *testing.T) {
	for _, input := range []string{
		`{`, `{"name":false}`, `{"name":"git","unknown":true}`,
		`null`, `{"name":"git","version":""}`, `{"name":"git","version":null}`,
		`{"name":"git","ensure":""}`, `{"name":"git","ensure":null}`,
		`{"name":"git","allowDowngrade":null}`, `{"name":"git","updateCache":null}`,
		`{"name":"git"} {}`, strings.Repeat(" ", maxInputBytes+1),
	} {
		t.Run(input[:min(len(input), 32)], func(t *testing.T) {
			var stdout, stderr strings.Builder
			code := runPackages(context.Background(), []string{"get"}, strings.NewReader(input), &stdout, &stderr, nil)
			if code == 0 || stdout.Len() != 0 || !json.Valid([]byte(stderr.String())) {
				t.Fatalf("invalid input returned (%d, %q, %q)", code, stdout.String(), stderr.String())
			}
		})
	}
	for _, args := range [][]string{nil, {"remove"}, {"get", "extra"}} {
		var stdout, stderr strings.Builder
		if code := runPackages(context.Background(), args, strings.NewReader("{}"), &stdout, &stderr, nil); code == 0 || stdout.Len() != 0 {
			t.Fatalf("invalid args %v returned %d, %q", args, code, stdout.String())
		}
	}
}
