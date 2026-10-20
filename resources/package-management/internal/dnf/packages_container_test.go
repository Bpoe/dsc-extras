package dnf

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The host only compiles/copies binaries. Every native package mutation and
// fixture build runs in an unmounted, disposable container.
func TestDNFContainer(t *testing.T) {
	if os.Getenv("DSCPM_DNF_CONTAINER") != "1" {
		t.Skip("set DSCPM_DNF_CONTAINER=1 to run disposable Rocky Linux integration tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	image := os.Getenv("DSCPM_DNF_IMAGE")
	if image == "" {
		image = "quay.io/rockylinux/rockylinux:9"
	}
	tool, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal("DSCPM_DNF_CONTAINER=1 requires Docker")
	}
	run := func(program string, args ...string) []byte {
		t.Helper()
		output, err := exec.CommandContext(ctx, program, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %q: %v\n%s", program, args, err, output)
		}
		return output
	}
	run(tool, "info")
	dir := t.TempDir()
	for _, build := range []struct{ output, target string }{{"dnf.test", "."}, {"dscpm", "../../cmd/dscpm"}} {
		args := []string{"build", "-o", filepath.Join(dir, build.output), build.target}
		if build.output == "dnf.test" {
			args = []string{"test", "-c", "-o", filepath.Join(dir, build.output), build.target}
		}
		command := exec.CommandContext(ctx, "go", args...)
		command.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build container binary: %v\n%s", err, output)
		}
	}
	name := fmt.Sprintf("dscpm-dnf-%d-%d", os.Getpid(), time.Now().UnixNano())
	run(tool, "run", "--detach", "--name", name, image, "sleep", "infinity")
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if output, err := exec.CommandContext(cleanupCtx, tool, "rm", "--force", name).CombinedOutput(); err != nil {
			t.Errorf("remove disposable DNF container: %v\n%s", err, output)
		}
	})
	for _, file := range []string{"dnf.test", "dscpm"} {
		run(tool, "cp", filepath.Join(dir, file), name+":/usr/local/bin/"+file)
	}
	run(tool, "cp", filepath.Join("..", "..", "manifests", "packages.dsc.resource.json"), name+":/usr/local/bin/packages.dsc.resource.json")
	run(tool, "exec", name, "chmod", "755", "/usr/local/bin/dnf.test", "/usr/local/bin/dscpm")
	run(tool, "exec", name, "dnf", "--assumeyes", "install", "rpm-build")
	output := run(tool, "exec", "--env", "DSCPM_DNF_NATIVE=1", name,
		"/usr/local/bin/dnf.test", "-test.v", "-test.timeout=10m", "-test.run=^TestDNFNative")
	t.Logf("%s:\n%s", image, output)
}

func nativeRun(t *testing.T, program string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("%s %q: %v\n%s\n%s", program, args, err, &stdout, &stderr)
	}
	return stdout.Bytes()
}

func requireNative(t *testing.T) {
	t.Helper()
	if os.Getenv("DSCPM_DNF_NATIVE") != "1" {
		t.Skip("only run inside the disposable DNF test container")
	}
	if _, err := os.Stat("/.dockerenv"); err != nil {
		t.Fatal("native mutating tests require a Docker container")
	}
	t.Logf("platform: %s", nativeRun(t, "cat", "/etc/os-release"))
	t.Logf("DNF: %s; RPM: %s", nativeRun(t, "dnf", "--version"), nativeRun(t, "rpm", "--version"))
}

func buildRPM(t *testing.T, name, version, extra string) string {
	t.Helper()
	root := t.TempDir()
	p := fixture(name, version, "noarch")
	spec := fmt.Sprintf(`Name: %s
Epoch: %s
Version: %s
Release: %s
Summary: Disposable dscpm integration fixture
License: MIT
BuildArch: noarch
%s
%%description
Disposable integration fixture.
%%install
mkdir -p %%{buildroot}/usr/share/dscpm-fixtures
echo fixture > %%{buildroot}/usr/share/dscpm-fixtures/%s
%%files
/usr/share/dscpm-fixtures/%s
`, p.name, p.epoch, p.version, p.release, extra, p.name, p.name)
	path := filepath.Join(root, "fixture.spec")
	if err := os.WriteFile(path, []byte(spec), 0600); err != nil {
		t.Fatal(err)
	}
	nativeRun(t, "rpmbuild", "-bb", "--define", "_topdir "+root, path)
	matches, err := filepath.Glob(filepath.Join(root, "RPMS", "noarch", "*.rpm"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("RPM fixture output %v: %v", matches, err)
	}
	// Deliberately discard the original filename: metadata must be authoritative.
	target := filepath.Join(root, "arbitrary-artifact.rpm")
	if err := os.Rename(matches[0], target); err != nil {
		t.Fatal(err)
	}
	return target
}

func history(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(string(nativeRun(t, "dnf", "--quiet", "history", "list")))
}
