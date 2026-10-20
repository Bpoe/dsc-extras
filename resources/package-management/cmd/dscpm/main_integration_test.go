package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/apt"
)

// DSC invokes this test executable so the real command uses an isolated APT root.
func TestDSCResourceProcess(t *testing.T) {
	root := os.Getenv("DSCAPT_TEST_ROOT")
	if root == "" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Exit(run(os.Args[i+1:], os.Stdin, os.Stdout, os.Stderr, apt.NewStore(root)))
		}
	}
	t.Fatal("missing resource arguments")
}

func TestDSCStateAndDiffIntegration(t *testing.T) {
	for _, manifest := range []string{"aptRepositories.dsc.resource.json", "dscapt.dsc.resource.json"} {
		t.Run(manifest, func(t *testing.T) {
			testDSCStateAndDiffIntegration(t, manifest)
		})
	}
}

func testDSCStateAndDiffIntegration(t *testing.T, manifestName string) {
	dsc, err := exec.LookPath("dsc")
	if err != nil {
		t.Skip("DSC executable not available on PATH")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "manifests", manifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	var resourceType string
	if err := json.Unmarshal(manifest["type"], &resourceType); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"get", "test", "set"} {
		var command struct {
			Executable        string   `json:"executable"`
			Args              []string `json:"args"`
			Input             string   `json:"input"`
			Return            string   `json:"return,omitempty"`
			ImplementsPretest bool     `json:"implementsPretest,omitempty"`
		}
		if err := json.Unmarshal(manifest[operation], &command); err != nil {
			t.Fatal(err)
		}
		if operation == "test" && command.Return != "stateAndDiff" {
			t.Fatalf("manifest test.return = %q, want stateAndDiff", command.Return)
		}
		command.Executable = executable
		command.Args = append([]string{"-test.run=^TestDSCResourceProcess$", "--"}, command.Args...)
		manifest[operation], err = json.Marshal(command)
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	resourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(resourceDir, manifestName), data, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSC_RESOURCE_PATH", resourceDir)
	list := exec.CommandContext(t.Context(), dsc, "resource", "list", resourceType)
	if output, err := list.CombinedOutput(); err != nil || !bytes.Contains(output, []byte(resourceType)) {
		t.Fatalf("DSC could not discover %s: %v\n%s", resourceType, err, output)
	}
	otherKey := differentSigningKey(t)
	for _, test := range []struct {
		name   string
		change func(*apt.Repository)
		want   []string
	}{
		{name: "equivalent armored and binary keys"},
		{name: "different key", change: func(desired *apt.Repository) { desired.SigningKey = otherKey }, want: []string{"signingKey"}},
		{name: "missing repository", change: func(desired *apt.Repository) { desired.URI += "/missing" }, want: []string{"ensure"}},
		{name: "already absent", change: func(desired *apt.Repository) {
			desired.URI += "/missing"
			desired.Ensure, desired.SigningKey = "Absent", ""
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := makeStore(t)
			desired := externalRepository(t, store, ".gpg")
			if test.change != nil {
				test.change(&desired)
			}
			t.Setenv("DSCAPT_TEST_ROOT", store.Root)
			input, err := json.Marshal(desired)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, dsc, "resource", "test", "--resource", resourceType,
				"--file", "-", "--output-format", "json")
			command.Stdin = bytes.NewReader(input)
			var stderr strings.Builder
			command.Stderr = &stderr
			output, err := command.Output()
			if err != nil {
				t.Fatalf("DSC resource test failed: %v\n%s\n%s", err, output, stderr.String())
			}
			var result struct {
				ActualState         testResult `json:"actualState"`
				InDesiredState      bool       `json:"inDesiredState"`
				DifferingProperties []string   `json:"differingProperties"`
			}
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatalf("invalid DSC result: %s, %v", output, err)
			}
			if result.DifferingProperties == nil || !slices.Equal(result.DifferingProperties, test.want) ||
				result.InDesiredState != (len(test.want) == 0) ||
				result.ActualState.InDesiredState != result.InDesiredState {
				t.Fatalf("unexpected DSC result: %s, want differences %v", output, test.want)
			}
			actual, err := store.Get(desired)
			if err != nil || !reflect.DeepEqual(actual, result.ActualState.Repository) {
				t.Fatalf("DSC did not preserve actual state: %s; Get = (%+v, %v)", output, actual, err)
			}
			if test.change == nil && result.ActualState.SigningKey == desired.SigningKey {
				t.Fatal("regression requires different actual and desired armor strings")
			}
			for _, operation := range []string{"get", "set"} {
				command := exec.CommandContext(ctx, dsc, "resource", operation, "--resource", resourceType,
					"--file", "-", "--output-format", "json")
				command.Stdin = bytes.NewReader(input)
				output, err := command.CombinedOutput()
				if operation == "set" && test.name == "different key" {
					if err == nil || !bytes.Contains(output, []byte("externally managed")) {
						t.Fatalf("DSC Set must reject external key rotation: %v\n%s", err, output)
					}
					observed, getErr := store.Get(desired)
					if getErr != nil || !reflect.DeepEqual(actual, observed) {
						t.Fatalf("rejected DSC Set changed actual state: %+v (%v)", observed, getErr)
					}
					continue
				}
				if err != nil {
					t.Fatalf("DSC resource %s failed: %v\n%s", operation, err, output)
				}
				if operation == "get" {
					var result struct {
						ActualState apt.Repository `json:"actualState"`
					}
					if err := json.Unmarshal(output, &result); err != nil || !reflect.DeepEqual(actual, result.ActualState) {
						t.Fatalf("DSC Get changed actual state: %s (%v)", output, err)
					}
				} else {
					var result struct {
						AfterState apt.Repository `json:"afterState"`
					}
					observed, differences, storeErr := store.Test(desired)
					if err := json.Unmarshal(output, &result); err != nil || storeErr != nil ||
						len(differences) != 0 || !reflect.DeepEqual(observed, result.AfterState) {
						t.Fatalf("DSC Set did not converge: %s (%v, %v, %v)", output, err, storeErr, differences)
					}
				}
			}
		})
	}
}
