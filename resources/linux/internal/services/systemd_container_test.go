package services

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This opt-in test boots its own systemd; it never mounts host unit directories
// or connects to the host's system/user bus.
func TestSystemdContainer(t *testing.T) {
	if os.Getenv("DSCLINUX_SYSTEMD_CONTAINER") != "1" {
		t.Skip("set DSCLINUX_SYSTEMD_CONTAINER=1 to test real systemd in a disposable privileged container")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal("DSCLINUX_SYSTEMD_CONTAINER=1 requires Docker")
	}
	command := func(t *testing.T, input string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, docker, args...)
		cmd.Stdin = strings.NewReader(input)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, output)
		}
		return string(output)
	}
	command(t, "", "info")
	name := fmt.Sprintf("dsclinux-systemd-%d-%d", os.Getpid(), time.Now().UnixNano())
	image := name + ":test"
	command(t, `FROM debian:bookworm-slim
RUN apt-get update -qq && apt-get install -y --no-install-recommends systemd systemd-sysv dbus dbus-user-session >/dev/null && apt-get clean
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
`, "build", "--quiet", "--tag", image, "-")
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if output, err := exec.CommandContext(cleanupCtx, docker, "image", "rm", image).CombinedOutput(); err != nil {
			t.Errorf("remove test image: %v\n%s", err, output)
		}
	})
	command(t, "", "run", "--detach", "--name", name, "--privileged", "--cgroupns", "private",
		"--tmpfs", "/run", "--tmpfs", "/run/lock", "--network", "none", image)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if output, err := exec.CommandContext(cleanupCtx, docker, "rm", "--force", name).CombinedOutput(); err != nil {
			t.Errorf("remove test container: %v\n%s", err, output)
		}
	})
	ready := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if exec.CommandContext(ctx, docker, "exec", name, "systemctl", "show", "--property=Version").Run() == nil {
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		t.Fatal("isolated systemd did not become responsive; Docker must support privileged containers with a private cgroup namespace")
	}
	binary := filepath.Join(t.TempDir(), "dsclinux")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, filepath.Join("..", "..", "cmd", "dsclinux"))
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test executable: %v\n%s", err, output)
	}
	command(t, "", "cp", binary, name+":/usr/local/bin/dsclinux")
	command(t, "", "exec", name, "chmod", "755", "/usr/local/bin/dsclinux")
	command(t, `set -eu
mkdir -p /usr/lib/systemd/system /usr/lib/systemd/user
cat > /usr/lib/systemd/system/dsclinux-test.service <<'EOF'
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/true
[Install]
WantedBy=default.target
EOF
cp /usr/lib/systemd/system/dsclinux-test.service /usr/lib/systemd/user/dsclinux-test.service
cp /usr/lib/systemd/system/dsclinux-test.service /usr/lib/systemd/system/dsclinux-test@.service
cat > /usr/lib/systemd/system/dsclinux-test.timer <<'EOF'
[Timer]
OnActiveSec=1h
Unit=dsclinux-test.service
[Install]
WantedBy=timers.target
EOF
cat > /usr/lib/systemd/system/dsclinux-test.socket <<'EOF'
[Socket]
ListenStream=/run/dsclinux-test.sock
Service=dsclinux-test.service
[Install]
WantedBy=sockets.target
EOF
cat > /usr/lib/systemd/system/dsclinux-static.service <<'EOF'
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/true
EOF
cat > /usr/lib/systemd/system/dsclinux-failed.service <<'EOF'
[Service]
Type=oneshot
ExecStart=/bin/false
EOF
systemctl daemon-reload
systemctl start user@0.service
`, "exec", "-i", name, "sh", "-s")

	invoke := func(t *testing.T, operation, input string) Unit {
		t.Helper()
		output := command(t, input, "exec", "-i", "--env", "XDG_RUNTIME_DIR=/run/user/0",
			"--env", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/0/bus", name, "dsclinux", "services", operation)
		var actual Unit
		if err := json.Unmarshal([]byte(output), &actual); err != nil {
			t.Fatalf("invalid %s output: %s (%v)", operation, output, err)
		}
		return actual
	}
	for _, scope := range []string{"system", "user"} {
		t.Run(scope, func(t *testing.T) {
			input := fmt.Sprintf(`{"name":"dsclinux-test","scope":%q,"state":"active","enabled":true,"masked":true}`, scope)
			actual := invoke(t, "set", input)
			if actual.ActiveState != "active" || !*actual.Enabled || !*actual.Masked || actual.UnitFileState != "masked" {
				t.Fatalf("real systemd did not retain independent states: %+v", actual)
			}
			invoke(t, "set", input)
			invoke(t, "get", fmt.Sprintf(`{"name":"dsclinux-test","scope":%q}`, scope))
			invoke(t, "set", fmt.Sprintf(`{"name":"dsclinux-test","scope":%q,"state":"inactive"}`, scope))
			actual = invoke(t, "set", fmt.Sprintf(`{"name":"dsclinux-test","scope":%q,"enabled":false}`, scope))
			if *actual.Enabled || !*actual.Masked || actual.ActiveState != "inactive" {
				t.Fatalf("disable changed unmanaged activity/mask: %+v", actual)
			}
			actual = invoke(t, "set", fmt.Sprintf(`{"name":"dsclinux-test","scope":%q,"enabled":true}`, scope))
			if !*actual.Enabled || !*actual.Masked || actual.ActiveState != "inactive" {
				t.Fatalf("enable did not preserve unmanaged mask: %+v", actual)
			}
			invoke(t, "set", fmt.Sprintf(`{"name":"dsclinux-test","scope":%q,"masked":false}`, scope))
		})
	}
	for _, unit := range []string{"dsclinux-test.timer", "dsclinux-test.socket"} {
		actual := invoke(t, "set", fmt.Sprintf(`{"name":%q,"state":"active","enabled":true}`, unit))
		if actual.ActiveState != "active" || !*actual.Enabled {
			t.Fatalf("timer/socket state: %+v", actual)
		}
		invoke(t, "set", fmt.Sprintf(`{"name":%q,"state":"inactive","enabled":false}`, unit))
	}
	t.Run("relative enablement under mask", func(t *testing.T) {
		command(t, `set -eu
systemctl disable dsclinux-test.service
mkdir -p /etc/systemd/system/default.target.wants
ln -s ../dsclinux-test.service /etc/systemd/system/default.target.wants/dsclinux-test.service
systemctl mask dsclinux-test.service
`, "exec", "-i", name, "sh", "-s")
		actual := invoke(t, "get", `{"name":"dsclinux-test"}`)
		if !*actual.Enabled || !*actual.Masked {
			t.Fatalf("relative enablement lost behind mask: %+v", actual)
		}
		invoke(t, "set", `{"name":"dsclinux-test","masked":false}`)
	})
	t.Run("template masks", func(t *testing.T) {
		command(t, "", "exec", name, "systemctl", "mask", "dsclinux-test@.service")
		actual := invoke(t, "get", `{"name":"dsclinux-test@one.service"}`)
		if !*actual.Masked {
			t.Fatalf("template mask was not inherited: %+v", actual)
		}
		cmd := exec.CommandContext(ctx, docker, "exec", "-i", name, "dsclinux", "services", "set")
		cmd.Stdin = strings.NewReader(`{"name":"dsclinux-test@one.service","masked":false}`)
		if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "inherits its mask") {
			t.Fatalf("inherited template mask must be explicitly removed: %v\n%s", err, output)
		}
		command(t, "", "exec", name, "systemctl", "unmask", "dsclinux-test@.service")
	})
	t.Run("new definition", func(t *testing.T) {
		invoke(t, "get", `{"name":"dsclinux-new"}`)
		command(t, "", "exec", name, "cp", "/usr/lib/systemd/system/dsclinux-static.service",
			"/usr/lib/systemd/system/dsclinux-new.service")
		actual := invoke(t, "set", `{"name":"dsclinux-new","state":"active"}`)
		if actual.ActiveState != "active" {
			t.Fatalf("new unit did not become active: %+v", actual)
		}
	})
	t.Run("failed missing static", func(t *testing.T) {
		if err := exec.CommandContext(ctx, docker, "exec", name, "systemctl", "start", "dsclinux-failed.service").Run(); err == nil {
			t.Fatal("failure fixture unexpectedly started")
		}
		actual := invoke(t, "get", `{"name":"dsclinux-failed"}`)
		if actual.ActiveState != "failed" || actual.State != nil {
			t.Fatalf("failed state normalized: %+v", actual)
		}
		invoke(t, "set", `{"name":"dsclinux-failed","state":"inactive"}`)
		actual = invoke(t, "get", `{"name":"dsclinux-nonexistent"}`)
		if actual.LoadState != "not-found" {
			t.Fatalf("missing unit: %+v", actual)
		}
		actual = invoke(t, "get", `{"name":"dsclinux-static"}`)
		if actual.UnitFileState != "static" || *actual.Enabled {
			t.Fatalf("static unit: %+v", actual)
		}
		cmd := exec.CommandContext(ctx, docker, "exec", "-i", name, "dsclinux", "services", "set")
		cmd.Stdin = strings.NewReader(`{"name":"dsclinux-static","enabled":true}`)
		if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "did not converge") {
			t.Fatalf("static enablement must fail: %v\n%s", err, output)
		}
	})
}
