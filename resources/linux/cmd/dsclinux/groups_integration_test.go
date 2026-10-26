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

	"github.com/Bpoe/dsc-extras/resources/linux/internal/groups"
)

// DSC invokes this child process with a fake manager; host groups are never used.
func TestDSCGroupsResourceProcess(t *testing.T) {
	path := os.Getenv("DSCLINUX_TEST_GROUP_STATE")
	if path == "" {
		return
	}
	manager := newFakeGroupManager()
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
		code := run(t.Context(), os.Args[i+1:], os.Stdin, os.Stdout, os.Stderr, nil, manager)
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

func TestDSCGroupsIntegration(t *testing.T) {
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
	data, err := os.ReadFile(filepath.Join("..", "..", "manifests", "groups.dsc.resource.json"))
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
		command["args"] = []string{"-test.run=^TestDSCGroupsResourceProcess$", "--", "groups", operation}
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
	if err := os.WriteFile(filepath.Join(dir, "groups.dsc.resource.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSC_RESOURCE_PATH", dir)
	path := filepath.Join(t.TempDir(), "state.json")
	initial := []byte(`{"name":"dev","ensure":"Absent"}`)
	if err := os.WriteFile(path, initial, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSCLINUX_TEST_GROUP_STATE", path)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	resourceType := "DscExtras.Linux/groups"
	for _, operation := range []string{"list", "schema"} {
		args := []string{"resource", operation, "--resource", resourceType}
		if operation == "list" {
			args = []string{"resource", "list", resourceType}
		}
		output, err := exec.CommandContext(ctx, dsc, args...).CombinedOutput()
		if err != nil || (operation == "list" && !bytes.Contains(output, []byte(resourceType))) ||
			(operation == "schema" && !bytes.Contains(output, []byte(`"x-dsc-key"`))) {
			t.Fatalf("DSC %s: %v\n%s", operation, err, output)
		}
	}
	invokeRaw := func(operation, input string) ([]byte, string, error) {
		command := exec.CommandContext(ctx, dsc, "resource", operation, "--resource", resourceType,
			"--file", "-", "--output-format", "json")
		command.Stdin = strings.NewReader(input)
		var stderr strings.Builder
		command.Stderr = &stderr
		output, err := command.Output()
		return output, stderr.String(), err
	}
	invoke := func(operation, input string) map[string]json.RawMessage {
		t.Helper()
		output, stderr, err := invokeRaw(operation, input)
		if err != nil {
			t.Fatalf("DSC %s: %v\n%s\n%s", operation, err, output, stderr)
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatalf("DSC output: %s (%v)", output, err)
		}
		return result
	}
	assertDiff := func(input string, want []string) {
		t.Helper()
		result := invoke("test", input)
		var diff []string
		if err := json.Unmarshal(result["differingProperties"], &diff); err != nil || diff == nil ||
			!slices.Equal(diff, want) || string(result["inDesiredState"]) != map[bool]string{true: "true", false: "false"}[len(want) == 0] {
			t.Fatalf("DSC stateAndDiff: %s, %v; want %v", result, err, want)
		}
	}
	assertState := func(result map[string]json.RawMessage, key, ensure string, members []string) {
		t.Helper()
		var actual groups.Group
		if err := json.Unmarshal(result[key], &actual); err != nil || actual.Ensure != ensure ||
			actual.IncludedMembers != nil || actual.ExcludedMembers != nil {
			t.Fatalf("DSC state: %s (%v)", result, err)
		}
		if ensure == "Absent" {
			if actual.GID != nil || actual.Members != nil {
				t.Fatalf("invented absent properties: %+v", actual)
			}
		} else if actual.GID == nil || actual.Members == nil || !slices.Equal(*actual.Members, members) {
			t.Fatalf("incorrect observed group: %+v", actual)
		}
	}
	assertState(invoke("get", `{"name":"dev"}`), "actualState", "Absent", nil)
	assertDiff(`{"name":"dev","gid":3000,"includedMembers":["alice"]}`, []string{"ensure", "gid", "includedMembers"})
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(initial, unchanged) {
		t.Fatalf("Get/Test mutated fixture: %v", err)
	}
	for _, input := range []string{
		`{}`, `{"name":""}`, `{"name":"dev","ensure":"invalid"}`, `{"name":"dev","gid":-1}`,
		`{"name":"dev","gid":4294967295}`, `{"name":"dev","members":null}`,
		`{"name":"dev","members":["alice","alice"]}`, `{"name":"dev","members":[],"includedMembers":[]}`,
		`{"name":"dev","members":[],"excludedMembers":[]}`, `{"name":"dev","ensure":"Absent","gid":3000}`,
		`{"name":"dev","ensure":"Absent","members":[]}`,
	} {
		if output, stderr, err := invokeRaw("set", input); err == nil {
			t.Fatalf("DSC accepted invalid schema input %s: %s %s", input, output, stderr)
		}
	}
	unchanged, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(initial, unchanged) {
		t.Fatalf("schema rejection invoked Set: %v", err)
	}
	assertState(invoke("set", `{"name":"dev","gid":3000,"members":["old","bob"]}`), "afterState", "Present", []string{"bob", "old"})
	for _, args := range [][]string{
		{"resource", "export", "--resource", resourceType, "--output-format", "json"},
		{"resource", "get", "--all", "--resource", resourceType, "--output-format", "json"},
	} {
		output, err := exec.CommandContext(ctx, dsc, args...).Output()
		if err != nil || !json.Valid(output) || !bytes.Contains(output, []byte("dev")) {
			t.Fatalf("DSC %s failed to consume Export: %v\n%s", strings.Join(args, " "), err, output)
		}
		if args[1] == "export" {
			var config struct {
				Resources []struct {
					Name       string
					Properties groups.Group
				}
			}
			if err := json.Unmarshal(output, &config); err != nil || len(config.Resources) != 1 {
				t.Fatalf("DSC group Export: %s, %v", output, err)
			}
			exported := config.Resources[0].Properties
			if config.Resources[0].Name != exported.Name {
				t.Fatalf("DSC group Export did not use the group name: %s", output)
			}
			input, err := json.Marshal(exported)
			if err != nil {
				t.Fatal(err)
			}
			get := invoke("get", string(input))
			var observed groups.Group
			if err := json.Unmarshal(get["actualState"], &observed); err != nil || !reflect.DeepEqual(exported, observed) {
				t.Fatalf("Export differs from Get: %s; Get = %s, %v", input, get["actualState"], err)
			}
		}
	}
	assertDiff(`{"name":"dev","includedMembers":["alice"],"excludedMembers":["old"]}`, []string{"includedMembers", "excludedMembers"})
	assertState(invoke("set", `{"name":"dev","includedMembers":["alice"],"excludedMembers":["old"],"future":42}`),
		"afterState", "Present", []string{"alice", "bob"})
	assertDiff(`{"name":"dev","gid":3000,"members":["bob","alice"]}`, []string{})
	assertDiff(`{"name":"dev"}`, []string{})
	assertState(invoke("get", `{"name":"dev","includedMembers":["carol"]}`), "actualState", "Present", []string{"alice", "bob"})
	assertState(invoke("set", `{"name":"dev","members":[]}`), "afterState", "Present", []string{})
	assertDiff(`{"name":"dev","members":[]}`, []string{})
	invoke("set", `{"name":"dev","members":[]}`)
	assertState(invoke("set", `{"name":"dev","ensure":"Absent"}`), "afterState", "Absent", nil)
	assertDiff(`{"name":"dev","ensure":"Absent"}`, []string{})
	invoke("set", `{"name":"dev","ensure":"Absent"}`)
}
