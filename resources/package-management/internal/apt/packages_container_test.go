package apt

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

type containerPackageExecutor struct {
	tool, container string
	mutations       int
}

func (e *containerPackageExecutor) Execute(ctx context.Context, program string, args, env []string) ([]byte, []byte, error) {
	if program == "apt-get" && !slices.Contains(args, "--simulate") {
		e.mutations++
	}
	commandArgs := []string{"exec"}
	for _, entry := range env {
		commandArgs = append(commandArgs, "--env", entry)
	}
	commandArgs = append(commandArgs, e.container, program)
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(ctx, e.tool, commandArgs...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// All package writes, including fixture creation, run inside an unmounted,
// disposable Debian container. This test never uses the native APT executor.
func TestPackagesContainerRemoval(t *testing.T) {
	if os.Getenv("DSCPM_APT_CONTAINER") != "1" {
		t.Skip("set DSCPM_APT_CONTAINER=1 to run disposable-container APT tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	var tool string
	for _, candidate := range []string{"docker", "podman"} {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, 15*time.Second)
		err = exec.CommandContext(probeCtx, path, "info").Run()
		probeCancel()
		if err == nil {
			tool = path
			break
		}
	}
	if tool == "" {
		t.Fatal("DSCPM_APT_CONTAINER=1 requires an available Docker or Podman container runtime")
	}
	name := fmt.Sprintf("dscpm-apt-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if output, err := exec.CommandContext(cleanupCtx, tool, "rm", "--force", name).CombinedOutput(); err != nil {
			t.Errorf("remove disposable container: %v\n%s", err, output)
		}
	})
	output, err := exec.CommandContext(ctx, tool, "run", "--rm", "--detach",
		"--name", name, "--network", "none", "debian:bookworm-slim", "sleep", "infinity").CombinedOutput()
	if err != nil {
		t.Fatalf("start disposable Debian container: %v\n%s", err, output)
	}
	script := `
set -eu
export LC_ALL=C DEBIAN_FRONTEND=noninteractive
mkdir -p /root/dscpm-fixtures
cd /root/dscpm-fixtures
arch=$(dpkg --print-architecture)
make_deb() {
    package=$1
    depends=$2
    mkdir -p "$package/DEBIAN" "$package/etc"
    printf 'Package: %s\nVersion: 1.0\nArchitecture: %s\nMaintainer: Test <test@example.invalid>\nDescription: Disposable DSC APT test\n' "$package" "$arch" > "$package/DEBIAN/control"
    if [ -n "$depends" ]; then
        printf 'Depends: %s\n' "$depends" >> "$package/DEBIAN/control"
    fi
    printf 'retained configuration\n' > "$package/etc/$package.conf"
    printf '/etc/%s.conf\n' "$package" > "$package/DEBIAN/conffiles"
    dpkg-deb --build "$package" "$package.deb"
}
make_deb dscpm-orphan ''
make_deb dscpm-old-orphan ''
make_deb dscpm-shared ''
make_deb dscpm-manual ''
make_deb dscpm-app 'dscpm-orphan, dscpm-shared, dscpm-manual'
make_deb dscpm-consumer 'dscpm-shared'
dpkg -i ./dscpm-orphan.deb ./dscpm-old-orphan.deb ./dscpm-shared.deb ./dscpm-manual.deb ./dscpm-app.deb ./dscpm-consumer.deb
# Isolate the fixture graph from the image's pre-existing automatic marks.
dpkg-query --show --showformat='${binary:Package}\n' | xargs apt-mark manual
apt-mark auto dscpm-orphan dscpm-old-orphan dscpm-shared
printf 'APT::Get::AutomaticRemove "true";\n' > /etc/apt/apt.conf.d/99dscpm-test
`
	command := exec.CommandContext(ctx, tool, "exec", "-i", name, "sh", "-s")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build and install container fixtures: %v\n%s", err, output)
	}
	executor := &containerPackageExecutor{tool: tool, container: name}
	manager := NewPackageManager(executor)
	run := func(t *testing.T, program string, args ...string) string {
		t.Helper()
		output, err := manager.run(ctx, program, args...)
		if err != nil {
			t.Fatal(err)
		}
		return string(output)
	}
	absent := func(name string) Packages {
		return Packages{Name: PackageNames{Values: []string{name}, Scalar: true}, Ensure: "Absent"}
	}
	checkInstalled := func(t *testing.T, name string, want bool) {
		t.Helper()
		actual, err := manager.Get(ctx, absent(name))
		if err != nil {
			t.Fatal(err)
		}
		if len(actual.Packages) != 1 || actual.Packages[0].Installed != want {
			t.Fatalf("%s installed = %+v, want %t", name, actual.Packages, want)
		}
	}
	for _, mark := range []string{"manual", "auto"} {
		t.Run("reverse-dependent-"+mark, func(t *testing.T) {
			run(t, "apt-mark", mark, "dscpm-app")
			before := run(t, "dpkg-query", "--show", packageQueryFormat)
			beforeMarks := run(t, "apt-mark", "showauto")
			_, err := manager.Set(ctx, absent("dscpm-orphan"))
			if err == nil || !strings.Contains(err.Error(), "not explicitly requested") ||
				!strings.Contains(err.Error(), "dscpm-app") {
				t.Fatalf("expected reverse-dependent removal rejection, got %v", err)
			}
			if executor.mutations != 0 {
				t.Fatal("blocked removal executed an APT mutation")
			}
			if after := run(t, "dpkg-query", "--show", packageQueryFormat); after != before {
				t.Fatal("blocked removal changed dpkg inventory")
			}
			if after := run(t, "apt-mark", "showauto"); after != beforeMarks {
				t.Fatal("blocked removal changed automatic marks")
			}
		})
	}
	run(t, "apt-mark", "manual", "dscpm-app")
	t.Run("explicit-removal-retains-orphans", func(t *testing.T) {
		actual, err := manager.Set(ctx, absent("dscpm-app"))
		if err != nil {
			t.Fatal(err)
		}
		if actual.State != "Absent" || executor.mutations != 1 {
			t.Fatalf("removal result = %+v, mutations = %d", actual, executor.mutations)
		}
		checkInstalled(t, "dscpm-app", false)
		status := run(t, "dpkg-query", "--show", "--showformat=${Status}", "dscpm-app")
		if status != "deinstall ok config-files" {
			t.Fatalf("dscpm-app status = %q, expected retained conffiles", status)
		}
		if content := run(t, "cat", "/etc/dscpm-app.conf"); content != "retained configuration\n" {
			t.Fatalf("dscpm-app conffile changed: %q", content)
		}
		for _, name := range []string{"dscpm-orphan", "dscpm-old-orphan", "dscpm-shared", "dscpm-manual", "dscpm-consumer"} {
			checkInstalled(t, name, true)
		}
	})
	t.Run("idempotent-absence", func(t *testing.T) {
		before := executor.mutations
		actual, err := manager.Set(ctx, absent("dscpm-app"))
		if err != nil || actual.State != "Absent" {
			t.Fatalf("second removal: %+v, error %v", actual, err)
		}
		if executor.mutations != before {
			t.Fatal("already absent package triggered another APT mutation")
		}
	})
}
