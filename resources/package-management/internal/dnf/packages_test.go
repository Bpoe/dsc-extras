package dnf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/packages"
)

type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e) }
func (e exitError) ExitCode() int { return int(e) }

type call struct {
	program string
	args    []string
}

type fakeExecutor struct {
	t              *testing.T
	installed      []identity
	local          identity
	plan           []action
	calls          []call
	comparison     int
	mutations      int
	preflights     int
	failProgram    string
	failArgument   string
	failCode       int
	dnfVersion     string
	planOutput     string
	afterPreflight func(*fakeExecutor)
	noEffect       bool
}

func metadata(items ...identity) []byte {
	var output strings.Builder
	for _, p := range items {
		fmt.Fprintf(&output, "%s\t%s\t%s\t%s\t%s\n", p.name, p.epoch, p.version, p.release, p.arch)
	}
	return []byte(output.String())
}

func formatPlan(plan []action) string {
	var output strings.Builder
	output.WriteString("Dependencies resolved.\n====\n Package Architecture Version Repository Size\n====\n")
	for _, kind := range []string{"Install", "Upgrade", "Downgrade"} {
		header := map[string]string{"Install": "Installing", "Upgrade": "Upgrading", "Downgrade": "Downgrading"}[kind]
		started := false
		for _, change := range plan {
			if change.kind != kind {
				continue
			}
			if !started {
				fmt.Fprintf(&output, "%s:\n", header)
				started = true
			}
			fmt.Fprintf(&output, " %s %s %s repo 10 k\n", change.name, change.arch, change.evr())
		}
	}
	output.WriteString("\nTransaction Summary\n====\n")
	for _, kind := range []string{"Install", "Upgrade", "Downgrade"} {
		count := 0
		for _, change := range plan {
			if change.kind == kind {
				count++
			}
		}
		if count > 0 {
			fmt.Fprintf(&output, "%s %d Packages\n", kind, count)
		}
	}
	output.WriteString("\nTotal download size: 10 k\nInstalled size: 20 k\n")
	return output.String()
}

func (e *fakeExecutor) Execute(ctx context.Context, program string, args, env []string) ([]byte, []byte, error) {
	e.t.Helper()
	e.calls = append(e.calls, call{program, slices.Clone(args)})
	deadline, ok := ctx.Deadline()
	limit := 30 * time.Second
	if program == "dnf" {
		limit = 15 * time.Minute
	}
	if !ok || time.Until(deadline) > limit || !slices.Contains(env, "LC_ALL=C") {
		e.t.Fatal("missing command timeout or stable locale")
	}
	if program == e.failProgram && (e.failArgument == "" || slices.Contains(args, e.failArgument)) {
		return []byte("solver details"), []byte("repository or lock unavailable"), exitError(e.failCode)
	}
	switch program {
	case "rpm":
		switch args[0] {
		case "--eval":
			if args[1] == "%{_arch}" {
				return []byte("x86_64\n"), nil, nil
			}
			return fmt.Appendf(nil, "%d\n", e.comparison), nil, nil
		case "--showrc":
			return []byte("compatible archs : x86_64 noarch i686\n"), nil, nil
		case "-qa":
			return metadata(e.installed...), nil, nil
		case "-qp":
			return metadata(e.local), nil, nil
		}
	case "dnf":
		if slices.Contains(args, "--version") {
			if e.dnfVersion != "" {
				return []byte(e.dnfVersion), nil, nil
			}
			return []byte("4.14.0\n"), nil, nil
		}
		for _, option := range []string{"--setopt=obsoletes=False", "--setopt=installonly_limit=0", "--setopt=strict=True",
			"--setopt=*.skip_if_unavailable=False", "--setopt=ignorearch=False"} {
			if !slices.Contains(args, option) {
				e.t.Fatalf("missing safety option %s: %q", option, args)
			}
		}
		for _, arg := range args {
			if strings.HasPrefix(arg, "--setopt=protected_packages=glob:") {
				path := strings.TrimPrefix(arg, "--setopt=protected_packages=glob:")
				data, err := os.ReadFile(path)
				if err != nil || !strings.Contains(string(data), "dnf\n") {
					e.t.Fatalf("missing native protected-package list: %s, %v", data, err)
				}
				for _, p := range e.installed {
					if !slices.Contains(strings.Fields(string(data)), p.name) {
						e.t.Fatalf("installed package not protected: %s", p.name)
					}
				}
			}
		}
		if slices.Contains(args, "--assumeno") {
			e.preflights++
			output := e.planOutput
			if output == "" {
				output = formatPlan(e.plan)
			}
			if e.afterPreflight != nil {
				e.afterPreflight(e)
			}
			return []byte(output), []byte("Operation aborted.\n"), exitError(1)
		}
		if !slices.Contains(args, "--assumeyes") || !slices.Contains(args, "--setopt=assumeno=False") {
			e.t.Fatal("installation must be noninteractive")
		}
		e.mutations++
		if !e.noEffect {
			for _, change := range e.plan {
				e.installed = slices.DeleteFunc(e.installed, func(p identity) bool { return p.key() == change.key() })
				e.installed = append(e.installed, change.identity)
			}
		}
		return nil, nil, nil
	}
	return nil, nil, fmt.Errorf("unexpected command %s %q", program, args)
}

func fixture(name, version, arch string) identity {
	p, err := parseEVR(version)
	if err != nil {
		panic(err)
	}
	p.name, p.arch = name, arch
	return p
}

func TestExportEnumeratesInstalledRPMIdentities(t *testing.T) {
	executor := &fakeExecutor{t: t, installed: []identity{
		fixture("zlib", "2:1.2-3", "x86_64"),
		fixture("zlib", "2:1.2-3", "i686"),
		fixture("noarch-pkg", "1-1", "noarch"),
	}}
	manager := NewPackageManager(executor)
	items, err := manager.Export(context.Background(), packages.Packages{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].Name.Values[0] != "noarch-pkg:noarch" ||
		items[0].Version != "0:1-1" || items[1].Name.Values[0] != "zlib:i686" ||
		items[2].Name.Values[0] != "zlib:x86_64" {
		t.Fatalf("unexpected RPM export: %+v", items)
	}
	filtered, err := manager.Export(context.Background(), packages.Packages{
		Name: packages.PackageNames{Values: []string{"zlib:i686"}, Scalar: true},
	})
	if err != nil || len(filtered) != 1 || filtered[0].Name.Values[0] != "zlib:i686" {
		t.Fatalf("filtered RPM export = %+v, %v", filtered, err)
	}
	if len(executor.calls) != 2 || executor.calls[0].program != "rpm" || executor.calls[0].args[0] != "-qa" {
		t.Fatalf("Export did not use read-only local RPM inventory: %+v", executor.calls)
	}
	for _, item := range items {
		if item.Ensure != "Present" || item.State != "Present" || len(item.Packages) != 1 {
			t.Fatalf("missing observed package properties: %+v", item)
		}
		state := item.Packages[0]
		if !state.Installed || state.Status != "installed" || state.RequestedName != item.Name.Values[0] ||
			state.Version != item.Version || state.Architecture == "" {
			t.Fatalf("incorrect RPM observation: %+v", item)
		}
		observed, err := manager.Get(context.Background(), item)
		if err != nil || !reflect.DeepEqual(item, observed) {
			t.Fatalf("Export differs from Get: %+v; Get = %+v, %v", item, observed, err)
		}
	}
	if executor.mutations != 0 || executor.preflights != 0 {
		t.Fatalf("Export/Get mutated state: %+v", executor.calls)
	}
}

func TestExportPreservesInstallOnlyRPMVersions(t *testing.T) {
	executor := &fakeExecutor{t: t, comparison: -1, installed: []identity{
		fixture("kernel", "1:1.0-1", "x86_64"),
		fixture("kernel", "1:2.0-1", "x86_64"),
	}}
	manager := NewPackageManager(executor)
	items, err := manager.Export(context.Background(), packages.Packages{
		Name:   packages.PackageNames{Values: []string{"kernel:x86_64"}, Scalar: true},
		Ensure: "Absent", Version: "not-observed", State: "Absent",
	})
	if err != nil || len(items) != 2 || items[0].Version != "1:1.0-1" || items[1].Version != "1:2.0-1" {
		t.Fatalf("install-only Export = %+v, %v", items, err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("Export made per-package invocations: %+v", executor.calls)
	}
	for _, item := range items {
		observed, err := manager.Get(context.Background(), item)
		if err != nil || !reflect.DeepEqual(item, observed) {
			t.Fatalf("Export differs from Get for exact RPM version: %+v; Get = %+v, %v", item, observed, err)
		}
	}
}

func TestExportReturnsNoRowsForEmptyRPMInventory(t *testing.T) {
	manager := NewPackageManager(&fakeExecutor{t: t})
	items, err := manager.Export(context.Background(), packages.Packages{})
	if err != nil || len(items) != 0 {
		t.Fatalf("empty RPM inventory = %+v, %v", items, err)
	}
}

func desired(names ...string) packages.Packages {
	return packages.Packages{Name: packages.PackageNames{Values: names, Scalar: len(names) == 1}}
}

func TestScalarArrayAndRPMNames(t *testing.T) {
	for _, input := range []string{`{"name":"Test.pkg_1-x"}`, `{"name":["Test.pkg_1-x"]}`} {
		var want packages.Packages
		if err := json.Unmarshal([]byte(input), &want); err != nil {
			t.Fatal(err)
		}
		e := &fakeExecutor{t: t, installed: []identity{fixture("Test.pkg_1-x", "1-1", "x86_64")}}
		got, diff, err := NewPackageManager(e).Test(t.Context(), want)
		if err != nil || len(diff) != 0 || got.State != "Present" || got.Packages[0].Version != "0:1-1" {
			t.Fatalf("got %+v, %v, %v", got, diff, err)
		}
		data, err := json.Marshal(got.Name)
		if err != nil || strings.HasPrefix(string(data), "[") != !want.Name.Scalar {
			t.Fatalf("scalar/array changed: %s, %v", data, err)
		}
	}
}

func TestInstalledIdentityAndArchitecture(t *testing.T) {
	for _, test := range []struct {
		request, name, arch string
		present             bool
	}{
		{"Test.pkg_1-x", "Test.pkg_1-x", "x86_64", true},
		{"test.pkg_1-x", "Test.pkg_1-x", "x86_64", false},
		{"test", "test-extra", "x86_64", false},
		{"test", "test", "i686", false},
		{"test:i686", "test", "x86_64", false},
		{"test:i686", "test", "i686", true},
		{"test", "test", "noarch", true},
		{"test:x86_64", "test", "noarch", false},
		{"test:noarch", "test", "noarch", true},
	} {
		t.Run(test.request+"/"+test.name+"/"+test.arch, func(t *testing.T) {
			e := &fakeExecutor{t: t, installed: []identity{fixture(test.name, "1-1", test.arch)}}
			got, diff, err := NewPackageManager(e).Test(t.Context(), desired(test.request))
			if err != nil || got.Packages[0].Installed != test.present || (len(diff) == 0) != test.present {
				t.Fatalf("got %+v, %v, %v", got, diff, err)
			}
		})
	}
	e := &fakeExecutor{t: t, installed: []identity{
		fixture("pkg", "1-1", "x86_64"), fixture("pkg", "1-1", "i686"),
	}}
	got, diff, err := NewPackageManager(e).Test(t.Context(), desired("pkg", "pkg:i686"))
	if err != nil || len(diff) != 0 || len(got.Packages) != 2 {
		t.Fatalf("distinct native and multilib identities: %+v, %v, %v", got, diff, err)
	}
}

func TestInvalidInputsAndUnsupportedRemoval(t *testing.T) {
	for _, want := range []packages.Packages{
		desired(), desired(""), desired("--allowerasing"), desired("name*"), desired("$(touch x)"),
		desired("x;true"), desired("name:bad:arch"), desired("http://host/pkg.rpm"),
		desired("same", "same"), desired("pkg", "pkg:x86_64"), desired("pkg:noarch", "pkg:x86_64"),
		desired("pkg:aarch64"),
		{Name: packages.PackageNames{Values: []string{"pkg"}, Scalar: false}, Version: "1-1"},
		{Name: packages.PackageNames{Values: []string{"pkg"}, Scalar: true}, Version: `1%{lua:print(1)}-1`},
		{Name: packages.PackageNames{Values: []string{"pkg"}, Scalar: true}, Version: "1"},
		{Name: packages.PackageNames{Values: []string{"pkg"}, Scalar: true}, Ensure: "Invalid"},
	} {
		e := &fakeExecutor{t: t}
		if _, err := NewPackageManager(e).Set(t.Context(), want); err == nil || e.mutations != 0 {
			t.Fatalf("accepted %+v: %v", want, err)
		}
	}
	for _, operation := range []string{"get", "test", "set"} {
		e := &fakeExecutor{t: t}
		m := NewPackageManager(e)
		want := desired("pkg")
		want.Ensure = "Absent"
		var err error
		switch operation {
		case "get":
			_, err = m.Get(t.Context(), want)
		case "test":
			_, _, err = m.Test(t.Context(), want)
		case "set":
			_, err = m.Set(t.Context(), want)
		}
		if err == nil || !strings.Contains(err.Error(), "not supported") || len(e.calls) != 0 {
			t.Fatalf("removal was not rejected before native commands: %v, %v", err, e.calls)
		}
	}
}

func TestLocalRPMValidationAndIdempotence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "filename-does-not-identify-package.rpm")
	if err := os.WriteFile(path, []byte("original fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	p := fixture("Actual.Package_name", "2:1.0-3", "noarch")
	e := &fakeExecutor{t: t, local: p, installed: []identity{p}}
	m := NewPackageManager(e)
	want := desired(path)
	want.UpdateCache = true
	for _, contents := range []string{"original", "different bytes"} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := m.Set(t.Context(), want)
		if err != nil || got.Packages[0].Name != p.name || got.Packages[0].Version != p.evr() {
			t.Fatalf("local identity: %+v, %v", got, err)
		}
	}
	for _, c := range e.calls {
		if c.program == "dnf" {
			t.Fatal("compliant local package invoked DNF")
		}
	}
	for _, names := range [][]string{{path, p.name}, {path, path + ".missing.rpm"}, {filepath.Dir(path)}} {
		if _, err := m.Set(t.Context(), desired(names...)); err == nil {
			t.Fatalf("accepted invalid local input %q", names)
		}
	}
	for _, bad := range []identity{
		fixture("Bad/name", "1-1", "x86_64"), fixture("pkg", "1-1", "aarch64"),
		{name: "pkg", epoch: "0", version: "", release: "1", arch: "x86_64"},
		{name: "pkg", epoch: "0", version: "1", release: "", arch: "x86_64"},
	} {
		e.local = bad
		if _, err := m.Get(t.Context(), desired(path)); err == nil {
			t.Fatalf("accepted invalid local metadata %+v", bad)
		}
	}
}

func TestBatchTransactionAndNoop(t *testing.T) {
	p, dep := fixture("Package.one", "1:2.0-1", "x86_64"), fixture("dependency", "1-1", "noarch")
	already := fixture("already", "1-1", "x86_64")
	e := &fakeExecutor{t: t, installed: []identity{already}, plan: []action{{p, "Install"}, {dep, "Install"}}}
	m := NewPackageManager(e)
	want := desired("Package.one", "dependency", "already")
	want.UpdateCache = true
	got, err := m.Set(t.Context(), want)
	if err != nil || got.State != "Present" || e.mutations != 1 || e.preflights != 2 {
		t.Fatalf("batch: %+v, %v, mutations %d, preflights %d", got, err, e.mutations, e.preflights)
	}
	refreshes := 0
	for _, c := range e.calls {
		if slices.Contains(c.args, "--refresh") {
			refreshes++
		}
		if c.program == "dnf" && slices.Contains(c.args, "--assumeyes") {
			last := slices.Index(c.args, "--")
			if last < 0 || !slices.Equal(c.args[last+1:], []string{p.nevra(), dep.nevra()}) {
				t.Fatalf("unexpected mutation targets %q", c.args)
			}
		}
		for _, arg := range c.args {
			if arg == "--allowerasing" || arg == "--skip-broken" || arg == "--nogpgcheck" {
				t.Fatalf("unsafe DNF flag %q", arg)
			}
			if strings.HasPrefix(arg, "--setopt=protected_packages=glob:") {
				path := strings.TrimPrefix(arg, "--setopt=protected_packages=glob:")
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("temporary protection file not cleaned: %v", err)
				}
			}
		}
	}
	if refreshes != 1 {
		t.Fatalf("refresh count = %d", refreshes)
	}
	calls := len(e.calls)
	if _, err := m.Set(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	for _, c := range e.calls[calls:] {
		if c.program == "dnf" {
			t.Fatal("already-compliant batch invoked DNF")
		}
	}
}

func TestVersionChangeAndFailedVerification(t *testing.T) {
	for _, test := range []struct {
		name         string
		comparison   int
		allow, fails bool
		kind         string
	}{
		{"upgrade", -1, false, false, "Upgrade"},
		{"blocked downgrade", 1, false, true, "Downgrade"},
		{"allowed downgrade", 1, true, false, "Downgrade"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := fixture("pkg", "2-1", "x86_64")
			e := &fakeExecutor{t: t, comparison: test.comparison,
				installed: []identity{fixture("pkg", "1-1", "x86_64")}, plan: []action{{p, test.kind}}}
			want := desired("pkg")
			want.Version, want.AllowDowngrade = p.evr(), test.allow
			_, err := NewPackageManager(e).Set(t.Context(), want)
			if (err != nil) != test.fails || (e.mutations == 0) != test.fails {
				t.Fatalf("err %v, mutations %d", err, e.mutations)
			}
		})
	}
	e := &fakeExecutor{t: t, noEffect: true, plan: []action{{fixture("pkg", "1-1", "x86_64"), "Install"}}}
	if _, err := NewPackageManager(e).Set(t.Context(), desired("pkg")); err == nil || !strings.Contains(err.Error(), "did not achieve") {
		t.Fatalf("missing postcondition error: %v", err)
	}
}

func TestPlanValidation(t *testing.T) {
	p := fixture("pkg", "1-1", "x86_64")
	for _, plan := range [][]action{
		{{fixture("provides-pkg", "1-1", "x86_64"), "Install"}},
		{{fixture("pkg", "1-1", "i686"), "Install"}},
		{{p, "Install"}, {fixture("pkg", "1-1", "noarch"), "Install"}},
		{{p, "Install"}, {fixture("unrelated", "1-1", "x86_64"), "Downgrade"}},
	} {
		e := &fakeExecutor{t: t, plan: plan}
		if _, err := NewPackageManager(e).Set(t.Context(), desired("pkg")); err == nil || e.mutations != 0 {
			t.Fatalf("unsafe plan accepted: %+v, %v", plan, err)
		}
	}
	e := &fakeExecutor{t: t, installed: []identity{p}, plan: []action{{p, "Upgrade"}, {fixture("new", "1-1", "x86_64"), "Install"}}}
	if _, err := NewPackageManager(e).Set(t.Context(), desired("pkg", "new")); err == nil || e.mutations != 0 {
		t.Fatalf("changed a compliant requested package: %v", err)
	}
}

func TestPlanParsingFailsClosed(t *testing.T) {
	good := formatPlan([]action{{fixture("pkg", "1-1", "x86_64"), "Install"}})
	for _, output := range []string{
		strings.Replace(good, "Installing:", "Removing:", 1),
		strings.Replace(good, "Installing:", "Reinstalling:", 1),
		strings.Replace(good, "Transaction Summary", " replacing victim.x86_64 1-1\nTransaction Summary", 1),
		strings.Replace(good, "Install 1", "Install 2", 1),
		strings.Replace(good, "Install 1", "Remove 1", 1),
		strings.Replace(good, "Transaction Summary", "Transaction Changed", 1),
		strings.Replace(good, "repo 10 k", "repo unknown-size", 1),
		good + "unknown trailer\n", "", "Nothing to do.",
	} {
		if _, err := parsePlan(output); err == nil {
			t.Fatalf("accepted unsafe/malformed plan %q", output)
		}
	}
	wrapped := strings.Replace(good, "pkg x86_64", "pkg\n x86_64", 1)
	if _, err := parsePlan(wrapped); err != nil {
		t.Fatal(err)
	}
}

func TestErrorsCancellationAndChangingState(t *testing.T) {
	for _, program := range []string{"rpm", "dnf"} {
		e := &fakeExecutor{t: t, failProgram: program, failCode: 200}
		_, err := NewPackageManager(e).Set(t.Context(), desired("pkg"))
		var commandError *packages.CommandError
		if !errors.As(err, &commandError) || commandError.ExitCode() != 200 || !strings.Contains(err.Error(), "solver details") {
			t.Fatalf("lost command error: %v", err)
		}
	}
	e := &fakeExecutor{t: t}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewPackageManager(e).Set(ctx, desired("pkg")); !errors.Is(err, context.Canceled) || len(e.calls) != 0 {
		t.Fatalf("canceled command executed: %v", err)
	}
	ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := NewPackageManager(e).Set(ctx, desired("pkg")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline not propagated: %v", err)
	}
	e = &fakeExecutor{t: t, dnfVersion: "dnf5 version 5.2.0"}
	if _, err := NewPackageManager(e).Set(t.Context(), desired("pkg")); err == nil || !strings.Contains(err.Error(), "DNF 4") {
		t.Fatalf("unsupported DNF version accepted: %v", err)
	}
	e = &fakeExecutor{t: t, plan: []action{{fixture("pkg", "1-1", "x86_64"), "Install"}}}
	e.afterPreflight = func(e *fakeExecutor) {
		if e.preflights == 2 {
			e.installed = append(e.installed, fixture("concurrent", "1-1", "x86_64"))
		}
	}
	if _, err := NewPackageManager(e).Set(t.Context(), desired("pkg")); err == nil || !strings.Contains(err.Error(), "inventory changed") || e.mutations != 0 {
		t.Fatalf("inventory drift accepted: %v", err)
	}
	e = &fakeExecutor{t: t, plan: []action{{fixture("pkg", "1-1", "x86_64"), "Install"}}}
	e.afterPreflight = func(e *fakeExecutor) {
		e.plan[0].version = "2"
	}
	if _, err := NewPackageManager(e).Set(t.Context(), desired("pkg")); err == nil || !strings.Contains(err.Error(), "plan changed") || e.mutations != 0 {
		t.Fatalf("plan drift accepted: %v", err)
	}
}

func TestFailedRefreshAndInstallationPreserveStatus(t *testing.T) {
	for _, stage := range []string{"--refresh", "--assumeyes"} {
		e := &fakeExecutor{t: t, failProgram: "dnf", failArgument: stage, failCode: 200,
			plan: []action{{fixture("pkg", "1-1", "x86_64"), "Install"}}}
		want := desired("pkg")
		want.UpdateCache = true
		_, err := NewPackageManager(e).Set(t.Context(), want)
		var commandError *packages.CommandError
		if !errors.As(err, &commandError) || commandError.ExitCode() != 200 ||
			!strings.Contains(err.Error(), "repository or lock unavailable") {
			t.Fatalf("%s lost command failure: %v", stage, err)
		}
		if stage == "--refresh" && e.preflights != 0 {
			t.Fatal("continued after explicit refresh failure")
		}
	}
}

type executorFunc func(context.Context, string, []string, []string) ([]byte, []byte, error)

func (f executorFunc) Execute(ctx context.Context, program string, args, env []string) ([]byte, []byte, error) {
	return f(ctx, program, args, env)
}

func TestInFlightCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := NewPackageManager(executorFunc(func(ctx context.Context, _ string, _, _ []string) ([]byte, []byte, error) {
		cancel()
		<-ctx.Done()
		return []byte("interrupted"), nil, exitError(137)
	}))
	_, err := m.Set(ctx, desired("pkg"))
	var commandError *packages.CommandError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &commandError) || commandError.ExitCode() != 137 {
		t.Fatalf("lost cancellation or exit status: %v", err)
	}
}

func TestMetadataAndEVR(t *testing.T) {
	for _, epoch := range []string{"(none)", "0", "000"} {
		items, err := parseMetadata([]byte("Pkg\t"+epoch+"\t1.2\t3.el9\tnoarch\n"), true)
		if err != nil || len(items) != 1 || items[0].evr() != "0:1.2-3.el9" {
			t.Fatalf("epoch %q: %+v, %v", epoch, items, err)
		}
	}
	for _, value := range []string{"1.0", ":1-1", "x:1-1", "1:2:3-1", "1-2-3", `1";print(1)-1`, "1%-1", "1*-1", "1-"} {
		if _, err := parseEVR(value); err == nil {
			t.Fatalf("accepted EVR %q", value)
		}
	}
}
