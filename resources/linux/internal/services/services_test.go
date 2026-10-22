//go:build linux

package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func ptr[T any](value T) *T { return &value }

type fakeSystemctl struct {
	t                         *testing.T
	manager                   *Manager
	active, load, sub, file   string
	names                     string
	fragment                  string
	calls                     []string
	fail                      map[string]string
	needReload, appearsOnLoad bool
	static, ignoreMutation    bool
	runtimeMask, masked       bool
	pruneOnUnmask             bool
	defaultInstance           string
}

func fixture(t *testing.T) (*Manager, *fakeSystemctl) {
	t.Helper()
	executor := &fakeSystemctl{
		t: t, active: "inactive", load: "loaded", sub: "dead", fail: make(map[string]string),
	}
	manager := NewManager(executor, t.TempDir())
	executor.manager = manager
	return manager, executor
}

func symlink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeSystemctl) path(scope, name string) string {
	dirs, err := f.manager.configDirs(scope)
	if err != nil {
		f.t.Fatal(err)
	}
	return filepath.Join(dirs[0], name)
}

func (f *fakeSystemctl) enable(scope, name string) {
	instance := name
	if f.defaultInstance != "" {
		instance = strings.Replace(name, "@.", "@"+f.defaultInstance+".", 1)
	}
	symlink(f.t, f.path(scope, filepath.Join("default.target.wants", instance)), filepath.Join("..", name))
}

func (f *fakeSystemctl) mask(scope, name string) {
	symlink(f.t, f.path(scope, name), "/dev/null")
	f.masked = true
}

func (f *fakeSystemctl) Execute(ctx context.Context, args []string) ([]byte, []byte, error) {
	f.t.Helper()
	if len(args) < 4 || (args[0] != "--system" && args[0] != "--user") ||
		args[1] != "--no-pager" || args[2] != "--no-ask-password" {
		f.t.Fatalf("unsafe systemctl arguments: %v", args)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	scope, operation := strings.TrimPrefix(args[0], "--"), args[3]
	f.calls = append(f.calls, operation)
	if message := f.fail[operation]; message != "" {
		return nil, []byte(message), errors.New("exit status 1")
	}
	if operation == "daemon-reload" {
		f.needReload = false
		if f.appearsOnLoad {
			f.load = "loaded"
		}
		return nil, nil, nil
	}
	name := args[len(args)-1]
	if args[len(args)-2] != "--" {
		f.t.Fatalf("missing option terminator: %v", args)
	}
	if operation == "show" || operation == "list-unit-files" {
		enabled, _, err := f.manager.persistentEnablement(scope, []string{name})
		if err != nil {
			return nil, nil, err
		}
		file := "disabled"
		if enabled {
			file = "enabled"
		}
		if f.static {
			file = "static"
		}
		if f.runtimeMask {
			file = "masked-runtime"
		}
		if f.masked {
			file = "masked"
		}
		if f.file != "" {
			file = f.file
		}
		if operation == "list-unit-files" {
			if f.load == "not-found" {
				return nil, nil, nil
			}
			return []byte(fmt.Sprintf("%s %s -\n", name, file)), nil, nil
		}
		load := f.load
		if f.masked {
			load = "masked"
		}
		names := name
		if f.names != "" {
			names = f.names
		}
		reload := "no"
		if f.needReload {
			reload = "yes"
		}
		return []byte(fmt.Sprintf("Id=%s\nNames=%s\nActiveState=%s\nSubState=%s\nLoadState=%s\nUnitFileState=%s\nNeedDaemonReload=%s\nFragmentPath=%s\n",
			name, names, f.active, f.sub, load, file, reload, f.fragment)), nil, nil
	}
	if f.ignoreMutation {
		return nil, nil, nil
	}
	switch operation {
	case "start":
		f.active, f.sub = "active", "exited"
	case "stop":
		if f.active == "failed" {
			break
		}
		f.active, f.sub = "inactive", "dead"
	case "reset-failed":
		f.active, f.sub = "inactive", "dead"
	case "enable":
		if !f.static {
			f.enable(scope, name)
		}
	case "disable":
		_, links, err := f.manager.persistentEnablement(scope, []string{name})
		if err != nil {
			f.t.Fatal(err)
		}
		for _, link := range links {
			if err := os.Remove(link.path); err != nil {
				f.t.Fatal(err)
			}
		}
	case "mask":
		if _, err := os.Lstat(f.path(scope, name)); errors.Is(err, os.ErrNotExist) {
			f.mask(scope, name)
		} else if err != nil {
			f.t.Fatal(err)
		}
	case "unmask":
		if err := os.Remove(f.path(scope, name)); err != nil {
			f.t.Fatal(err)
		}
		f.masked = false
		if f.pruneOnUnmask {
			_, links, err := f.manager.persistentEnablement(scope, []string{name})
			if err != nil {
				f.t.Fatal(err)
			}
			for _, link := range links {
				if err := os.Remove(link.path); err != nil {
					f.t.Fatal(err)
				}
			}
		}
	default:
		f.t.Fatalf("unexpected operation %q", operation)
	}
	return nil, nil, nil
}

func mutations(calls []string) []string {
	result := []string{}
	for _, call := range calls {
		if call != "show" && call != "list-unit-files" {
			result = append(result, call)
		}
	}
	return result
}

func TestReconcile(t *testing.T) {
	for _, test := range []struct {
		name                  string
		active                string
		enabled, masked       bool
		desired               Unit
		want                  []string
		wantEnabled, wantMask bool
	}{
		{name: "unmanaged", active: "active", enabled: true, masked: true, wantEnabled: true, wantMask: true},
		{name: "already enabled and masked", enabled: true, masked: true, desired: Unit{Enabled: ptr(true), Masked: ptr(true)}, wantEnabled: true, wantMask: true},
		{name: "start", desired: Unit{State: ptr("active")}, want: []string{"start"}},
		{name: "start failed service", active: "failed", desired: Unit{State: ptr("active")}, want: []string{"start"}},
		{name: "stop failed service", active: "failed", desired: Unit{State: ptr("inactive")}, want: []string{"stop", "reset-failed"}},
		{name: "stop", active: "active", enabled: true, desired: Unit{State: ptr("inactive")}, want: []string{"stop"}, wantEnabled: true},
		{name: "enable does not start", desired: Unit{Enabled: ptr(true)}, want: []string{"enable"}, wantEnabled: true},
		{name: "disable does not stop", active: "active", enabled: true, desired: Unit{Enabled: ptr(false)}, want: []string{"disable"}},
		{name: "mask preserves enablement and activity", active: "active", enabled: true, desired: Unit{Masked: ptr(true)}, want: []string{"mask"}, wantEnabled: true, wantMask: true},
		{name: "unmask preserves enablement", enabled: true, masked: true, desired: Unit{Masked: ptr(false)}, want: []string{"unmask"}, wantEnabled: true},
		{name: "enable then mask", desired: Unit{Enabled: ptr(true), Masked: ptr(true)}, want: []string{"enable", "mask"}, wantEnabled: true, wantMask: true},
		{name: "enable behind managed mask", masked: true, desired: Unit{Enabled: ptr(true), Masked: ptr(true)}, want: []string{"unmask", "enable", "mask"}, wantEnabled: true, wantMask: true},
		{name: "enable preserves unmanaged mask", masked: true, desired: Unit{Enabled: ptr(true)}, want: []string{"unmask", "enable", "mask"}, wantEnabled: true, wantMask: true},
		{name: "disable preserves unmanaged mask", enabled: true, masked: true, desired: Unit{Enabled: ptr(false)}, want: []string{"unmask", "disable", "mask"}, wantMask: true},
		{name: "start preserves unmanaged mask", masked: true, enabled: true, desired: Unit{State: ptr("active")}, want: []string{"unmask", "start", "mask"}, wantEnabled: true, wantMask: true},
		{name: "start before requested mask", desired: Unit{State: ptr("active"), Masked: ptr(true)}, want: []string{"start", "mask"}, wantMask: true},
		{name: "stop masked active unit", active: "active", masked: true, desired: Unit{State: ptr("inactive")}, want: []string{"stop"}, wantMask: true},
		{name: "all properties", masked: true, desired: Unit{State: ptr("active"), Enabled: ptr(true), Masked: ptr(false)}, want: []string{"unmask", "enable", "start"}, wantEnabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, executor := fixture(t)
			desired := test.desired
			desired.Name = "example"
			if test.active != "" {
				executor.active = test.active
			}
			if test.enabled {
				executor.enable("system", "example.service")
			}
			if test.masked {
				executor.mask("system", "example.service")
			}
			before, diff, err := manager.Test(t.Context(), desired)
			if err != nil || len(mutations(executor.calls)) != 0 {
				t.Fatalf("Test must be read-only: %+v %v %v", before, executor.calls, err)
			}
			if (len(diff) == 0) != (len(test.want) == 0) {
				t.Fatalf("Test differences %v, planned mutations %v", diff, test.want)
			}
			executor.calls = nil
			actual, err := manager.Set(t.Context(), desired)
			if err != nil || !slices.Equal(mutations(executor.calls), test.want) {
				t.Fatalf("Set = %+v, %v; mutations %v, want %v", actual, err, mutations(executor.calls), test.want)
			}
			if *actual.Enabled != test.wantEnabled || *actual.Masked != test.wantMask {
				t.Fatalf("independent persistent states: %+v", actual)
			}
			if desired.State == nil && actual.ActiveState != before.ActiveState {
				t.Fatal("unmanaged runtime state changed")
			}
			executor.calls = nil
			repeated, err := manager.Set(t.Context(), desired)
			if err != nil || !reflect.DeepEqual(actual, repeated) || len(mutations(executor.calls)) != 0 || len(executor.calls) != 1 {
				t.Fatalf("second Set must only inspect: %+v %v %v", repeated, executor.calls, err)
			}
		})
	}
}

func TestTemplateReconcile(t *testing.T) {
	for _, scope := range []string{"system", "user"} {
		for _, masked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/masked=%t", scope, masked), func(t *testing.T) {
				manager, executor := fixture(t)
				executor.defaultInstance = "default"
				executor.pruneOnUnmask = true
				desired := Unit{Name: "worker@.service", Scope: scope, Enabled: ptr(true)}
				if masked {
					executor.mask(scope, desired.Name)
				}
				actual, err := manager.Get(t.Context(), desired)
				if err != nil || *actual.Enabled || actual.State != nil || actual.ActiveState != "" || actual.SubState != "" {
					t.Fatalf("template must report only unit-file state: %+v %v", actual, err)
				}
				executor.calls = nil
				actual, err = manager.Set(t.Context(), desired)
				want := []string{"enable"}
				if masked {
					want = []string{"unmask", "enable", "mask"}
				}
				if err != nil || !*actual.Enabled || *actual.Masked != masked || !slices.Equal(mutations(executor.calls), want) {
					t.Fatalf("template Set: %+v %v; mutations %v, want %v", actual, err, mutations(executor.calls), want)
				}
				executor.calls = nil
				actual, diff, err := manager.Test(t.Context(), desired)
				if err != nil || len(diff) != 0 || !*actual.Enabled || len(mutations(executor.calls)) != 0 {
					t.Fatalf("enabled template Test must be read-only and compliant: %+v %v %v", actual, diff, err)
				}
				executor.calls = nil
				if _, err := manager.Set(t.Context(), desired); err != nil ||
					!slices.Equal(executor.calls, []string{"list-unit-files"}) {
					t.Fatalf("compliant template Set mutated configuration: %v %v", executor.calls, err)
				}
				if masked {
					actual, err = manager.Set(t.Context(), Unit{Name: desired.Name, Scope: scope, Masked: ptr(false)})
					if err != nil || !*actual.Enabled || *actual.Masked {
						t.Fatalf("unmask pruned template's instance enablement: %+v %v", actual, err)
					}
				}
				actual, err = manager.Set(t.Context(), Unit{Name: desired.Name, Scope: scope, Enabled: ptr(false)})
				if err != nil || *actual.Enabled {
					t.Fatalf("template disable did not converge: %+v %v", actual, err)
				}
			})
		}
	}
	t.Run("missing template", func(t *testing.T) {
		manager, executor := fixture(t)
		executor.load = "not-found"
		desired := Unit{Name: "missing@.service", Enabled: ptr(true)}
		actual, err := manager.Get(t.Context(), desired)
		if err != nil || actual.LoadState != "not-found" || *actual.Enabled {
			t.Fatalf("missing template: %+v %v", actual, err)
		}
		if _, err := manager.Set(t.Context(), desired); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("missing template Set claimed success: %v", err)
		}
	})
	t.Run("unit-file inspection failure", func(t *testing.T) {
		manager, executor := fixture(t)
		executor.fail["list-unit-files"] = "Permission denied"
		if _, err := manager.Get(t.Context(), Unit{Name: "worker@.service"}); err == nil || !strings.Contains(err.Error(), "Permission denied") {
			t.Fatalf("template inspection failure hidden: %v", err)
		}
	})
}

func TestRawDiagnosticsAndDifferences(t *testing.T) {
	for _, state := range []string{"failed", "activating", "deactivating", "reloading", "active", "inactive"} {
		t.Run(state, func(t *testing.T) {
			manager, executor := fixture(t)
			executor.active, executor.sub = state, "raw-substate"
			actual, diff, err := manager.Test(t.Context(), Unit{Name: "example", State: ptr("inactive"), Enabled: ptr(true), Masked: ptr(true)})
			want := []string{"state", "enabled", "masked"}
			if state == "inactive" {
				want = want[1:]
			}
			if err != nil || actual.ActiveState != state || actual.SubState != "raw-substate" || !slices.Equal(diff, want) {
				t.Fatalf("raw state lost: %+v %v %v", actual, diff, err)
			}
			if state != "active" && state != "inactive" && actual.State != nil {
				t.Fatalf("non-declarative runtime state must only appear in activeState: %+v", actual)
			}
			if len(mutations(executor.calls)) != 0 {
				t.Fatal("Test mutated the unit")
			}
		})
	}
}

func TestScopesAndTypes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	for _, scope := range []string{"system", "user"} {
		for _, name := range []string{"example", "example.timer", "example.socket", "example.mount", "example.target", "worker@one.service", `mnt-data\x20disk.mount`, "-.mount"} {
			t.Run(scope+"/"+name, func(t *testing.T) {
				manager, _ := fixture(t)
				actual, err := manager.Set(t.Context(), Unit{Name: name, Scope: scope, Enabled: ptr(true), State: ptr("active")})
				if err != nil || actual.Name != name || actual.Scope != scope || !*actual.Enabled || actual.ActiveState != "active" {
					t.Fatalf("scope/type transition: %+v %v", actual, err)
				}
			})
		}
	}
}

func TestInvalidProperties(t *testing.T) {
	for _, desired := range []Unit{
		{}, {Name: "../example"}, {Name: "example*"}, {Name: "example unit"},
		{Name: "a@@b.service"}, {Name: "@one.service"}, {Name: ".service"}, {Name: strings.Repeat("a", 256)},
		{Name: "example", State: ptr("")}, {Name: "example", State: ptr("restart")},
		{Name: "example", State: ptr("failed")}, {Name: "example", Scope: "global"},
		{Name: "worker@.service", State: ptr("active")}, {Name: "worker@.service", State: ptr("inactive")},
	} {
		manager, executor := fixture(t)
		for _, operation := range []func(context.Context, Unit) (Unit, error){manager.Get, manager.Set} {
			if _, err := operation(t.Context(), desired); err == nil {
				t.Fatalf("accepted invalid desired state %+v", desired)
			}
		}
		if len(executor.calls) != 0 {
			t.Fatal("invalid input executed systemctl")
		}
	}
}

func TestMissingStaticAndReload(t *testing.T) {
	t.Run("missing read-only", func(t *testing.T) {
		manager, executor := fixture(t)
		executor.load = "not-found"
		actual, diff, err := manager.Test(t.Context(), Unit{Name: "missing", State: ptr("active")})
		if err != nil || actual.LoadState != "not-found" || !slices.Equal(diff, []string{"state"}) || len(mutations(executor.calls)) != 0 {
			t.Fatalf("missing Test: %+v %v %v", actual, diff, err)
		}
		if _, err := manager.Set(t.Context(), Unit{Name: "missing", State: ptr("active")}); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("missing Set: %v", err)
		}
		if !slices.Equal(mutations(executor.calls), []string{"daemon-reload"}) {
			t.Fatalf("mutated a nonexistent unit: %v", executor.calls)
		}
	})
	t.Run("new definition", func(t *testing.T) {
		manager, executor := fixture(t)
		executor.load, executor.appearsOnLoad = "not-found", true
		if _, err := manager.Set(t.Context(), Unit{Name: "new", State: ptr("active")}); err != nil ||
			!slices.Equal(mutations(executor.calls), []string{"daemon-reload", "start"}) {
			t.Fatalf("reload new definition: %v %v", executor.calls, err)
		}
	})
	t.Run("conditional reload", func(t *testing.T) {
		manager, executor := fixture(t)
		executor.needReload = true
		if _, err := manager.Get(t.Context(), Unit{Name: "example"}); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Set(t.Context(), Unit{Name: "example", State: ptr("inactive")}); err != nil || len(mutations(executor.calls)) != 0 {
			t.Fatalf("compliant Set reloaded: %v %v", executor.calls, err)
		}
		if _, err := manager.Set(t.Context(), Unit{Name: "example", State: ptr("active")}); err != nil ||
			!slices.Equal(mutations(executor.calls), []string{"daemon-reload", "start"}) {
			t.Fatalf("stale definition transition: %v %v", executor.calls, err)
		}
	})
	t.Run("static", func(t *testing.T) {
		manager, executor := fixture(t)
		executor.static = true
		actual, err := manager.Get(t.Context(), Unit{Name: "static"})
		if err != nil || *actual.Enabled || actual.UnitFileState != "static" {
			t.Fatalf("static Get: %+v %v", actual, err)
		}
		if _, err := manager.Set(t.Context(), Unit{Name: "static", Enabled: ptr(true)}); err == nil || !strings.Contains(err.Error(), "static") {
			t.Fatalf("static enable claimed success: %v", err)
		}
	})
}

func TestFailuresAndMaskRestoration(t *testing.T) {
	for _, operation := range []string{"show", "daemon-reload", "unmask", "enable", "disable", "start", "stop", "reset-failed", "mask"} {
		t.Run(operation, func(t *testing.T) {
			manager, executor := fixture(t)
			executor.fail[operation] = "Permission denied"
			executor.needReload = operation == "daemon-reload"
			desired := Unit{Name: "example", Enabled: ptr(true), State: ptr("active"), Masked: ptr(true)}
			if operation == "unmask" || operation == "enable" || operation == "start" {
				executor.mask("system", "example.service")
			}
			if operation == "disable" {
				executor.enable("system", "example.service")
				desired.Enabled = ptr(false)
			}
			if operation == "stop" {
				executor.active = "active"
				desired.State = ptr("inactive")
			}
			if operation == "reset-failed" {
				executor.active = "failed"
				desired = Unit{Name: "example", State: ptr("inactive")}
			}
			if _, err := manager.Set(t.Context(), desired); err == nil || !strings.Contains(err.Error(), "Permission denied") {
				t.Fatalf("%s failure hidden: %v", operation, err)
			}
			if operation == "enable" || operation == "start" {
				if !executor.masked {
					t.Fatal("failed transition discarded mask")
				}
			}
		})
	}
	t.Run("restore failure is explicit", func(t *testing.T) {
		manager, executor := fixture(t)
		executor.mask("system", "example.service")
		executor.fail["start"], executor.fail["mask"] = "start failed", "mask failed"
		_, err := manager.Set(t.Context(), Unit{Name: "example", State: ptr("active")})
		if err == nil || !strings.Contains(err.Error(), "start failed") || !strings.Contains(err.Error(), "restore persistent mask") {
			t.Fatalf("restoration failure lost: %v", err)
		}
	})
	t.Run("user manager unavailable", func(t *testing.T) {
		manager, executor := fixture(t)
		executor.fail["show"] = "Failed to connect to bus"
		_, err := manager.Get(t.Context(), Unit{Name: "example", Scope: "user"})
		if err == nil || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR") {
			t.Fatalf("missing actionable user-manager error: %v", err)
		}
	})
	t.Run("verification", func(t *testing.T) {
		manager, executor := fixture(t)
		executor.ignoreMutation = true
		_, err := manager.Set(t.Context(), Unit{Name: "example", State: ptr("active")})
		if err == nil || !strings.Contains(err.Error(), "did not converge") {
			t.Fatalf("no-op systemctl claimed success: %v", err)
		}
	})
	t.Run("runtime mask preserved", func(t *testing.T) {
		manager, executor := fixture(t)
		executor.runtimeMask = true
		actual, err := manager.Get(t.Context(), Unit{Name: "example"})
		if err != nil || *actual.Masked || actual.UnitFileState != "masked-runtime" {
			t.Fatalf("runtime mask mistaken for persistent: %+v %v", actual, err)
		}
		_, err = manager.Set(t.Context(), Unit{Name: "example", State: ptr("active")})
		if err == nil || !strings.Contains(err.Error(), "runtime mask") || len(mutations(executor.calls)) != 0 {
			t.Fatalf("runtime mask silently removed: %v %v", executor.calls, err)
		}
	})
	t.Run("inherited template mask preserved", func(t *testing.T) {
		manager, executor := fixture(t)
		executor.file, executor.fragment = "masked", "/etc/systemd/system/worker@.service"
		actual, err := manager.Get(t.Context(), Unit{Name: "worker@one.service"})
		if err != nil || !*actual.Masked {
			t.Fatalf("inherited mask not observed: %+v %v", actual, err)
		}
		_, err = manager.Set(t.Context(), Unit{Name: "worker@one.service", Masked: ptr(false)})
		if err == nil || !strings.Contains(err.Error(), "inherits its mask") || len(mutations(executor.calls)) != 0 {
			t.Fatalf("shared template mask was changed: %v %v", executor.calls, err)
		}
	})
	t.Run("unmask prunes enablement", func(t *testing.T) {
		manager, executor := fixture(t)
		executor.mask("system", "example.service")
		executor.enable("system", "example.service")
		executor.pruneOnUnmask = true
		link := executor.path("system", filepath.Join("default.target.wants", "example.service"))
		before, err := os.Readlink(link)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := manager.Set(t.Context(), Unit{Name: "example", Masked: ptr(false)})
		if err != nil || !*actual.Enabled || *actual.Masked ||
			!slices.Equal(mutations(executor.calls), []string{"unmask", "daemon-reload"}) {
			t.Fatalf("unmask discarded unmanaged enablement: %+v %v %v", actual, executor.calls, err)
		}
		after, err := os.Readlink(link)
		if err != nil || before != after {
			t.Fatalf("unmask changed exact dependency: %q, want %q (%v)", after, before, err)
		}
	})
}
