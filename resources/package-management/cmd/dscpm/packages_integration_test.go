package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/apt"
)

type dscPackageExecutor struct{ root string }

type missingPackageError struct{}

func (missingPackageError) Error() string { return "exit status 1" }
func (missingPackageError) ExitCode() int { return 1 }

func newDSCPackageManager(root string) *apt.PackageManager {
	return apt.NewPackageManager(dscPackageExecutor{root})
}

func dscLocalIdentity(path string) (string, string) {
	if filepath.Base(path) == "fixture-two.deb" {
		return "local-two", "2.0"
	}
	return "local-one", "1.0"
}

// Persist only fake installed versions so DSC's separate processes share state.
func (e dscPackageExecutor) Execute(ctx context.Context, program string, args, env []string) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	statePath := filepath.Join(e.root, "installed.json")
	state := map[string]string{}
	if data, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return nil, nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	switch program {
	case "dpkg-deb":
		name, version := dscLocalIdentity(args[len(args)-1])
		return fmt.Appendf(nil, "%s\t%s\tamd64\n", name, version), nil, nil
	case "dpkg":
		switch args[0] {
		case "--print-architecture":
			return []byte("amd64\n"), nil, nil
		case "--print-foreign-architectures":
			return nil, nil, nil
		case "--validate-version", "--compare-versions":
			command := exec.CommandContext(ctx, program, args...)
			output, err := command.Output()
			return output, nil, err
		}
	case "dpkg-query":
		if len(args) == 2 && args[0] == "--show" {
			var inventory []byte
			names := make([]string, 0, len(state))
			for name := range state {
				names = append(names, name)
			}
			slices.Sort(names)
			for _, name := range names {
				inventory = fmt.Appendf(inventory, "%s\tinstall ok installed\t%s\tamd64\n", name, state[name])
			}
			return inventory, nil, nil
		}
		name := args[len(args)-1]
		if version, ok := state[name]; ok {
			return fmt.Appendf(nil, "%s\tinstall ok installed\t%s\tamd64\n", name, version), nil, nil
		}
		return nil, []byte("dpkg-query: no packages found matching " + name + "\n"), missingPackageError{}
	case "apt-mark":
		if slices.Equal(args, []string{"showauto"}) {
			return nil, nil, nil
		}
	case "apt-config":
		if slices.Equal(args, []string{"dump"}) {
			return []byte("APT::Get::Purge \"false\";\n"), nil, nil
		}
	case "apt-get":
		if slices.Contains(args, "--simulate") {
			var operations []byte
			count := 0
			for index, arg := range args {
				if arg != "--" {
					continue
				}
				for _, target := range args[index+1:] {
					name, _, _ := strings.Cut(target, ":")
					if version, ok := state[name]; ok {
						operations = fmt.Appendf(operations, "Remv %s [%s]\n", target, version)
						count++
					}
				}
				break
			}
			output := fmt.Appendf(nil, "0 upgraded, 0 newly installed, %d to remove and 0 not upgraded.\n", count)
			return append(output, operations...), nil, nil
		}
		log, err := os.OpenFile(filepath.Join(e.root, "transactions"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return nil, nil, err
		}
		_, writeErr := fmt.Fprintln(log, args)
		closeErr := log.Close()
		if writeErr != nil {
			return nil, nil, writeErr
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}
		remove := false
		for index, arg := range args {
			if arg == "remove" {
				remove = true
			}
			if arg != "--" {
				continue
			}
			for _, target := range args[index+1:] {
				name, version, exact := strings.Cut(target, "=")
				name, _, _ = strings.Cut(name, ":")
				name = strings.TrimSuffix(strings.TrimPrefix(name, "^"), "$")
				if strings.HasSuffix(target, ".deb") {
					name, version = dscLocalIdentity(target)
					exact = true
				}
				if remove {
					delete(state, name)
				} else {
					if !exact {
						version = "1.0"
					}
					state[name] = version
				}
			}
			break
		}
		data, err := json.Marshal(state)
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, os.WriteFile(statePath, data, 0600)
	}
	return nil, nil, fmt.Errorf("unexpected fake command: %s %q", program, args)
}

func TestDSCPackageExecutorRemoval(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "installed.json")
	if err := os.WriteFile(statePath, []byte(`{"git":"1.0","vim":"2.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	manager := newDSCPackageManager(root)
	desired := apt.Packages{
		Name:   apt.PackageNames{Values: []string{"git"}, Scalar: true},
		Ensure: "Absent",
	}
	actual, err := manager.Set(t.Context(), desired)
	if err != nil || actual.State != "Absent" {
		t.Fatalf("fake removal: %+v, error %v", actual, err)
	}
	state, err := os.ReadFile(statePath)
	if err != nil || string(state) != `{"vim":"2.0"}` {
		t.Fatalf("fake removal state = %s, error %v", state, err)
	}
	logPath := filepath.Join(root, "transactions")
	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(before, []byte("\n")) != 1 || bytes.Contains(before, []byte("--simulate")) ||
		!bytes.Contains(before, []byte("--auto-remove remove -- git:amd64")) {
		t.Fatalf("expected only final auto-removal in transaction log: %s", before)
	}
	if _, err := manager.Set(t.Context(), desired); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(logPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("idempotent fake removal changed transaction log: %s (%v)", after, err)
	}
}

func TestDSCPackagesProcess(t *testing.T) {
	root := os.Getenv("DSCPM_PACKAGES_TEST_ROOT")
	if root == "" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Exit(runPackages(t.Context(), os.Args[i+1:], os.Stdin, os.Stdout, os.Stderr, newDSCPackageManager(root)))
		}
	}
	t.Fatal("missing resource arguments")
}

func TestDSCPackagesIntegration(t *testing.T) {
	dsc, err := exec.LookPath("dsc")
	if err != nil {
		if os.Getenv("DSCPM_REQUIRE_DSC") == "1" {
			t.Fatal("DSC executable required by DSCPM_REQUIRE_DSC but not available on PATH")
		}
		t.Skip("DSC executable not available on PATH")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "manifests", "packages.dsc.resource.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"get", "test", "set"} {
		command := manifest[operation].(map[string]any)
		if command["executable"] != "dscpm" {
			t.Fatalf("unexpected %s executable", operation)
		}
		args := command["args"].([]any)
		if len(args) != 2 || args[0] != "packages" || args[1] != operation {
			t.Fatalf("unexpected %s args: %v", operation, args)
		}
		command["executable"] = executable
		command["args"] = []string{"-test.run=^TestDSCPackagesProcess$", "--", operation}
	}
	resourceDir := t.TempDir()
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resourceDir, "packages.dsc.resource.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSC_RESOURCE_PATH", resourceDir)
	const resourceType = "DscExtras.PackageManagement/packages"
	output, err := exec.CommandContext(t.Context(), dsc, "resource", "list", resourceType).CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte(resourceType)) {
		t.Fatalf("discovery: %v\n%s", err, output)
	}
	for _, input := range []string{
		`{"name":"git"}`,
		`{"name":["vim","git"],"updateCache":true}`,
		`{"name":"git","version":"2.0"}`,
		`{"name":["git","vim"],"ensure":"Absent"}`,
		`{"name":"git","ensure":"Absent"}`,
		`{"name":"$DEB"}`,
		`{"name":"$DEB","updateCache":true}`,
		`{"name":["git","$DEB"]}`,
		`{"name":["$DEB","$DEB2"]}`,
	} {
		t.Run(input, func(t *testing.T) {
			t.Setenv("DSCPM_PACKAGES_TEST_ROOT", t.TempDir())
			localOne := filepath.Join(os.Getenv("DSCPM_PACKAGES_TEST_ROOT"), "fixture-one.deb")
			localTwo := filepath.Join(os.Getenv("DSCPM_PACKAGES_TEST_ROOT"), "fixture-two.deb")
			for _, path := range []string{localOne, localTwo} {
				if err := os.WriteFile(path, []byte("metadata provided by injected executor"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			input = strings.NewReplacer("$DEB2", localTwo, "$DEB", localOne).Replace(input)
			if strings.Contains(input, `"name":[`) || strings.Contains(input, `"version"`) {
				if err := os.WriteFile(filepath.Join(os.Getenv("DSCPM_PACKAGES_TEST_ROOT"), "installed.json"),
					[]byte(`{"git":"1.0"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			localMismatch := strings.Contains(input, `"name":"`+localOne+`"`) && strings.Contains(input, `"updateCache"`)
			if localMismatch {
				if err := os.WriteFile(filepath.Join(os.Getenv("DSCPM_PACKAGES_TEST_ROOT"), "installed.json"),
					[]byte(`{"local-one":"0.5"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			invoke := func(operation string) map[string]json.RawMessage {
				t.Helper()
				command := exec.CommandContext(t.Context(), dsc, "resource", operation, "--resource", resourceType,
					"--file", "-", "--output-format", "json")
				command.Stdin = strings.NewReader(input)
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("DSC %s: %v\n%s", operation, err, output)
				}
				var result map[string]json.RawMessage
				if err := json.Unmarshal(output, &result); err != nil {
					t.Fatalf("DSC %s JSON: %v\n%s", operation, err, output)
				}
				return result
			}
			observed := invoke("get")
			if strings.Contains(input, `"name":[`) && strings.Contains(input, `"git"`) {
				var state struct{ State string }
				if err := json.Unmarshal(observed["actualState"], &state); err != nil || state.State != "Mixed" {
					t.Fatalf("partial group not Mixed: %s (%v)", observed["actualState"], err)
				}
			}
			before := invoke("test")
			wantDifferences := `["ensure"]`
			if strings.Contains(input, `"version"`) || localMismatch {
				wantDifferences = `["version"]`
			} else if input == `{"name":"git","ensure":"Absent"}` {
				wantDifferences = `[]`
			}
			if string(before["differingProperties"]) != wantDifferences {
				t.Fatalf("stateAndDiff differences: %s, want %s", before["differingProperties"], wantDifferences)
			}
			invoke("set")
			after := invoke("test")
			if string(after["inDesiredState"]) != "true" || string(after["differingProperties"]) != "[]" {
				t.Fatalf("not converged: %s", after)
			}
			var state struct {
				State          string `json:"state"`
				InDesiredState bool   `json:"_inDesiredState"`
			}
			if err := json.Unmarshal(after["actualState"], &state); err != nil || !state.InDesiredState {
				t.Fatalf("invalid actualState: %s (%v)", after["actualState"], err)
			}
			wantState := "Present"
			if strings.Contains(input, `"Absent"`) {
				wantState = "Absent"
			}
			if state.State != wantState {
				t.Fatalf("aggregate state = %s, want %s", state.State, wantState)
			}
			// A second Set must not start another transaction, including cache refresh.
			logPath := filepath.Join(os.Getenv("DSCPM_PACKAGES_TEST_ROOT"), "transactions")
			beforeLog, _ := os.ReadFile(logPath)
			if err := os.WriteFile(localOne, []byte("changed bytes, same package metadata"), 0600); err != nil {
				t.Fatal(err)
			}
			invoke("set")
			afterLog, _ := os.ReadFile(logPath)
			if !bytes.Equal(beforeLog, afterLog) {
				t.Fatalf("compliant DSC Set ran commands: %q -> %q", beforeLog, afterLog)
			}
		})
	}
	for _, input := range []string{
		`{"name":[]}`, `{"name":["git","git"]}`, `{"name":"git","ensure":"Mixed"}`,
		`{"name":["git"],"version":"1.0"}`, `{"name":"git","ensure":"Absent","version":"1.0"}`,
	} {
		command := exec.CommandContext(t.Context(), dsc, "resource", "test", "--resource", resourceType, "--file", "-")
		command.Stdin = strings.NewReader(input)
		if output, err := command.CombinedOutput(); err == nil {
			t.Fatalf("DSC accepted illegal combination %s: %s", input, output)
		}
	}
}
