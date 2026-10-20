package apt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func removalStateSteps(inventory, automatic string) []packageStep {
	return []packageStep{
		{program: "dpkg-query", args: []string{"--show", packageQueryFormat}, stdout: inventory},
		{program: "apt-mark", args: []string{"showauto"}, stdout: automatic},
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

func removalSimulation(cleanup bool, output string, targets ...string) packageStep {
	return packageStep{program: "apt-get", args: removalArguments(true, cleanup, targets), stdout: output}
}

func approvedRemovalSteps(inventory, automatic, base, cleanup string, targets ...string) []packageStep {
	steps := removalStateSteps(inventory, automatic)
	for range 2 {
		steps = append(steps, removalSimulation(false, base, targets...), removalSimulation(true, cleanup, targets...))
	}
	steps = append(steps, removalStateSteps(inventory, automatic)...)
	return append(steps, packageStep{program: "apt-get", args: removalArguments(false, true, targets)})
}

func TestRemovalTransactions(t *testing.T) {
	for _, tc := range []struct {
		name, automatic              string
		requested, cleanup, retained []string
	}{
		{name: "single manual", requested: []string{"nginx"}},
		{name: "multiple explicit including manual", requested: []string{"libfoo", "nginx"}},
		{name: "unused automatic dependencies and previous orphan", requested: []string{"nginx"}, cleanup: []string{"libbar", "libfoo", "old-orphan"}, automatic: "libbar\nlibfoo\nold-orphan\n"},
		{name: "shared automatic dependency retained", requested: []string{"nginx"}, retained: []string{"libfoo", "other-app"}, automatic: "libfoo\n"},
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
			for _, name := range append(tc.cleanup, tc.retained...) {
				inventory += packageInstalled(name, "1", "amd64")
			}
			cleanup := append(append([]string{}, tc.requested...), tc.cleanup...)
			steps = append(steps, approvedRemovalSteps(inventory, tc.automatic,
				removalOutput(tc.requested...), removalOutput(cleanup...), targets...)...)
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

func TestRemovalRejectsReverseDependenciesBeforeCleanup(t *testing.T) {
	for _, automatic := range []string{"", "nginx\n", "nginx\norphan\n"} {
		t.Run(automatic, func(t *testing.T) {
			inventory := packageInstalled("libexample", "1", "amd64") +
				packageInstalled("nginx", "1", "amd64") + packageInstalled("orphan", "1", "amd64")
			steps := append(architectureSteps(), packageQuery("libexample", packageInstalled("libexample", "1", "amd64")))
			steps = append(steps, removalStateSteps(inventory, automatic)...)
			steps = append(steps, removalSimulation(false, removalOutput("libexample", "nginx"), "libexample:amd64"))
			desired := packageDesired("libexample")
			desired.Ensure = "Absent"
			_, err := packageManagerFixture(t, steps...).Set(context.Background(), desired)
			if err == nil || !strings.Contains(err.Error(), "nginx:amd64") || !strings.Contains(err.Error(), "explicitly") {
				t.Fatalf("expected actionable refusal without mutation: %v", err)
			}
		})
	}
}

func TestRemovalRejectsManualCleanup(t *testing.T) {
	inventory := packageInstalled("nginx", "1", "amd64") + packageInstalled("manual", "1", "amd64") + packageInstalled("orphan", "1", "amd64")
	steps := removalStateSteps(inventory, "orphan\n")
	steps = append(steps, removalSimulation(false, removalOutput("nginx"), "nginx:amd64"),
		removalSimulation(true, removalOutput("nginx", "manual", "orphan"), "nginx:amd64"))
	err := packageManagerFixture(t, steps...).validateRemoval(context.Background(), []string{"nginx:amd64"}, nil)
	if err == nil || !strings.Contains(err.Error(), "manual:amd64") {
		t.Fatalf("expected manual cleanup refusal: %v", err)
	}
}

func TestRemovalSimulationFailures(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		err          error
	}{
		{"failed solver", "", packageExit(100)},
		{"empty output", "", nil},
		{"summary only", "0 upgraded, 0 newly installed, 1 to remove and 0 not upgraded.\n", nil},
		{"missing target", removalOutput(), nil},
		{"install", removalOutput("nginx") + "Inst new-app (1 Debian [amd64])\n", nil},
		{"upgrade", "1 upgraded, 0 newly installed, 1 to remove and 0 not upgraded.\nRemv nginx [1]\n", nil},
		{"configure", removalOutput("nginx") + "Conf other-app (1 Debian [amd64])\n", nil},
		{"purge", "0 upgraded, 0 newly installed, 1 to remove and 0 not upgraded.\nPurg nginx [1]\n", nil},
		{"malformed removal", "0 upgraded, 0 newly installed, 1 to remove and 0 not upgraded.\nRemv nginx nonsense\n", nil},
		{"unknown identity", removalOutput("missing"), nil},
		{"wrong version", strings.ReplaceAll(removalOutput("nginx"), "[1]", "[2]"), nil},
		{"duplicate removal", removalOutput("nginx", "nginx"), nil},
		{"duplicate summary", removalOutput("nginx") + "0 upgraded, 0 newly installed, 1 to remove and 0 not upgraded.\n", nil},
		{"unknown output", removalOutput("nginx") + "Unexpected transaction\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := removalStateSteps(packageInstalled("nginx", "1", "amd64"), "")
			step := removalSimulation(false, tc.output, "nginx:amd64")
			step.err = tc.err
			steps = append(steps, step)
			err := packageManagerFixture(t, steps...).validateRemoval(context.Background(), []string{"nginx:amd64"}, nil)
			if err == nil {
				t.Fatal("accepted unreliable simulation")
			}
			if tc.err != nil {
				var commandErr *PackageCommandError
				if !errors.As(err, &commandErr) || commandErr.ExitCode() != 100 {
					t.Fatalf("lost exit code: %v", err)
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
				steps = approvedRemovalSteps(inventory, "libfoo:arm64\n", tc.output, tc.output, "libfoo:amd64")
				// This test validates planning only.
				steps = steps[:len(steps)-1]
			} else {
				steps = removalStateSteps(inventory, "libfoo:arm64\n")
				steps = append(steps, removalSimulation(false, tc.output, "libfoo:amd64"))
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
		name, inventory, automatic string
		failCommand                int
	}{
		{name: "dpkg failure", failCommand: 1},
		{name: "bad inventory", inventory: "nginx\tinstalled\n"},
		{name: "mark failure", inventory: packageInstalled("nginx", "1", "amd64"), failCommand: 2},
		{name: "config failure", inventory: packageInstalled("nginx", "1", "amd64"), failCommand: 3},
		{name: "bad marks", inventory: packageInstalled("nginx", "1", "amd64"), automatic: "nginx --invalid\n"},
		{name: "ambiguous marks", inventory: packageInstalled("nginx:amd64", "1", "amd64") + packageInstalled("nginx:arm64", "1", "arm64"), automatic: "nginx\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := removalStateSteps(tc.inventory, tc.automatic)
			if tc.failCommand != 0 {
				steps = steps[:tc.failCommand]
				steps[len(steps)-1].err = packageExit(2)
			} else if tc.name == "bad inventory" {
				steps = steps[:1]
			} else {
				steps = steps[:2]
			}
			if err := packageManagerFixture(t, steps...).validateRemoval(context.Background(), []string{"nginx:amd64"}, nil); err == nil {
				t.Fatal("accepted unreliable state")
			}
		})
	}
}

func TestRemovalDetectsStateAndPlanChanges(t *testing.T) {
	inventory := packageInstalled("nginx", "1", "amd64") + packageInstalled("orphan", "1", "amd64")
	for _, change := range []string{"packages", "marks", "config", "base plan", "cleanup plan"} {
		t.Run(change, func(t *testing.T) {
			steps := approvedRemovalSteps(inventory, "orphan\n", removalOutput("nginx"), removalOutput("nginx", "orphan"), "nginx:amd64")
			steps = steps[:len(steps)-1]
			switch change {
			case "packages":
				steps[7].stdout = strings.ReplaceAll(inventory, "\t1\t", "\t2\t")
			case "marks":
				steps[8].stdout = ""
			case "config":
				steps[9].stdout += "APT::Get::Purge \"true\";\n"
			case "base plan":
				steps[5].stdout = removalOutput("nginx", "orphan")
				steps = steps[:6]
			case "cleanup plan":
				steps[6].stdout = removalOutput("nginx")
				steps = steps[:7]
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

func TestRemovalArgumentsNeverPurge(t *testing.T) {
	for _, simulate := range []bool{false, true} {
		for _, cleanup := range []bool{false, true} {
			args := removalArguments(simulate, cleanup, []string{"nginx:amd64"})
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "-o APT::Get::Purge=false") ||
				!strings.Contains(joined, "-o APT::Get::AutomaticRemove=false") ||
				!strings.HasSuffix(joined, "remove -- nginx:amd64") {
				t.Fatalf("unsafe removal arguments: %q", args)
			}
			if strings.Contains(joined, "--purge") || strings.Contains(joined, "--no-remove") ||
				strings.Contains(joined, "autoremove") || strings.Contains(joined, "allow-remove") {
				t.Fatalf("unexpected removal override: %q", args)
			}
			if strings.Contains(joined, "--auto-remove") != cleanup ||
				strings.Contains(joined, "--simulate") != simulate {
				t.Fatalf("incorrect planning/cleanup mode: %q", args)
			}
		}
	}
}

func TestRemovalCleanupSimulationFailure(t *testing.T) {
	for _, output := range []string{"", "Inst other (1 Debian [amd64])\n"} {
		steps := removalStateSteps(packageInstalled("nginx", "1", "amd64"), "")
		steps = append(steps, removalSimulation(false, removalOutput("nginx"), "nginx:amd64"))
		step := removalSimulation(true, output, "nginx:amd64")
		if output == "" {
			step.err = packageExit(100)
		}
		steps = append(steps, step)
		if err := packageManagerFixture(t, steps...).validateRemoval(context.Background(), []string{"nginx:amd64"}, nil); err == nil {
			t.Fatal("accepted failed cleanup simulation")
		}
	}
}

func TestRemovalDetectsChangeBeforePlanning(t *testing.T) {
	steps := removalStateSteps(packageInstalled("nginx", "2", "amd64"), "")
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
			steps = append(steps, approvedRemovalSteps(row, "", removalOutput("nginx"), removalOutput("nginx"), "nginx:amd64")...)
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
