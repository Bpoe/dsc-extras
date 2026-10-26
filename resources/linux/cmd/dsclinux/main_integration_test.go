package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-extras/resources/linux/internal/services"
)

// DSC runs this test executable with a fake manager; no host units are touched.
func TestDSCResourceProcess(t *testing.T) {
	path := os.Getenv("DSCLINUX_TEST_STATE")
	if path == "" {
		return
	}
	manager := newFakeManager()
	manager.actual = services.Unit{}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manager.actual); err != nil {
		t.Fatal(err)
	}
	for i, arg := range os.Args {
		if arg != "--" {
			continue
		}
		var resource serviceManager = manager
		if slices.Equal(os.Args[i+1:], []string{"services", "export"}) {
			resource = services.NewManager(integrationSystemctl{manager.actual}, filepath.Dir(path))
		}
		code := run(t.Context(), os.Args[i+1:], os.Stdin, os.Stdout, os.Stderr, resource, nil)
		if manager.sets > 0 {
			data, err := json.Marshal(manager.actual)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		os.Exit(code)
	}
	t.Fatal("missing resource arguments")
}

type integrationSystemctl struct{ actual services.Unit }

func (f integrationSystemctl) Execute(_ context.Context, args []string) ([]byte, []byte, error) {
	switch args[3] {
	case "list-unit-files":
		return []byte("example.service disabled -\nother.service disabled -\n"), nil, nil
	case "show":
		return []byte(fmt.Sprintf("ActiveState=%s\nSubState=%s\nLoadState=%s\nUnitFileState=%s\n",
			f.actual.ActiveState, f.actual.SubState, f.actual.LoadState, f.actual.UnitFileState)), nil, nil
	default:
		return nil, nil, fmt.Errorf("unexpected mutating systemctl invocation: %v", args)
	}
}

func TestDSCIntegration(t *testing.T) {
	dsc, err := exec.LookPath("dsc")
	if err != nil {
		if os.Getenv("DSCLINUX_REQUIRE_DSC") == "1" {
			t.Fatal("DSC executable required by DSCLINUX_REQUIRE_DSC but not available on PATH")
		}
		t.Skip("DSC executable not available on PATH")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "manifests", "services.dsc.resource.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"get", "test", "set", "export"} {
		var command map[string]any
		if err := json.Unmarshal(manifest[operation], &command); err != nil {
			t.Fatal(err)
		}
		command["executable"] = executable
		command["args"] = []string{"-test.run=^TestDSCResourceProcess$", "--", "services", operation}
		manifest[operation], err = json.Marshal(command)
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "services.dsc.resource.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSC_RESOURCE_PATH", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "user-config"))
	t.Setenv("XDG_CONFIG_DIRS", filepath.Join(dir, "shared-config"))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	resourceType := "DscExtras.Linux/services"
	for _, operation := range []string{"list", "schema"} {
		args := []string{"resource", operation, "--resource", resourceType}
		if operation == "list" {
			args = []string{"resource", "list", resourceType}
		}
		command := exec.CommandContext(ctx, dsc, args...)
		output, err := command.CombinedOutput()
		if err != nil || (operation == "list" && !bytes.Contains(output, []byte(resourceType))) ||
			(operation == "schema" && !bytes.Contains(output, []byte(`"x-dsc-key"`))) {
			t.Fatalf("DSC %s failed: %v\n%s", operation, err, output)
		}
	}
	for _, state := range []string{"active", "inactive", "failed", "activating", "deactivating"} {
		t.Run(state, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			actual := newFakeManager().actual
			actual.ActiveState = state
			if state == "active" {
				actual.State = pointer(state)
			} else if state != "inactive" {
				actual.State = nil
			}
			data, err := json.Marshal(actual)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DSCLINUX_TEST_STATE", path)
			invoke := func(operation, input string) map[string]json.RawMessage {
				t.Helper()
				command := exec.CommandContext(ctx, dsc, "resource", operation, "--resource", resourceType,
					"--file", "-", "--output-format", "json")
				command.Stdin = strings.NewReader(input)
				var stderr strings.Builder
				command.Stderr = &stderr
				output, err := command.Output()
				if err != nil {
					t.Fatalf("DSC %s failed: %v\n%s\n%s", operation, err, output, stderr.String())
				}
				var result map[string]json.RawMessage
				if err := json.Unmarshal(output, &result); err != nil {
					t.Fatalf("DSC output: %s (%v)", output, err)
				}
				return result
			}
			for _, filter := range []struct {
				input string
				scope string
				names []string
			}{
				{"", "system", []string{"example.service", "other.service"}},
				{`{}`, "system", []string{"example.service", "other.service"}},
				{`{"scope":"system"}`, "system", []string{"example.service", "other.service"}},
				{`{"scope":"user"}`, "user", []string{"example.service", "other.service"}},
				{`{"name":"example","scope":"user"}`, "user", []string{"example.service"}},
				{`{"name":"missing","scope":"user"}`, "user", []string{}},
			} {
				args := []string{"resource", "export", "--resource", resourceType, "--output-format", "json"}
				if filter.input != "" {
					args = append(args, "--input", filter.input)
				}
				command := exec.CommandContext(ctx, dsc, args...)
				var stderr strings.Builder
				command.Stderr = &stderr
				output, err := command.Output()
				if err != nil {
					t.Fatalf("DSC %s failed to consume Export: %v\n%s\n%s", strings.Join(args, " "), err, output, stderr.String())
				}
				var config struct {
					Resources []struct {
						Name       string
						Properties map[string]json.RawMessage
					}
				}
				if err := json.Unmarshal(output, &config); err != nil || len(config.Resources) != len(filter.names) {
					t.Fatalf("filtered Export = %s, %v", output, err)
				}
				for i, resource := range config.Resources {
					name := filter.names[i]
					if filter.scope != "system" {
						name += "," + filter.scope
					}
					properties := resource.Properties
					var exported services.Unit
					data, err := json.Marshal(properties)
					if err != nil || json.Unmarshal(data, &exported) != nil ||
						resource.Name != encodeExportName(name) || exported.Name != filter.names[i] || exported.Scope != filter.scope ||
						exported.Enabled == nil || *exported.Enabled || exported.Masked == nil || *exported.Masked {
						t.Fatalf("incorrect exported properties: %s (%v)", data, err)
					}
					wantKeys := 8
					if state == "active" || state == "inactive" {
						wantKeys++
						if exported.State == nil || *exported.State != state {
							t.Fatalf("lost representable state: %s", data)
						}
					} else if exported.State != nil {
						t.Fatalf("guessed runtime state: %s", data)
					}
					if len(properties) != wantKeys {
						t.Fatalf("Export lost observed properties: %s", data)
					}
					get := invoke("get", string(data))
					var observed services.Unit
					if err := json.Unmarshal(get["actualState"], &observed); err != nil ||
						!reflect.DeepEqual(exported, observed) {
						t.Fatalf("Export differs from Get: %s; Get = %s, %v", data, get["actualState"], err)
					}
				}
			}
			for _, operation := range []string{"get", "test", "set", "export"} {
				input := `{"scope":"user"}`
				if operation == "export" {
					input = `{"scope":"global"}`
				}
				output, err := exec.CommandContext(ctx, dsc, "resource", operation, "--resource", resourceType,
					"--input", input, "--output-format", "json").CombinedOutput()
				if err == nil {
					t.Fatalf("DSC %s accepted invalid input %s: %s", operation, input, output)
				}
				property := "name"
				if operation == "export" {
					property = "global"
				}
				if !bytes.Contains(output, []byte(property)) {
					t.Fatalf("DSC %s failed for an unexpected reason: %s", operation, output)
				}
			}
			output, err := exec.CommandContext(ctx, dsc, "resource", "get", "--all", "--resource", resourceType,
				"--output-format", "json").Output()
			if err != nil || !bytes.Contains(output, []byte("example.service")) {
				t.Fatalf("DSC Get --all failed: %v\n%s", err, output)
			}
			lines := bytes.Split(bytes.TrimSpace(output), []byte("\n"))
			if len(lines) != 2 {
				t.Fatalf("DSC Get --all did not return two JSON Lines: %s", output)
			}
			for _, line := range lines {
				if !json.Valid(line) {
					t.Fatalf("DSC Get --all returned invalid JSON Line: %s", line)
				}
			}
			desired := `{"name":"example","state":"active","enabled":true,"masked":true,"futureProperty":42}`
			get := invoke("get", `{"name":"example"}`)
			var observed services.Unit
			if err := json.Unmarshal(get["actualState"], &observed); err != nil || observed.ActiveState != state ||
				(state != "active" && state != "inactive" && observed.State != nil) {
				t.Fatalf("DSC Get lost raw runtime state: %+v %v", observed, err)
			}
			result := invoke("test", desired)
			var diff []string
			wantDiff := []string{"state", "enabled", "masked"}
			if state == "active" {
				wantDiff = wantDiff[1:]
			}
			if err := json.Unmarshal(result["differingProperties"], &diff); err != nil ||
				!slices.Equal(diff, wantDiff) || string(result["inDesiredState"]) != "false" {
				t.Fatalf("DSC stateAndDiff: %s %v", result, err)
			}
			unchanged, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, unchanged) {
				t.Fatalf("Get/Test modified fixture: %v", err)
			}
			result = invoke("set", desired)
			if err := json.Unmarshal(result["afterState"], &observed); err != nil ||
				observed.ActiveState != "active" || !*observed.Enabled || !*observed.Masked {
				t.Fatalf("DSC Set: %+v %v", observed, err)
			}
			result = invoke("test", desired)
			if string(result["inDesiredState"]) != "true" || string(result["differingProperties"]) != "[]" {
				t.Fatalf("DSC did not converge: %s", result)
			}
			invoke("set", desired)
			result = invoke("test", `{"name":"example"}`)
			if string(result["inDesiredState"]) != "true" || string(result["differingProperties"]) != "[]" {
				t.Fatalf("omitted properties were compared: %s", result)
			}
		})
	}
}

func TestDSCAvailabilityPolicy(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	for _, required := range []string{"0", "1"} {
		t.Run("required="+required, func(t *testing.T) {
			t.Setenv("DSCLINUX_REQUIRE_DSC", required)
			output, err := exec.CommandContext(t.Context(), executable, "-test.run=^TestDSC(Groups)?Integration$", "-test.v").CombinedOutput()
			if required == "1" {
				if err == nil || !bytes.Contains(output, []byte("DSC executable required")) {
					t.Fatalf("missing required DSC must fail: %v\n%s", err, output)
				}
			} else if err != nil || !bytes.Contains(output, []byte("--- SKIP:")) {
				t.Fatalf("optional missing DSC must skip: %v\n%s", err, output)
			}
		})
	}
}
