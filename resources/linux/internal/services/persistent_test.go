//go:build linux

package services

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPersistentConfiguration(t *testing.T) {
	for _, dependency := range []string{"default.target.wants", "example.target.requires", "example.target.upholds"} {
		t.Run(dependency, func(t *testing.T) {
			manager, executor := fixture(t)
			executor.mask("system", "example.service")
			symlink(t, executor.path("system", filepath.Join(dependency, "example.service")), filepath.Join("..", "example.service"))
			actual, err := manager.Get(t.Context(), Unit{Name: "example"})
			if err != nil || !*actual.Enabled || !*actual.Masked || actual.UnitFileState != "masked" {
				t.Fatalf("mask hid underlying enablement: %+v %v", actual, err)
			}
		})
	}
	t.Run("runtime symlinks are not persistent", func(t *testing.T) {
		manager, _ := fixture(t)
		symlink(t, filepath.Join(manager.root, "run", "systemd", "system", "example.service"), "/dev/null")
		symlink(t, filepath.Join(manager.root, "run", "systemd", "system", "default.target.wants", "example.service"), "../example.service")
		enabled, _, err := manager.persistentEnablement("system", []string{"example.service"})
		if err != nil || enabled {
			t.Fatalf("runtime configuration leaked into persistent state: %t %v", enabled, err)
		}
	})
	t.Run("alias only enablement", func(t *testing.T) {
		manager, executor := fixture(t)
		symlink(t, executor.path("system", "alias.service"), "example.service")
		enabled, _, err := manager.persistentEnablement("system", []string{"example.service"})
		if err != nil || !enabled {
			t.Fatalf("Alias= enablement lost: %t %v", enabled, err)
		}
	})
	t.Run("linked unit is not enabled", func(t *testing.T) {
		manager, executor := fixture(t)
		symlink(t, executor.path("system", "example.service"), filepath.Join(manager.root, "opt", "example.service"))
		enabled, _, err := manager.persistentEnablement("system", []string{"example.service"})
		if err != nil || enabled {
			t.Fatalf("linked unit mistaken for enabled: %t %v", enabled, err)
		}
	})
	t.Run("masked dependency is not enabled", func(t *testing.T) {
		manager, executor := fixture(t)
		symlink(t, executor.path("system", filepath.Join("default.target.wants", "example.service")), "/dev/null")
		enabled, _, err := manager.persistentEnablement("system", []string{"example.service"})
		if err != nil || enabled {
			t.Fatalf("masked dependency mistaken for enablement: %t %v", enabled, err)
		}
	})
	t.Run("inspection error is not disabled fallback", func(t *testing.T) {
		manager, executor := fixture(t)
		dir := executor.path("system", "")
		if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir, []byte("not a directory"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Get(t.Context(), Unit{Name: "example"}); err == nil {
			t.Fatal("inspection failure became a success-shaped disabled result")
		}
	})
}

func TestUserConfiguration(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	t.Setenv("XDG_CONFIG_DIRS", t.TempDir())
	manager, _ := fixture(t)
	dirs, err := manager.configDirs("user")
	if err != nil || len(dirs) != 3 {
		t.Fatalf("user dirs: %v %v", dirs, err)
	}

	symlink(t, filepath.Join(dirs[2], "default.target.wants", "example.service"), "../example.service")
	enabled, _, err := manager.persistentEnablement("user", []string{"example.service"})
	if err != nil || !enabled {
		t.Fatalf("global user enablement: %t %v", enabled, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative")
	if _, err := manager.configDirs("user"); err == nil {
		t.Fatal("relative XDG_CONFIG_HOME accepted")
	}
}

func TestTemplateEnablement(t *testing.T) {
	for _, scope := range []string{"system", "user"} {
		for _, dependency := range []string{"default.target.wants", "example.target.requires", "example.target.upholds"} {
			t.Run(scope+"/"+dependency, func(t *testing.T) {
				manager, executor := fixture(t)
				path := executor.path(scope, filepath.Join(dependency, "worker@default.service"))
				symlink(t, path, "../worker@.service")
				for _, test := range []struct {
					name string
					want bool
				}{
					{"worker@.service", true},
					{"worker@default.service", true},
					{"worker@other.service", false},
					{"other@.service", false},
					{"worker.service", false},
					{"worker@.timer", false},
				} {
					enabled, links, err := manager.persistentEnablement(scope, []string{test.name})
					if err != nil || enabled != test.want || (len(links) == 1) != test.want {
						t.Fatalf("%s enablement = %t, links = %v, error = %v", test.name, enabled, links, err)
					}
					if test.want && links[0].path != path {
						t.Fatalf("template did not retain the exact instance link: %+v", links)
					}
				}
			})
		}
	}
	t.Run("template alias", func(t *testing.T) {
		manager, executor := fixture(t)
		symlink(t, executor.path("system", "alias@.service"), "worker@.service")
		symlink(t, executor.path("system", filepath.Join("default.target.wants", "alias@default.service")), "../alias@.service")
		enabled, links, err := manager.persistentEnablement("system", []string{"worker@.service"})
		if err != nil || !enabled || len(links) != 2 {
			t.Fatalf("aliased instance enablement lost: %t %v %v", enabled, links, err)
		}
	})
	t.Run("runtime instance is not persistent", func(t *testing.T) {
		manager, _ := fixture(t)
		symlink(t, filepath.Join(manager.root, "run", "systemd", "system", "default.target.wants", "worker@default.service"), "../worker@.service")
		enabled, _, err := manager.persistentEnablement("system", []string{"worker@.service"})
		if err != nil || enabled {
			t.Fatalf("runtime instance enabled template persistently: %t %v", enabled, err)
		}
	})
	t.Run("invalid instance names do not enable template", func(t *testing.T) {
		manager, executor := fixture(t)
		for _, instance := range []string{"worker@@default.service", "worker@*.service", "worker@invalid instance.service"} {
			symlink(t, executor.path("system", filepath.Join("default.target.wants", instance)), "../worker@.service")
		}
		enabled, _, err := manager.persistentEnablement("system", []string{"worker@.service"})
		if err != nil || enabled {
			t.Fatalf("invalid instance enabled template: %t %v", enabled, err)
		}
	})
}

func TestRestoreEnablement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.target.wants", "example.service")
	link := configLink{path: path, name: "example.service", target: "../example.service", dependency: true}
	restored, err := restoreEnablement([]configLink{link})
	if err != nil || !restored {
		t.Fatalf("restore missing link: %t %v", restored, err)
	}
	restored, err = restoreEnablement([]configLink{link})
	if err != nil || restored {
		t.Fatalf("existing link must not be rewritten: %t %v", restored, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("concurrent configuration"), 0644); err != nil {
		t.Fatal(err)
	}
	restored, err = restoreEnablement([]configLink{link})
	data, readErr := os.ReadFile(path)
	if err != nil || restored || readErr != nil || string(data) != "concurrent configuration" {
		t.Fatalf("overwrote concurrent configuration: %t %v %q %v", restored, err, data, readErr)
	}
	link.path = filepath.Join(path, "not-a-directory", "example.service")
	if _, err := restoreEnablement([]configLink{link}); err == nil {
		t.Fatal("restore failure was ignored")
	}
}
