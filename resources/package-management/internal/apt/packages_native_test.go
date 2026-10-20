package apt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPackagesNativePipeHelper(t *testing.T) {
	switch os.Getenv("DSCPM_PACKAGE_PIPE_HELPER") {
	case "parent":
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.Command(executable, "-test.run=^TestPackagesNativePipeHelper$")
		child.Env = append(os.Environ(), "DSCPM_PACKAGE_PIPE_HELPER=child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		fmt.Println(child.Process.Pid)
		time.Sleep(30 * time.Second)
	case "child":
		time.Sleep(30 * time.Second)
	}
}

func TestPackagesNativeCancellationBoundsInheritedPipes(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSCPM_PACKAGE_PIPE_HELPER", "parent")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = NewPackageManager(nil).run(ctx, executable, "-test.run=^TestPackagesNativePipeHelper$")
	elapsed := time.Since(start)
	var commandError *PackageCommandError
	if !errors.As(err, &commandError) {
		t.Fatalf("expected command error with child PID, got %v", err)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(commandError.Stdout))
	if parseErr != nil {
		t.Fatalf("helper did not report child PID: %q: %v", commandError.Stdout, parseErr)
	}
	child, findErr := os.FindProcess(pid)
	if findErr != nil {
		t.Fatal(findErr)
	}
	defer func() { _ = child.Kill() }()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("native deadline lost: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("inherited pipes exceeded finite drain timeout: %s", elapsed)
	}
}

// Only archive building and Get/Test are used here; no package transaction runs.
func TestPackagesNativeReadOnly(t *testing.T) {
	for _, tool := range []string{"dpkg", "dpkg-query", "dpkg-deb"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("optional native integration tool %s unavailable: %v", tool, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manager := NewPackageManager(nil)
	bash, err := manager.Get(ctx, packageDesired("bash"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bash.Packages) != 1 || !bash.Packages[0].Installed {
		t.Skip("optional native integration requires installed bash")
	}
	installed := bash.Packages[0]
	artifact := packageDeb(t, "artifact-name-does-not-match-package.deb")
	directory := filepath.Dir(artifact)
	buildArchive := func(name, version, architecture string) {
		t.Helper()
		root := filepath.Join(directory, "archive")
		controlDirectory := filepath.Join(root, "DEBIAN")
		if err := os.MkdirAll(controlDirectory, 0755); err != nil {
			t.Fatal(err)
		}
		control := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: Test <test@example.invalid>\nDescription: Native read-only package resource fixture\n",
			name, version, architecture)
		if err := os.WriteFile(filepath.Join(controlDirectory, "control"), []byte(control), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.run(ctx, "dpkg-deb", "--build", "--root-owner-group", root, artifact); err != nil {
			t.Fatal(err)
		}
	}
	buildArchive("bash", installed.Version, installed.Architecture)
	actual, differences, err := manager.Test(ctx, packageDesired(artifact))
	if err != nil {
		t.Fatal(err)
	}
	if actual.State != "Present" || len(differences) != 0 || differences == nil ||
		actual.Packages[0].Name != "bash" || actual.Packages[0].RequestedName != artifact ||
		actual.Packages[0].Version != installed.Version || actual.Packages[0].Architecture != installed.Architecture {
		t.Fatalf("archive metadata did not resolve to installed bash: %+v, differences %v", actual, differences)
	}

	missingName := fmt.Sprintf("dsc-extras-uninstalled-test-%d", time.Now().UnixNano())
	buildArchive(missingName, "1", installed.Architecture)
	actual, differences, err = manager.Test(ctx, packageDesired(artifact))
	if err != nil {
		t.Fatal(err)
	}
	if actual.State != "Absent" || len(differences) != 1 || differences[0] != "ensure" ||
		actual.Packages[0].Name != missingName || actual.Packages[0].Installed ||
		actual.Packages[0].Status != "" || actual.Packages[0].Version != "" {
		t.Fatalf("native missing-package diagnostic was not recognized: %+v, differences %v", actual, differences)
	}
}
