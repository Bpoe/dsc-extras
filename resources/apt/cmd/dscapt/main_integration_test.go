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

	"github.com/Bpoe/dsc-extras/resources/apt/internal/apt"
)

// DSC invokes this test executable so the real command uses an isolated APT root.
func TestDSCResourceProcess(t *testing.T) {
	root := os.Getenv("DSCAPT_TEST_ROOT")
	if root == "" {
		return
	}
	os.Exit(run(os.Args[len(os.Args)-1:], os.Stdin, os.Stdout, os.Stderr, apt.NewStore(root)))
}

func TestDSCStateAndDiffIntegration(t *testing.T) {
	dsc, err := exec.LookPath("dsc")
	if err != nil {
		t.Skip("DSC executable not available on PATH")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "dscapt.dsc.resource.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	var testOperation struct {
		Executable string   `json:"executable"`
		Args       []string `json:"args"`
		Input      string   `json:"input"`
		Return     string   `json:"return"`
	}
	if err := json.Unmarshal(manifest["test"], &testOperation); err != nil {
		t.Fatal(err)
	}
	if testOperation.Return != "stateAndDiff" {
		t.Fatalf("manifest test.return = %q, want stateAndDiff", testOperation.Return)
	}
	testOperation.Executable = executable
	testOperation.Args = append([]string{"-test.run=^TestDSCResourceProcess$", "--"}, testOperation.Args...)
	manifest["test"], err = json.Marshal(testOperation)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	resourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(resourceDir, "dscapt.dsc.resource.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSC_RESOURCE_PATH", resourceDir)
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
			command := exec.CommandContext(ctx, dsc, "resource", "test", "--resource", "DscExtras.Apt/Repository",
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
		})
	}
}
