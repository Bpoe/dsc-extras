package dnf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/packages"
)

type recordingExecutor struct {
	packages.OSExecutor
	mutations int
	targets   []string
}

func (e *recordingExecutor) Execute(ctx context.Context, program string, args, env []string) ([]byte, []byte, error) {
	if program == "dnf" && slices.Contains(args, "--assumeyes") {
		e.mutations++
		e.targets = slices.Clone(args[slices.Index(args, "--")+1:])
	}
	return e.OSExecutor.Execute(ctx, program, args, env)
}

func TestDNFNativeVersions(t *testing.T) {
	requireNative(t)
	m := NewPackageManager(nil)
	for _, test := range []struct {
		left, right string
		want        int
	}{
		{"1-1", "0:1-1", 0}, {"0001:1-1", "1:1-1", 0},
		{"1:1-1", "0:99-99", 1}, {"1.10-1", "1.9-99", 1},
		{"1.0~rc1-1", "1.0-1", -1}, {"1-10", "1-2", 1},
		{"1.0-1", "1.0.1-1", -1}, {"1.0-99", "1.0.0-1", -1},
		{"1.0+git-1", "1.0.git-1", 0},
	} {
		left, right := fixture("pkg", test.left, "noarch"), fixture("pkg", test.right, "noarch")
		got, err := m.compare(t.Context(), left, right)
		if err != nil || got != test.want {
			t.Fatalf("rpm compare %s / %s = %d, %v; want %d", test.left, test.right, got, err, test.want)
		}
	}
}

func TestDNFNativeInstallation(t *testing.T) {
	requireNative(t)
	executor := &recordingExecutor{}
	m := NewPackageManager(executor)
	set := func(t *testing.T, want packages.Packages) packages.Packages {
		t.Helper()
		actual, err := m.Set(t.Context(), want)
		if err != nil || actual.State != "Present" {
			t.Fatalf("Set %+v: %+v, %v", want, actual, err)
		}
		got, diff, err := m.Test(t.Context(), want)
		if err != nil || len(diff) != 0 || got.State != "Present" {
			t.Fatalf("Test after Set: %+v, %v, %v", got, diff, err)
		}
		return actual
	}
	t.Run("repository-and-noop", func(t *testing.T) {
		want := desired("jq")
		want.UpdateCache = true
		set(t, want)
		before, calls := history(t), executor.mutations
		set(t, want)
		if history(t) != before || executor.mutations != calls {
			t.Fatal("compliant repository package created a new transaction")
		}
		got, err := m.Get(t.Context(), want)
		if err != nil || !got.Packages[0].Installed || got.Packages[0].Status != "installed" {
			t.Fatalf("Get: %+v, %v", got, err)
		}
	})
	t.Run("batch", func(t *testing.T) {
		before := executor.mutations
		set(t, desired("tree", "lsof"))
		if executor.mutations != before+1 || len(executor.targets) != 2 {
			t.Fatal("batch was not one native installation transaction")
		}
	})
	t.Run("exact-repository-EVR", func(t *testing.T) {
		output := nativeRun(t, "dnf", "--quiet", "repoquery", "--available", "--latest-limit=1",
			"--queryformat", strings.TrimSuffix(queryFormat, "\n"), "which")
		candidates, err := parseMetadata(output, true)
		if err != nil || len(candidates) == 0 {
			t.Fatalf("exact-version fixture: %s, %v", output, err)
		}
		p := candidates[0]
		want := desired(p.name + ":" + p.arch)
		want.Version = p.evr()
		actual := set(t, want)
		if actual.Packages[0].Version != p.evr() {
			t.Fatalf("exact repository EVR: %+v", actual)
		}
		want.Version = "999999:1-1"
		before, calls := history(t), executor.mutations
		if _, err := m.Set(t.Context(), want); err == nil || history(t) != before || executor.mutations != calls {
			t.Fatalf("unavailable exact version was not rejected without a transaction: %v", err)
		}
	})
	dependency := buildRPM(t, "Dscpm.Dep", "1:1.0-1", "")
	app := buildRPM(t, "Dscpm.Test_pkg-x", "1:1.0-1", "Requires: Dscpm.Dep = 1:1.0-1")
	t.Run("mixed-local-batch-and-noop", func(t *testing.T) {
		want := desired(dependency, app, "diffstat")
		before := executor.mutations
		actual := set(t, want)
		if executor.mutations != before+1 || len(executor.targets) != 3 {
			t.Fatal("mixed batch did not use one transaction")
		}
		found := false
		for _, p := range actual.Packages {
			if p.Name == "Dscpm.Test_pkg-x" {
				found = p.Version == "1:1.0-1" && p.Architecture == "noarch"
			}
		}
		if !found {
			t.Fatalf("incorrect local metadata: %+v", actual.Packages)
		}
		original, err := os.ReadFile(app)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(app, original, 0600); err != nil {
				t.Error(err)
			}
		})
		file, err := os.OpenFile(app, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.WriteString("different bytes, identical RPM metadata")
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("modify fixture: %v, %v", writeErr, closeErr)
		}
		hist, calls := history(t), executor.mutations
		set(t, want)
		if history(t) != hist || executor.mutations != calls {
			t.Fatal("compliant local RPM was reinstalled")
		}
	})
	t.Run("local-upgrade-and-downgrade", func(t *testing.T) {
		newer := buildRPM(t, "Dscpm.Test_pkg-x", "1:2.0-1", "Requires: Dscpm.Dep = 1:1.0-1")
		set(t, desired(newer))
		want := desired(app)
		before, calls := history(t), executor.mutations
		if _, err := m.Set(t.Context(), want); err == nil || !strings.Contains(err.Error(), "allowDowngrade") ||
			history(t) != before || executor.mutations != calls {
			t.Fatalf("unapproved downgrade changed the host: %v", err)
		}
		want.AllowDowngrade = true
		set(t, want)
	})
	t.Run("obsoletes-cannot-erase-unrelated-package", func(t *testing.T) {
		victim := buildRPM(t, "dscpm-victim", "1-1", "")
		set(t, desired(victim))
		replacement := buildRPM(t, "dscpm-replacement", "2-1", "Obsoletes: dscpm-victim\nProvides: dscpm-victim")
		before := string(nativeRun(t, "rpm", "-qa", "--queryformat", queryFormat))
		calls, hist := executor.mutations, history(t)
		_, err := m.Set(t.Context(), desired(replacement))
		var commandError *packages.CommandError
		nativeProtection := errors.As(err, &commandError) && strings.Contains(commandError.Stderr, "protected") &&
			strings.Contains(commandError.Stderr, "dscpm-victim")
		preflightProtection := err != nil && strings.Contains(err.Error(), `unsafe or unknown transaction row "replacing  dscpm-victim.noarch`)
		if !nativeProtection && !preflightProtection {
			t.Fatalf("expected native protection or explicit preflight replacement rejection: %v", err)
		}
		if executor.mutations != calls || history(t) != hist ||
			string(nativeRun(t, "rpm", "-qa", "--queryformat", queryFormat)) != before {
			t.Fatal("rejected replacement modified the RPM inventory")
		}
	})
	t.Run("unsupported-removal", func(t *testing.T) {
		want := desired("jq")
		want.Ensure = "Absent"
		hist, calls := history(t), executor.mutations
		if _, err := m.Set(t.Context(), want); err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("removal: %v", err)
		}
		if history(t) != hist || executor.mutations != calls {
			t.Fatal("unsupported removal modified the host")
		}
	})
	t.Run("real-manifest-and-pinned-DSC", func(t *testing.T) {
		if runtime.GOARCH != "amd64" {
			t.Fatal("the pinned DSC container fixture currently requires linux/amd64")
		}
		dir := t.TempDir()
		archive := filepath.Join(dir, "dsc.tar.gz")
		nativeRun(t, "curl", "--fail", "--silent", "--show-error", "--location",
			"https://github.com/PowerShell/DSC/releases/download/v3.3.0/DSC-3.3.0-x86_64-linux.tar.gz", "--output", archive)
		data, err := os.ReadFile(archive)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != "cf17d1a130b65332ec65380ee9368133101d13d8f40c1a501038920b1e9cd748" {
			t.Fatal("pinned DSC archive checksum mismatch")
		}
		nativeRun(t, "tar", "-xzf", archive, "-C", dir)
		dsc := filepath.Join(dir, "dsc")
		if got := strings.TrimSpace(string(nativeRun(t, dsc, "--version"))); got != "dsc 3.3.0" {
			t.Fatalf("unexpected DSC version %q", got)
		}
		t.Setenv("DSC_RESOURCE_PATH", "/usr/local/bin")
		historyBeforeExport := history(t)
		for _, args := range [][]string{
			{"resource", "export", "--resource", "DscExtras.PackageManagement/packages", "--output-format", "json"},
			{"resource", "get", "--all", "--resource", "DscExtras.PackageManagement/packages", "--output-format", "json"},
		} {
			output := nativeRun(t, dsc, args...)
			if !json.Valid(output) || !strings.Contains(string(output), "Dscpm.Test_pkg-x:noarch") {
				t.Fatalf("DSC %s failed to consume RPM Export:\n%s", strings.Join(args, " "), output)
			}
		}
		if history(t) != historyBeforeExport {
			t.Fatal("DSC Export modified the RPM inventory")
		}
		input := filepath.Join(dir, "input.json")
		// Uppercase, punctuation and a version exercise the real schema too.
		if err := os.WriteFile(input, []byte(`{"name":"Dscpm.Test_pkg-x:noarch","version":"1:1.0-1"}`), 0600); err != nil {
			t.Fatal(err)
		}
		hist := history(t)
		for _, op := range []string{"get", "test", "set"} {
			output := nativeRun(t, dsc, "resource", op, "--resource", "DscExtras.PackageManagement/packages",
				"--file", input, "--output-format", "json")
			var result struct {
				ActualState         packages.Packages `json:"actualState"`
				AfterState          packages.Packages `json:"afterState"`
				InDesiredState      bool              `json:"inDesiredState"`
				DifferingProperties []string          `json:"differingProperties"`
			}
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatalf("DSC %s JSON: %v\n%s", op, err, output)
			}
			state := result.ActualState
			if op == "set" {
				state = result.AfterState
			}
			if state.State != "Present" || len(state.Packages) != 1 || !state.Packages[0].Installed ||
				state.Packages[0].Version != "1:1.0-1" || state.Packages[0].Architecture != "noarch" ||
				(op == "test" && (!result.InDesiredState || len(result.DifferingProperties) != 0)) {
				t.Fatalf("DSC %s: %s", op, output)
			}
		}
		if history(t) != hist {
			t.Fatal("DSC reconciliation of a compliant package created a transaction")
		}
		// Also exercise mutation through the shipped manifest (not a rewritten
		// test manifest or an injected executor).
		if err := os.WriteFile(input, []byte(`{"name":"ed"}`), 0600); err != nil {
			t.Fatal(err)
		}
		nativeRun(t, dsc, "resource", "set", "--resource", "DscExtras.PackageManagement/packages", "--file", input)
		got, diff, err := m.Test(t.Context(), desired("ed"))
		if err != nil || len(diff) != 0 || got.State != "Present" {
			t.Fatalf("DSC native installation: %+v, %v, %v", got, diff, err)
		}
	})
}
