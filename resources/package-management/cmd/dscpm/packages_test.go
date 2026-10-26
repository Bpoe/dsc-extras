package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/apt"
	"github.com/Bpoe/dsc-extras/resources/package-management/internal/dnf"
)

func TestPackageBackendSelection(t *testing.T) {
	for _, family := range []string{"apt", "dnf", "unsupported"} {
		manager, err := packageBackend(func() (string, error) { return family, nil })
		switch family {
		case "apt":
			if _, ok := manager.(*apt.PackageManager); !ok || err != nil {
				t.Fatalf("APT dispatch: %T, %v", manager, err)
			}
		case "dnf":
			if _, ok := manager.(*dnf.PackageManager); !ok || err != nil {
				t.Fatalf("DNF dispatch: %T, %v", manager, err)
			}
		default:
			if err == nil {
				t.Fatal("unsupported backend accepted")
			}
		}
	}
}

func TestDNFPackagesPreservesExitCode(t *testing.T) {
	var stdout, stderr strings.Builder
	code := runPackages(t.Context(), []string{"set"}, strings.NewReader(`{"name":"git"}`), &stdout, &stderr,
		dnf.NewPackageManager(failedPackageExecutor{packageExitError(200)}))
	if code != 200 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "lock unavailable") {
		t.Fatalf("DNF error: %d, %s, %s", code, stdout.String(), stderr.String())
	}
}

func TestDNFPackagesExportNamesIncludeVersion(t *testing.T) {
	var stdout, stderr strings.Builder
	code := runPackages(t.Context(), []string{"export"}, strings.NewReader(`{"name":"kernel:x86_64"}`),
		&stdout, &stderr, dnf.NewPackageManager(integrationPackageExecutor{installOnly: true}))
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if code != 0 || stderr.Len() != 0 || len(lines) != 2 {
		t.Fatalf("install-only RPM Export = %d %q %q", code, stdout.String(), stderr.String())
	}
	for i, test := range []struct {
		version string
		name    string
	}{
		{"1:1.0-1", "kernel%3Ax86_64%2C1%3A1.0-1"},
		{"1:2.0-1", "kernel%3Ax86_64%2C1%3A2.0-1"},
	} {
		var actual struct {
			Name       string `json:"name"`
			Version    string `json:"version"`
			ExportName string `json:"_name"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &actual); err != nil ||
			actual.Name != "kernel:x86_64" || actual.Version != test.version || actual.ExportName != test.name {
			t.Fatalf("incorrect RPM Export name: %s, %v", lines[i], err)
		}
	}
}

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

func TestPackagesTestMissingLocalDeb(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "downloaded-later.deb")
	input, err := json.Marshal(map[string]any{"name": []string{"git", missing}})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	code := runPackages(t.Context(), []string{"test"}, strings.NewReader(string(input)), &stdout, &stderr,
		newDSCPackageManager(t.TempDir()))
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("test: (%d, %q, %q)", code, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("invalid stateAndDiff: %s", stdout.String())
	}
	var actual struct {
		apt.Packages
		InDesiredState *bool `json:"_inDesiredState"`
	}
	var differences []string
	if err := json.Unmarshal([]byte(lines[0]), &actual); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &differences); err != nil {
		t.Fatal(err)
	}
	if actual.InDesiredState == nil || *actual.InDesiredState || len(differences) == 0 || len(actual.Packages.Packages) != 2 {
		t.Fatalf("missing archive must be noncompliant: %s", stdout.String())
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test must not create the archive: %v", err)
	}
	for _, operation := range []string{"get", "set"} {
		stdout.Reset()
		stderr.Reset()
		code := runPackages(t.Context(), []string{operation}, strings.NewReader(string(input)), &stdout, &stderr,
			newDSCPackageManager(t.TempDir()))
		if code == 0 || stdout.Len() != 0 || !json.Valid([]byte(stderr.String())) {
			t.Fatalf("%s must fail for a missing archive: (%d, %q, %q)", operation, code, stdout.String(), stderr.String())
		}
	}
}
