package apt

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type packageExit int

func (e packageExit) Error() string { return "fake command exit" }
func (e packageExit) ExitCode() int { return int(e) }

type packageStep struct {
	program, stdout, stderr string
	args                    []string
	err                     error
}

type fakePackageExecutor struct {
	t     *testing.T
	steps []packageStep
}

func (f *fakePackageExecutor) Execute(ctx context.Context, program string, args, env []string) ([]byte, []byte, error) {
	f.t.Helper()
	if len(f.steps) == 0 {
		f.t.Fatalf("unexpected execution: %s %q", program, args)
	}
	step := f.steps[0]
	f.steps = f.steps[1:]
	if step.program != program || !slices.Equal(step.args, args) {
		f.t.Fatalf("execution = %s %q; want %s %q", program, args, step.program, step.args)
	}
	for _, entry := range []string{"LC_ALL=C", "LANG=C", "DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none"} {
		if !slices.Contains(env, entry) {
			f.t.Errorf("missing environment override %q", entry)
		}
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 15*time.Minute {
		f.t.Errorf("command must have a finite deadline: %v", deadline)
	}
	return []byte(step.stdout), []byte(step.stderr), step.err
}

func packageManagerFixture(t *testing.T, steps ...packageStep) *PackageManager {
	t.Helper()
	fake := &fakePackageExecutor{t: t, steps: steps}
	t.Cleanup(func() {
		if len(fake.steps) != 0 {
			t.Errorf("%d expected command(s) were not executed; next %s %q", len(fake.steps), fake.steps[0].program, fake.steps[0].args)
		}
	})
	return NewPackageManager(fake)
}

func architectureSteps() []packageStep {
	return []packageStep{
		{program: "dpkg", args: []string{"--print-architecture"}, stdout: "amd64\n"},
		{program: "dpkg", args: []string{"--print-foreign-architectures"}, stdout: "arm64\n"},
	}
}

func packageQuery(name, stdout string) packageStep {
	return packageStep{program: "dpkg-query", args: []string{"--show", packageQueryFormat, "--", name}, stdout: stdout}
}

func packageMissing(name string) packageStep {
	step := packageQuery(name, "")
	step.err, step.stderr = packageExit(1), "dpkg-query: no packages found matching "+name+"\n"
	return step
}

func packageInstalled(name, version, arch string) string {
	return name + "\tinstall ok installed\t" + version + "\t" + arch + "\n"
}

func packageVersion(version string) packageStep {
	return packageStep{program: "dpkg", args: []string{"--validate-version", version}}
}

func packageComparison(left, operator, right string, equal bool) packageStep {
	step := packageStep{program: "dpkg", args: []string{"--compare-versions", left, operator, right}}
	if !equal {
		step.err = packageExit(1)
	}
	return step
}

func packageDesired(names ...string) Packages {
	return Packages{Name: PackageNames{Values: names}}
}

func packageInstall(allow bool, names ...string) packageStep {
	args := []string{"-o", "Dpkg::Use-Pty=0", "-o", "APT::Cmd::Pattern-Only=true", "--yes", "--no-remove"}
	if allow {
		args = append(args, "--allow-downgrades")
	}
	args = append(args, "install", "--")
	for _, name := range names {
		if !filepath.IsAbs(name) {
			base, version, pinned := strings.Cut(name, "=")
			if !strings.Contains(base, ":") {
				base += ":amd64"
			}
			name = base
			if pinned {
				name += "=" + version
			}
		}
		args = append(args, name)
	}
	return packageStep{program: "apt-get", args: args}
}

func packageDeb(t *testing.T, name string) string {
	t.Helper()
	// Keep test artifacts in the repository, never in the host's temporary area.
	directory, err := os.MkdirTemp(".", ".packages-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path, err := filepath.Abs(filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fake .deb; only the executor reads metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func packageMetadata(path, name, version, architecture string) packageStep {
	return packageStep{
		program: "dpkg-deb",
		args:    []string{"--show", "--showformat=${Package}\t${Version}\t${Architecture}\n", "--", path},
		stdout:  name + "\t" + version + "\t" + architecture + "\n",
	}
}

func TestPackageNamesJSON(t *testing.T) {
	for _, input := range []string{`"bash"`, `["bash"]`, `["zsh","bash"]`, `[]`} {
		t.Run(input, func(t *testing.T) {
			var names PackageNames
			if err := json.Unmarshal([]byte(input), &names); err != nil {
				t.Fatal(err)
			}
			output, err := json.Marshal(names)
			if err != nil || string(output) != input {
				t.Fatalf("round trip = %s, %v", output, err)
			}
		})
	}
	for _, input := range []string{`null`, `1`, `{}`, `true`, `["bash",2]`} {
		var names PackageNames
		if json.Unmarshal([]byte(input), &names) == nil {
			t.Errorf("accepted %s", input)
		}
	}
	if _, err := json.Marshal(PackageNames{Scalar: true}); err == nil {
		t.Error("accepted malformed scalar")
	}
}

func TestPackagesValidateBeforeExecution(t *testing.T) {
	cases := []struct {
		name    string
		desired Packages
	}{
		{"empty", packageDesired()},
		{"empty token", packageDesired("")},
		{"one character", packageDesired("a")},
		{"option", packageDesired("--allow-unauthenticated")},
		{"version syntax", packageDesired("bash=1")},
		{"release syntax", packageDesired("bash/stable")},
		{"glob", packageDesired("bash*")},
		{"regex", packageDesired("?name(bash)")},
		{"shell", packageDesired("bash;touch x")},
		{"whitespace", packageDesired(" bash")},
		{"newline", packageDesired("bash\n")},
		{"bad arch", packageDesired("bash:amd64:all")},
		{"option arch", packageDesired("bash:-amd64")},
		{"duplicate", packageDesired("bash", "bash")},
		{"bad ensure", Packages{Name: PackageNames{Values: []string{"bash"}}, Ensure: "Mixed"}},
		{"multiple version", Packages{Name: PackageNames{Values: []string{"bash", "zsh"}}, Version: "1"}},
		{"singleton array version", Packages{Name: PackageNames{Values: []string{"bash"}}, Version: "1"}},
		{"absent version", Packages{Name: PackageNames{Values: []string{"bash"}}, Ensure: "Absent", Version: "1"}},
		{"absent downgrade", Packages{Name: PackageNames{Values: []string{"bash"}}, Ensure: "Absent", AllowDowngrade: true}},
		{"absent update", Packages{Name: PackageNames{Values: []string{"bash"}}, Ensure: "Absent", UpdateCache: true}},
		{"scalar list", Packages{Name: PackageNames{Values: []string{"bash", "zsh"}, Scalar: true}}},
		{"missing local", packageDesired("./not-existing-package.deb")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := packageManagerFixture(t).Test(context.Background(), tc.desired)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestPackagesReadOnlyAggregate(t *testing.T) {
	cases := []struct {
		name, first, second, state string
		differences                []string
	}{
		{"installed", packageInstalled("bash", "1", "amd64"), packageInstalled("zsh", "2", "all"), "Present", nil},
		{"missing", "", "", "Absent", []string{"ensure"}},
		{"mixed", packageInstalled("bash", "1", "amd64"), "", "Mixed", []string{"ensure"}},
		{"partial", "bash\tinstall ok unpacked\t1\tamd64\n", packageInstalled("zsh", "2", "amd64"), "Mixed", []string{"ensure"}},
		{"reinstall required", "bash\tinstall reinstreq installed\t1\tamd64\n", "", "Mixed", []string{"ensure"}},
		{"residual configs", "bash\tdeinstall ok config-files\t1\tamd64\n", "", "Absent", []string{"ensure"}},
		{"foreign only", packageInstalled("bash:arm64", "1", "arm64"), "", "Absent", []string{"ensure"}},
		{"native and foreign", packageInstalled("bash:arm64", "9", "arm64") + packageInstalled("bash:amd64", "1", "amd64"), packageInstalled("zsh", "2", "all"), "Present", nil},
		{"held", "bash\thold ok installed\t1\tamd64\n", packageInstalled("zsh", "2", "amd64"), "Present", nil},
		{"not installed record", "bash\tunknown ok not-installed\t\t\n", "", "Absent", []string{"ensure"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps := architectureSteps()
			for _, row := range []struct{ name, output string }{{"bash", tc.first}, {"zsh", tc.second}} {
				if row.output == "" {
					steps = append(steps, packageMissing(row.name))
				} else {
					steps = append(steps, packageQuery(row.name, row.output))
				}
			}
			manager := packageManagerFixture(t, steps...)
			desired := packageDesired("zsh", "bash")
			result, differences, err := manager.Test(context.Background(), desired)
			if err != nil {
				t.Fatal(err)
			}
			if result.State != tc.state || result.Ensure != "Present" || !slices.Equal(differences, tc.differences) {
				t.Fatalf("result = %+v, differences %v", result, differences)
			}
			if differences == nil {
				t.Fatal("successful Test differences must be [] rather than null")
			}
			if !slices.Equal(result.Name.Values, []string{"bash", "zsh"}) || desired.Name.Values[0] != "zsh" {
				t.Fatal("names must be sorted without mutating caller input")
			}
			if len(result.Packages) != 2 || result.Packages[0].Name != "bash" || result.Packages[1].Name != "zsh" {
				t.Fatalf("missing per-package observations: %+v", result.Packages)
			}
		})
	}
}

func TestPackagesMissingVersusFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		step packageStep
	}{
		{"database error", packageStep{program: "dpkg-query", args: []string{"--show", packageQueryFormat, "--", "bash"}, err: packageExit(2), stderr: "database corrupt"}},
		{"exit one unknown", packageStep{program: "dpkg-query", args: []string{"--show", packageQueryFormat, "--", "bash"}, err: packageExit(1), stderr: "permission denied"}},
		{"missing plus error", packageStep{program: "dpkg-query", args: []string{"--show", packageQueryFormat, "--", "bash"}, err: packageExit(1), stderr: "dpkg-query: no packages found matching bash\npermission denied"}},
		{"partial output error", packageStep{program: "dpkg-query", args: []string{"--show", packageQueryFormat, "--", "bash"}, err: packageExit(1), stdout: packageInstalled("bash", "1", "amd64"), stderr: "dpkg-query: no packages found matching bash"}},
		{"malformed", packageQuery("bash", "bash\tinstalled\n")},
		{"unexpected identity", packageQuery("bash", packageInstalled("zsh", "1", "amd64"))},
		{"unexpected status", packageQuery("bash", "bash\tinstall ok mysterious\t1\tamd64\n")},
		{"ambiguous", packageQuery("bash", packageInstalled("bash", "1", "amd64")+packageInstalled("bash", "1", "all"))},
		{"empty successful output", packageQuery("bash", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := packageManagerFixture(t, append(architectureSteps(), tc.step)...)
			if _, err := manager.Get(context.Background(), packageDesired("bash")); err == nil {
				t.Fatal("genuine failure was treated as absence")
			}
		})
	}
}

func TestPackagesSetMinimalTransaction(t *testing.T) {
	steps := append(architectureSteps(),
		packageQuery("bash", packageInstalled("bash", "1", "amd64")),
		packageMissing("zsh"),
		packageStep{program: "apt-get", args: []string{"-o", "Dpkg::Use-Pty=0", "-o", "APT::Update::Error-Mode=any", "update"}},
		packageInstall(false, "zsh"),
		packageQuery("bash", packageInstalled("bash", "1", "amd64")),
		packageQuery("zsh", packageInstalled("zsh", "2", "amd64")),
	)
	desired := packageDesired("zsh", "bash")
	desired.UpdateCache = true
	result, err := packageManagerFixture(t, steps...).Set(context.Background(), desired)
	if err != nil || result.State != "Present" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestPackagesSetNoOpDoesNotUpdate(t *testing.T) {
	desired := packageDesired("bash")
	desired.UpdateCache = true
	steps := append(architectureSteps(), packageQuery("bash", packageInstalled("bash", "1", "amd64")))
	if _, err := packageManagerFixture(t, steps...).Set(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
}

func TestPackagesSetRemoveOnlyActive(t *testing.T) {
	desired := packageDesired("zsh", "bash")
	desired.Ensure = "Absent"
	steps := append(architectureSteps(),
		packageQuery("bash", "bash\tinstall ok half-configured\t1\tamd64\n"),
		packageQuery("zsh", "zsh\tdeinstall ok config-files\t2\tamd64\n"),
		packageStep{program: "apt-get", args: []string{"-o", "Dpkg::Use-Pty=0", "-o", "APT::Cmd::Pattern-Only=true", "--yes", "remove", "--", "bash:amd64"}},
		packageMissing("bash"),
		packageQuery("zsh", "zsh\tdeinstall ok config-files\t2\tamd64\n"),
	)
	result, err := packageManagerFixture(t, steps...).Set(context.Background(), desired)
	if err != nil || result.State != "Absent" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestPackagesSetVersions(t *testing.T) {
	for _, tc := range []struct {
		name, installed, target string
		equal, downgrade, allow bool
	}{
		{"native equivalent", "1", "1-0", true, false, false},
		{"upgrade epoch", "1:9", "2:1", false, false, false},
		{"downgrade denied", "2", "1", false, true, false},
		{"downgrade allowed", "2", "1", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := packageDesired("bash")
			desired.Name.Scalar = true
			desired.Version, desired.AllowDowngrade = tc.target, tc.allow
			steps := append(architectureSteps(), packageVersion(tc.target),
				packageQuery("bash", packageInstalled("bash", tc.installed, "amd64")),
				packageComparison(tc.installed, "eq", tc.target, tc.equal))
			if !tc.equal {
				steps = append(steps, packageComparison(tc.installed, "gt", tc.target, tc.downgrade))
				if !tc.downgrade || tc.allow {
					steps = append(steps, packageInstall(tc.allow, "bash="+tc.target),
						packageQuery("bash", packageInstalled("bash", tc.target, "amd64")),
						packageComparison(tc.target, "eq", tc.target, true))
				}
			}
			result, err := packageManagerFixture(t, steps...).Set(context.Background(), desired)
			if tc.downgrade && !tc.allow {
				if err == nil || !strings.Contains(err.Error(), "allowDowngrade") {
					t.Fatalf("expected downgrade refusal: %v", err)
				}
			} else if err != nil || result.State != "Present" {
				t.Fatalf("result = %+v, err = %v", result, err)
			}
		})
	}
}

func TestPackagesLocalDebInstallAndArchitectures(t *testing.T) {
	for _, architecture := range []string{"amd64", "all", "arm64"} {
		t.Run(architecture, func(t *testing.T) {
			path := packageDeb(t, "local with spaces.deb")
			steps := append(architectureSteps(), packageMetadata(path, "local-app", "1:2.3-4", architecture),
				packageVersion("1:2.3-4"), packageMissing("local-app"),
				packageInstall(false, path),
				packageQuery("local-app", packageInstalled("local-app", "1:2.3-4", architecture)),
				packageComparison("1:2.3-4", "eq", "1:2.3-4", true))
			result, err := packageManagerFixture(t, steps...).Set(context.Background(), packageDesired(path))
			if err != nil || len(result.Packages) != 1 || result.Packages[0].Architecture != architecture ||
				result.Packages[0].RequestedName != path || result.Packages[0].Name != "local-app" {
				t.Fatalf("result = %+v, err = %v", result, err)
			}
		})
	}
}

func TestPackagesLocalValidation(t *testing.T) {
	path := packageDeb(t, "local.deb")
	for _, tc := range []struct {
		name    string
		desired Packages
		steps   []packageStep
	}{
		{"absent", Packages{Name: PackageNames{Values: []string{path}}, Ensure: "Absent"}, nil},
		{"version", Packages{Name: PackageNames{Values: []string{path}}, Version: "1"}, nil},
		{"unregistered architecture", packageDesired(path), append(architectureSteps(), packageMetadata(path, "local-app", "1", "riscv64"))},
		{"invalid package", packageDesired(path), append(architectureSteps(), packageMetadata(path, "--danger", "1", "amd64"))},
		{"invalid architecture", packageDesired(path), append(architectureSteps(), packageMetadata(path, "local-app", "1", "amd64\n"))},
		{"empty version", packageDesired(path), append(architectureSteps(), packageMetadata(path, "local-app", "", "amd64"))},
		{"newline version", packageDesired(path), append(architectureSteps(), packageMetadata(path, "local-app", "1\n2", "amd64"))},
		{"duplicate identity", packageDesired(path, "local-app"), append(architectureSteps(), packageMetadata(path, "local-app", "1", "all"), packageVersion("1"))},
		{"alias duplicate", packageDesired("local-app", "local-app:amd64"), architectureSteps()},
		{"all versus foreign", packageDesired("local-app:all", "local-app:arm64"), architectureSteps()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := packageManagerFixture(t, tc.steps...).Get(context.Background(), tc.desired); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	directory := filepath.Join(filepath.Dir(path), "directory.deb")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := packageManagerFixture(t).Get(context.Background(), packageDesired(directory)); err == nil {
		t.Fatal("accepted directory as .deb")
	}
}

func TestPackagesNativeValidationAndComparisonErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []packageStep
	}{
		{"invalid version", append(architectureSteps(), packageStep{program: "dpkg", args: []string{"--validate-version", "invalid"}, err: packageExit(2), stderr: "invalid version"})},
		{"compare failure", append(architectureSteps(), packageVersion("invalid"), packageQuery("bash", packageInstalled("bash", "1", "amd64")), packageStep{program: "dpkg", args: []string{"--compare-versions", "1", "eq", "invalid"}, err: packageExit(2), stderr: "comparison failed"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := packageDesired("bash")
			desired.Name.Scalar = true
			desired.Version = "invalid"
			if _, _, err := packageManagerFixture(t, tc.steps...).Test(context.Background(), desired); err == nil {
				t.Fatal("native command error was ignored")
			}
		})
	}
}

func TestPackagesSetPostVerification(t *testing.T) {
	steps := append(architectureSteps(), packageMissing("bash"), packageInstall(false, "bash"), packageMissing("bash"))
	if _, err := packageManagerFixture(t, steps...).Set(context.Background(), packageDesired("bash")); err == nil ||
		!strings.Contains(err.Error(), "did not achieve") {
		t.Fatalf("expected post-verification error: %v", err)
	}
}

func TestPackagesCommandFailurePreservesDiagnostics(t *testing.T) {
	failure := packageInstall(false, "bash")
	failure.stdout, failure.stderr, failure.err = "some output", "permission denied", packageExit(100)
	steps := append(architectureSteps(), packageMissing("bash"), failure)
	_, err := packageManagerFixture(t, steps...).Set(context.Background(), packageDesired("bash"))
	var commandError *PackageCommandError
	var exit packageExit
	if !errors.As(err, &commandError) || !errors.As(err, &exit) || commandError.ExitCode() != 100 ||
		commandError.Stdout != failure.stdout || commandError.Stderr != failure.stderr ||
		!strings.Contains(err.Error(), "permission denied") || !reflect.DeepEqual(commandError.Args, failure.args) {
		t.Fatalf("command diagnostics lost: %v", err)
	}
}

func TestPackagesUpdateFailureStopsTransaction(t *testing.T) {
	desired := packageDesired("bash")
	desired.UpdateCache = true
	steps := append(architectureSteps(), packageMissing("bash"),
		packageStep{program: "apt-get", args: []string{"-o", "Dpkg::Use-Pty=0", "-o", "APT::Update::Error-Mode=any", "update"}, err: packageExit(100)})
	if _, err := packageManagerFixture(t, steps...).Set(context.Background(), desired); err == nil {
		t.Fatal("ignored failed cache refresh")
	}
}

type packageExecutorFunc func(context.Context, string, []string, []string) ([]byte, []byte, error)

func (f packageExecutorFunc) Execute(ctx context.Context, program string, args, env []string) ([]byte, []byte, error) {
	return f(ctx, program, args, env)
}

func TestPackagesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := packageManagerFixture(t).Get(ctx, packageDesired("bash")); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancellation lost: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	manager := NewPackageManager(packageExecutorFunc(func(ctx context.Context, _ string, _, _ []string) ([]byte, []byte, error) {
		cancel()
		<-ctx.Done()
		return []byte("out"), []byte("err"), packageExit(137)
	}))
	_, err := manager.Get(ctx, packageDesired("bash"))
	var commandError *PackageCommandError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &commandError) || commandError.Stderr != "err" || commandError.ExitCode() != 137 {
		t.Fatalf("in-flight cancellation or diagnostics lost: %v", err)
	}
}

func TestPackagesDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	manager := NewPackageManager(packageExecutorFunc(func(ctx context.Context, _ string, _, _ []string) ([]byte, []byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}))
	if _, err := manager.Get(ctx, packageDesired("bash")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline not propagated: %v", err)
	}
}

func TestPackagesCancellationIsNotMistakenForExitOne(t *testing.T) {
	for _, stage := range []string{"query", "compare"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			manager := NewPackageManager(packageExecutorFunc(func(_ context.Context, program string, args, _ []string) ([]byte, []byte, error) {
				switch {
				case args[0] == "--print-architecture":
					return []byte("amd64"), nil, nil
				case args[0] == "--print-foreign-architectures", args[0] == "--validate-version":
					return nil, nil, nil
				case program == "dpkg-query" && stage == "compare":
					return []byte(packageInstalled("bash", "1", "amd64")), nil, nil
				default:
					cancel()
					return nil, []byte("dpkg-query: no packages found matching bash\n"), packageExit(1)
				}
			}))
			desired := packageDesired("bash")
			if stage == "compare" {
				desired.Name.Scalar = true
				desired.Version = "2"
			}
			if _, err := manager.Get(ctx, desired); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was treated as absent or unequal: %v", err)
			}
		})
	}
}

func TestPackagesForeignSelectorAndAbsentAll(t *testing.T) {
	desired := packageDesired("bash:arm64")
	steps := append(architectureSteps(), packageQuery("bash", packageInstalled("bash:amd64", "1", "amd64")+packageInstalled("bash:arm64", "2", "arm64")))
	result, err := packageManagerFixture(t, steps...).Get(context.Background(), desired)
	if err != nil || result.Packages[0].Architecture != "arm64" || result.Packages[0].Version != "2" {
		t.Fatalf("foreign selector = %+v, %v", result, err)
	}
	desired = packageDesired("bash")
	desired.Ensure = "Absent"
	steps = append(architectureSteps(),
		packageQuery("bash", packageInstalled("bash", "1", "all")),
		packageStep{program: "apt-get", args: []string{"-o", "Dpkg::Use-Pty=0", "-o", "APT::Cmd::Pattern-Only=true", "--yes", "remove", "--", "bash:all"}},
		packageMissing("bash"))
	if _, err := packageManagerFixture(t, steps...).Set(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
}

func TestPackagesLiteralNamesAndBatchInstall(t *testing.T) {
	names := []string{"g++", "python3.12", "some-package-"}
	steps := architectureSteps()
	for _, name := range names {
		steps = append(steps, packageMissing(name))
	}
	steps = append(steps, packageStep{
		program: "apt-get",
		args: []string{"-o", "Dpkg::Use-Pty=0", "-o", "APT::Cmd::Pattern-Only=true", "--yes", "--no-remove",
			"install", "--", "g++:amd64", "python3.12:amd64", "some-package-:amd64"},
	})
	for _, name := range names {
		steps = append(steps, packageQuery(name, packageInstalled(name, "1", "amd64")))
	}
	if _, err := packageManagerFixture(t, steps...).Set(context.Background(), packageDesired(names...)); err != nil {
		t.Fatal(err)
	}
}

func TestPackagesMixedLocalAndRepository(t *testing.T) {
	path := packageDeb(t, "app.deb")
	steps := append(architectureSteps(),
		packageMetadata(path, "local-app", "2", "amd64"), packageVersion("2"),
		packageMissing("local-app"), packageMissing("bash"),
		packageInstall(false, path, "bash"),
		packageQuery("local-app", packageInstalled("local-app", "2", "amd64")),
		packageComparison("2", "eq", "2", true),
		packageQuery("bash", packageInstalled("bash", "1", "amd64")),
	)
	if _, err := packageManagerFixture(t, steps...).Set(context.Background(), packageDesired("bash", path)); err != nil {
		t.Fatal(err)
	}
}

func TestPackagesLocalDowngradeDeniedBeforeCacheUpdate(t *testing.T) {
	path := packageDeb(t, "app.deb")
	desired := packageDesired(path)
	desired.UpdateCache = true
	steps := append(architectureSteps(),
		packageMetadata(path, "local-app", "1", "amd64"), packageVersion("1"),
		packageQuery("local-app", packageInstalled("local-app", "2", "amd64")),
		packageComparison("2", "eq", "1", false),
		packageComparison("2", "gt", "1", true),
	)
	if _, err := packageManagerFixture(t, steps...).Set(context.Background(), desired); err == nil {
		t.Fatal("local downgrade must be explicitly approved")
	}
}

func TestPackagesResolveCommandErrors(t *testing.T) {
	path := packageDeb(t, "app.deb")
	for _, tc := range []struct {
		name  string
		steps []packageStep
	}{
		{"native command", []packageStep{{program: "dpkg", args: []string{"--print-architecture"}, err: packageExit(2)}}},
		{"native output", []packageStep{{program: "dpkg", args: []string{"--print-architecture"}, stdout: "invalid\noutput"}}},
		{"foreign command", []packageStep{
			{program: "dpkg", args: []string{"--print-architecture"}, stdout: "amd64"},
			{program: "dpkg", args: []string{"--print-foreign-architectures"}, err: packageExit(2)},
		}},
		{"foreign output", []packageStep{
			{program: "dpkg", args: []string{"--print-architecture"}, stdout: "amd64"},
			{program: "dpkg", args: []string{"--print-foreign-architectures"}, stdout: "--bad"},
		}},
		{"deb command", append(architectureSteps(), packageStep{
			program: "dpkg-deb", args: []string{"--show", "--showformat=${Package}\t${Version}\t${Architecture}\n", "--", path}, err: packageExit(2),
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := packageManagerFixture(t, tc.steps...).Set(context.Background(), packageDesired(path)); err == nil {
				t.Fatal("expected command/output error")
			}
		})
	}
}

func TestPackagesSetPostQueryAndComparisonErrors(t *testing.T) {
	steps := append(architectureSteps(), packageMissing("bash"), packageInstall(false, "bash"),
		packageStep{program: "dpkg-query", args: []string{"--show", packageQueryFormat, "--", "bash"}, err: packageExit(2)})
	if _, err := packageManagerFixture(t, steps...).Set(context.Background(), packageDesired("bash")); err == nil {
		t.Fatal("ignored post-query error")
	}
	desired := packageDesired("bash")
	desired.Name.Scalar = true
	desired.Version = "1"
	steps = append(architectureSteps(), packageVersion("1"),
		packageQuery("bash", packageInstalled("bash", "2", "amd64")), packageComparison("2", "eq", "1", false),
		packageStep{program: "dpkg", args: []string{"--compare-versions", "2", "gt", "1"}, err: packageExit(2)})
	if _, err := packageManagerFixture(t, steps...).Set(context.Background(), desired); err == nil {
		t.Fatal("ignored downgrade-comparison error")
	}
}
