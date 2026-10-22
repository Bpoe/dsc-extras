package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
		code := run(t.Context(), os.Args[i+1:], os.Stdin, os.Stdout, os.Stderr, manager)
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
	for _, operation := range []string{"get", "test", "set"} {
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
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
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
	for _, state := range []string{"inactive", "failed", "activating", "deactivating"} {
		t.Run(state, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			actual := newFakeManager().actual
			actual.ActiveState = state
			if state != "inactive" {
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
			desired := `{"name":"example","state":"active","enabled":true,"masked":true,"futureProperty":42}`
			get := invoke("get", `{"name":"example"}`)
			var observed services.Unit
			if err := json.Unmarshal(get["actualState"], &observed); err != nil || observed.ActiveState != state ||
				(state != "inactive" && observed.State != nil) {
				t.Fatalf("DSC Get lost raw runtime state: %+v %v", observed, err)
			}
			result := invoke("test", desired)
			var diff []string
			if err := json.Unmarshal(result["differingProperties"], &diff); err != nil ||
				!slices.Equal(diff, []string{"state", "enabled", "masked"}) || string(result["inDesiredState"]) != "false" {
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
			output, err := exec.CommandContext(t.Context(), executable, "-test.run=^TestDSCIntegration$", "-test.v").CombinedOutput()
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
