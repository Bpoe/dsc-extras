package apt

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
)

func TestParseRemovalActionRecords(t *testing.T) {
	state := removalSnapshot{
		active: map[string]PackageState{"nginx:amd64": {Name: "nginx", Architecture: "amd64", Version: "1"}},
	}
	for _, tc := range []struct {
		name, output string
		removed      map[string]bool
		wantError    bool
	}{
		{name: "installation", output: "Inst nginx (1 Debian [amd64])\nConf nginx (1 Debian [amd64])\n", wantError: true},
		{name: "upgrade", output: "Inst nginx [1] (2 Debian [amd64])\n", wantError: true},
		{name: "configuration", output: "Conf nginx (1 Debian [amd64])\n", wantError: true},
		{name: "purge", output: "Purg nginx [1]\n", wantError: true},
		{name: "removal", output: "Remv nginx [1]\n", removed: map[string]bool{"nginx:amd64": true}},
		{name: "empty no-op"},
		{name: "no-op summary", output: removalOutput()},
		{name: "no-op notice", output: "Use 'sudo apt autoremove' to remove them.\n"},
		{name: "informational output", output: "Additional APT information\n" + removalOutput("nginx") + "Use 'sudo apt autoremove' to remove them.\n", removed: map[string]bool{"nginx:amd64": true}},
		{name: "summary is not an action", output: "0 upgraded, 0 newly installed, 1 to remove and 0 not upgraded.\n"},
		{name: "summary cannot override actions", output: removalOutput() + "Remv nginx [1]\n", removed: map[string]bool{"nginx:amd64": true}},
		{name: "malformed removal", output: "Remv nginx nonsense\n", wantError: true},
		{name: "missing removal identity", output: "Remv\n", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			removed, err := parseRemoval([]byte(tc.output), state)
			if (err != nil) != tc.wantError || !maps.Equal(removed, tc.removed) {
				t.Fatalf("removed = %v, error = %v; want %v, error = %t", removed, err, tc.removed, tc.wantError)
			}
		})
	}
}

func removalStateSteps(inventory string) []packageStep {
	return []packageStep{
		{program: "dpkg-query", args: []string{"--show", packageQueryFormat}, stdout: inventory},
		{program: "apt-config", args: []string{"dump"}, stdout: "APT::Architecture \"amd64\";\n"},
	}
}

func removalOutput(names ...string) string {
	output := fmt.Sprintf("Reading package lists...\nBuilding dependency tree...\nReading state information...\n0 upgraded, 0 newly installed, %d to remove and 0 not upgraded.\n", len(names))
	for _, name := range names {
		output += "Remv " + name + " [1]\n"
	}
	return output
}

func removalSimulation(output string, targets ...string) packageStep {
	return packageStep{program: "apt-get", args: removalArguments(true, targets), stdout: output}
}

func approvedRemovalSteps(inventory, output string, targets ...string) []packageStep {
	steps := removalStateSteps(inventory)
	for range 2 {
		steps = append(steps, removalSimulation(output, targets...))
	}
	steps = append(steps, removalStateSteps(inventory)...)
	return append(steps, packageStep{program: "apt-get", args: removalArguments(false, targets)})
}

func TestRemovalTransactions(t *testing.T) {
	for _, tc := range []struct {
		name, notice        string
		requested, retained []string
	}{
		{name: "single manual", requested: []string{"nginx"}},
		{name: "multiple explicit including manual", requested: []string{"libfoo", "nginx"}},
		{name: "unused dependencies and previous orphan retained", requested: []string{"nginx"}, retained: []string{"libbar", "libfoo", "old-orphan"},
			notice: "The following packages were automatically installed and are no longer required:\n  libbar libfoo old-orphan\nUse 'sudo apt autoremove' to remove them.\n"},
		{name: "single orphan retained", requested: []string{"nginx"}, retained: []string{"old-orphan"},
			notice: "The following package was automatically installed and is no longer required:\n  old-orphan\nUse 'sudo apt autoremove' to remove it.\n"},
		{name: "shared automatic dependency retained", requested: []string{"nginx"}, retained: []string{"libfoo", "other-app"}},
		{name: "manual dependency retained", requested: []string{"nginx"}, retained: []string{"libfoo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := architectureSteps()
			var inventory string
			var targets []string
			for _, name := range tc.requested {
				row := packageInstalled(name, "1", "amd64")
				steps = append(steps, packageQuery(name, row))
				inventory += row
				targets = append(targets, name+":amd64")
			}
			for _, name := range tc.retained {
				inventory += packageInstalled(name, "1", "amd64")
			}
			steps = append(steps, approvedRemovalSteps(inventory, tc.notice+removalOutput(tc.requested...), targets...)...)
			for _, name := range tc.requested {
				steps = append(steps, packageQuery(name, name+"\tdeinstall ok config-files\t1\tamd64\n"))
			}
			desired := packageDesired(tc.requested...)
			desired.Ensure = "Absent"
			result, err := packageManagerFixture(t, steps...).Set(context.Background(), desired)
			if err != nil || result.State != "Absent" {
				t.Fatalf("result = %+v, error = %v", result, err)
			}
		})
	}
}

func TestRemovalRejectsUnrequestedPackages(t *testing.T) {
	inventory := packageInstalled("libexample", "1", "amd64") +
		packageInstalled("nginx", "1", "amd64") + packageInstalled("orphan", "1", "amd64")
	steps := append(architectureSteps(), packageQuery("libexample", packageInstalled("libexample", "1", "amd64")))
	steps = append(steps, removalStateSteps(inventory)...)
	steps = append(steps, removalSimulation(removalOutput("libexample", "nginx", "orphan"), "libexample:amd64"))
	desired := packageDesired("libexample")
	desired.Ensure = "Absent"
	_, err := packageManagerFixture(t, steps...).Set(context.Background(), desired)
	if err == nil || !strings.Contains(err.Error(), "nginx:amd64, orphan:amd64") || !strings.Contains(err.Error(), "explicitly") {
		t.Fatalf("expected actionable refusal without mutation: %v", err)
	}
}

func TestRemovalSimulationFailures(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		err          error
	}{
		{"failed solver", "", packageExit(100)},
		{"failed solver with partial output", "Use 'sudo apt autoremove' to remove them.\n", packageExit(100)},
		{"failed solver with valid plan", removalOutput("nginx"), packageExit(100)},
		{"empty output", "", nil},
		{"summary only", "0 upgraded, 0 newly installed, 1 to remove and 0 not upgraded.\n", nil},
		{"missing target", removalOutput(), nil},
		{"install", removalOutput("nginx") + "Inst new-app (1 Debian [amd64])\n", nil},
		{"upgrade", removalOutput("nginx") + "Inst other-app [1] (2 Debian [amd64])\n", nil},
		{"configure", removalOutput("nginx") + "Conf other-app (1 Debian [amd64])\n", nil},
		{"purge", "0 upgraded, 0 newly installed, 1 to remove and 0 not upgraded.\nPurg nginx [1]\n", nil},
		{"malformed removal", "0 upgraded, 0 newly installed, 1 to remove and 0 not upgraded.\nRemv nginx nonsense\n", nil},
		{"unknown identity", removalOutput("missing"), nil},
		{"wrong version", strings.ReplaceAll(removalOutput("nginx"), "[1]", "[2]"), nil},
		{"duplicate removal", removalOutput("nginx", "nginx"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := removalStateSteps(packageInstalled("nginx", "1", "amd64"))
			step := removalSimulation(tc.output, "nginx:amd64")
			step.err = tc.err
			if tc.err != nil {
				step.stderr = "E: Unable to correct problems, you have held broken packages.\n"
			}
			steps = append(steps, step)
			err := packageManagerFixture(t, steps...).validateRemoval(context.Background(), []string{"nginx:amd64"}, nil)
			if err == nil {
				t.Fatal("accepted unreliable simulation")
			}
			if tc.err != nil {
				var commandErr *PackageCommandError
				if !errors.As(err, &commandErr) || commandErr.ExitCode() != 100 ||
					!errors.Is(err, tc.err) || commandErr.Stdout != tc.output || commandErr.Stderr != step.stderr {
					t.Fatalf("lost APT failure diagnostics: %v", err)
				}
			}
		})
	}
}

func TestRemovalArchitectureIdentity(t *testing.T) {
	inventory := packageInstalled("libfoo:amd64", "1", "amd64") + packageInstalled("libfoo:arm64", "1", "arm64")
	for _, tc := range []struct {
		name, output string
		success      bool
	}{
		{"qualified native only", removalOutput("libfoo:amd64"), true},
		{"foreign reverse dependent", removalOutput("libfoo:amd64", "libfoo:arm64"), false},
		{"ambiguous unqualified", removalOutput("libfoo"), false},
		{"foreign instead", removalOutput("libfoo:arm64"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var steps []packageStep
			if tc.success {
				steps = approvedRemovalSteps(inventory, tc.output, "libfoo:amd64")
				// This test validates planning only.
				steps = steps[:len(steps)-1]
			} else {
				steps = removalStateSteps(inventory)
				steps = append(steps, removalSimulation(tc.output, "libfoo:amd64"))
			}
			err := packageManagerFixture(t, steps...).validateRemoval(context.Background(), []string{"libfoo:amd64"}, nil)
			if (err == nil) != tc.success {
				t.Fatalf("success = %v, error = %v", tc.success, err)
			}
		})
	}
}

func TestRemovalStateFailures(t *testing.T) {
	for _, tc := range []struct {
		name, inventory string
		failCommand     int
	}{
		{name: "dpkg failure", failCommand: 1},
		{name: "bad inventory", inventory: "nginx\tinstalled\n"},
		{name: "config failure", inventory: packageInstalled("nginx", "1", "amd64"), failCommand: 2},
		{name: "empty config", inventory: packageInstalled("nginx", "1", "amd64")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := removalStateSteps(tc.inventory)
			if tc.failCommand != 0 {
				steps = steps[:tc.failCommand]
				steps[len(steps)-1].err = packageExit(2)
			} else if tc.name == "bad inventory" {
				steps = steps[:1]
			} else {
				steps[1].stdout = ""
			}
			if err := packageManagerFixture(t, steps...).validateRemoval(context.Background(), []string{"nginx:amd64"}, nil); err == nil {
				t.Fatal("accepted unreliable state")
			}
		})
	}
}

func TestRemovalDetectsStateAndPlanChanges(t *testing.T) {
	inventory := packageInstalled("nginx", "1", "amd64") + packageInstalled("orphan", "1", "amd64")
	for _, change := range []string{"packages", "config", "plan"} {
		t.Run(change, func(t *testing.T) {
			steps := approvedRemovalSteps(inventory, removalOutput("nginx"), "nginx:amd64")
			steps = steps[:len(steps)-1]
			switch change {
			case "packages":
				steps[4].stdout = strings.ReplaceAll(inventory, "\t1\t", "\t2\t")
			case "config":
				steps[5].stdout += "APT::Get::Purge \"true\";\n"
			case "plan":
				steps[3].stdout = removalOutput("nginx", "orphan")
				steps = steps[:4]
			}
			if err := packageManagerFixture(t, steps...).validateRemoval(context.Background(), []string{"nginx:amd64"}, nil); err == nil {
				t.Fatal("accepted changed state/plan")
			}
		})
	}
}

func TestRemovalAlreadyAbsentNoAPT(t *testing.T) {
	steps := append(architectureSteps(), packageMissing("nginx"))
	desired := packageDesired("nginx")
	desired.Ensure = "Absent"
	result, err := packageManagerFixture(t, steps...).Set(context.Background(), desired)
	if err != nil || result.State != "Absent" {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
}

func TestRemovalArgumentsNeverPurgeOrAutoremove(t *testing.T) {
	for _, simulate := range []bool{false, true} {
		args := removalArguments(simulate, []string{"nginx:amd64"})
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "-o APT::Get::Purge=false") ||
			!strings.Contains(joined, "-o APT::Get::AutomaticRemove=false") ||
			!strings.HasSuffix(joined, "remove -- nginx:amd64") {
			t.Fatalf("unsafe removal arguments: %q", args)
		}
		if strings.Contains(joined, "--purge") || strings.Contains(joined, "--no-remove") ||
			strings.Contains(joined, "--auto-remove") || strings.Contains(joined, "autoremove") ||
			strings.Contains(joined, "allow-remove") {
			t.Fatalf("unexpected removal override: %q", args)
		}
		if strings.Contains(joined, "--simulate") != simulate {
			t.Fatalf("incorrect simulation mode: %q", args)
		}
	}
}

func TestRemovalRepeatedSimulationFailure(t *testing.T) {
	for _, output := range []string{removalOutput("nginx"), "Inst other (1 Debian [amd64])\n"} {
		steps := removalStateSteps(packageInstalled("nginx", "1", "amd64"))
		steps = append(steps, removalSimulation(removalOutput("nginx"), "nginx:amd64"))
		step := removalSimulation(output, "nginx:amd64")
		if output == removalOutput("nginx") {
			step.err = packageExit(100)
			step.stderr = "E: Could not get lock /var/lib/dpkg/lock-frontend\n"
		}
		steps = append(steps, step)
		err := packageManagerFixture(t, steps...).validateRemoval(context.Background(), []string{"nginx:amd64"}, nil)
		if err == nil {
			t.Fatal("accepted failed repeated simulation")
		}
		if step.err != nil {
			var commandErr *PackageCommandError
			if !errors.As(err, &commandErr) || commandErr.ExitCode() != 100 || commandErr.Stderr != step.stderr {
				t.Fatalf("lost repeated simulation failure: %v", err)
			}
		}
	}
}

func TestRemovalNativeAutomaticPackageNotices(t *testing.T) {
	state := removalSnapshot{
		active: map[string]PackageState{"nginx:amd64": {Name: "nginx", Architecture: "amd64", Version: "1"}},
	}
	for _, notice := range []struct{ header, instruction string }{
		{"The following package was automatically installed and is no longer required:", "Use 'apt autoremove' to remove it."},
		{"The following packages were automatically installed and are no longer required:", "Use 'apt autoremove' to remove them."},
		{"The following package was automatically installed and is no longer required:", "Use 'sudo apt autoremove' to remove it."},
		{"The following packages were automatically installed and are no longer required:", "Use 'sudo apt autoremove' to remove them."},
	} {
		output := notice.header + "\n  old-orphan\n" + notice.instruction + "\n" + removalOutput("nginx")
		if _, err := parseRemoval([]byte(output), state); err != nil {
			t.Fatalf("rejected native automatic-package notice: %v", err)
		}
	}
}

func TestRemovalDetectsChangeBeforePlanning(t *testing.T) {
	steps := removalStateSteps(packageInstalled("nginx", "2", "amd64"))
	before := []PackageState{{Name: "nginx", Architecture: "amd64", Status: "install ok installed", Version: "1"}}
	if err := packageManagerFixture(t, steps...).validateRemoval(context.Background(), []string{"nginx:amd64"}, before); err == nil {
		t.Fatal("accepted stale initial observation")
	}
}

func TestRemovalExecutionAndVerificationErrors(t *testing.T) {
	for _, failure := range []string{"execution", "query", "still present"} {
		t.Run(failure, func(t *testing.T) {
			row := packageInstalled("nginx", "1", "amd64")
			steps := append(architectureSteps(), packageQuery("nginx", row))
			steps = append(steps, approvedRemovalSteps(row, removalOutput("nginx"), "nginx:amd64")...)
			switch failure {
			case "execution":
				steps[len(steps)-1].err = packageExit(100)
			case "query":
				step := packageQuery("nginx", "")
				step.err = packageExit(2)
				steps = append(steps, step)
			case "still present":
				steps = append(steps, packageQuery("nginx", row))
			}
			desired := packageDesired("nginx")
			desired.Ensure = "Absent"
			_, err := packageManagerFixture(t, steps...).Set(context.Background(), desired)
			if err == nil {
				t.Fatal("lost transaction/verification failure")
			}
		})
	}
}
