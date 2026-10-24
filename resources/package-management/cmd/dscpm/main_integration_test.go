package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

func TestDSCPackagesExportIntegration(t *testing.T) {
	store := makeStore(t)
	t.Setenv("DSCAPT_TEST_ROOT", store.Root)
	dsc, resourceType := prepareDSCResource(t, "packages.dsc.resource.json")
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	for _, args := range [][]string{
		{"resource", "export", "--resource", resourceType, "--output-format", "json"},
		{"resource", "get", "--all", "--resource", resourceType, "--output-format", "json"},
	} {
		output, err := exec.CommandContext(ctx, dsc, args...).Output()
		if err != nil {
			t.Fatalf("DSC %s failed to consume package Export: %v\n%s", strings.Join(args, " "), err, output)
		}
		decoder := json.NewDecoder(bytes.NewReader(output))
		for {
			var instance map[string]json.RawMessage
			err := decoder.Decode(&instance)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("DSC %s returned invalid JSON/JSON Lines: %v\n%s", strings.Join(args, " "), err, output)
			}
		}
	}
}

func TestDSCRepositoryExportFilterIntegration(t *testing.T) {
	for _, manifest := range []string{"aptRepositories.dsc.resource.json", "dscapt.dsc.resource.json"} {
		t.Run(manifest, func(t *testing.T) {
			dsc, resourceType := prepareDSCResource(t, manifest)
			for _, source := range []string{"external.sources", "docker.sources", "sources.list"} {
				t.Run(source, func(t *testing.T) {
					store := makeStore(t)
					desired := externalRepository(t, store, ".gpg")
					path := filepath.Join(store.Root, "etc", "apt", "sources.list.d", "external.sources")
					content, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if source == "sources.list" {
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
						path = filepath.Join(store.Root, "etc", "apt", source)
						content = []byte("deb [arch=arm64,amd64 signed-by=/usr/share/keyrings/external.gpg] https://packages.example.org/debian/ stable contrib main\n")
						if err := os.WriteFile(path, content, 0644); err != nil {
							t.Fatal(err)
						}
					} else if source != "external.sources" {
						newPath := filepath.Join(filepath.Dir(path), source)
						if err := os.Rename(path, newPath); err != nil {
							t.Fatal(err)
						}
						path = newPath
					}
					t.Setenv("DSCAPT_TEST_ROOT", store.Root)
					for _, label := range []string{"docker", "management-label"} {
						input, err := json.Marshal(map[string]string{"name": label, "uri": desired.URI, "suite": desired.Suite})
						if err != nil {
							t.Fatal(err)
						}
						command := exec.CommandContext(t.Context(), dsc, "resource", "export", "--resource", resourceType,
							"--file", "-", "--output-format", "json")
						command.Stdin = bytes.NewReader(input)
						var stderr strings.Builder
						command.Stderr = &stderr
						output, err := command.Output()
						if err != nil {
							t.Fatalf("DSC Export filter %s: %v\n%s\n%s", input, err, output, stderr.String())
						}
						var config struct {
							Resources []struct {
								Properties apt.Repository
							}
						}
						if err := json.Unmarshal(output, &config); err != nil || len(config.Resources) != 1 {
							t.Fatalf("filtered Export = %s, %v", output, err)
						}
						exported := config.Resources[0].Properties
						if err := apt.ValidateDesired(&exported); err != nil {
							t.Fatalf("Export is not reusable desired state: %+v, %v", exported, err)
						}
						if _, diff, err := store.Test(exported); err != nil || len(diff) != 0 {
							t.Fatalf("Export differs from existing source: %v, %v", diff, err)
						}
					}
					if after, err := os.ReadFile(path); err != nil || !bytes.Equal(content, after) {
						t.Fatalf("Export modified source: %v", err)
					}
				})
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
			t.Setenv("DSCPM_REQUIRE_DSC", required)
			command := exec.CommandContext(t.Context(), executable,
				"-test.run=^TestDSCStateAndDiffIntegration$", "-test.v")
			output, err := command.CombinedOutput()
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

func testDSCStateAndDiffIntegration(t *testing.T, manifestName string) {
	dsc, resourceType := prepareDSCResource(t, manifestName)
	testDSCOperations(t, dsc, resourceType)
}

func prepareDSCResource(t *testing.T, manifestName string) (string, string) {
	t.Helper()
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
	for _, operation := range []string{"get", "test", "set", "export"} {
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
	return dsc, resourceType
}

func testDSCOperations(t *testing.T, dsc, resourceType string) {
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
			for _, args := range [][]string{
				{"resource", "export", "--resource", resourceType, "--output-format", "json"},
				{"resource", "get", "--all", "--resource", resourceType, "--output-format", "json"},
			} {
				output, err := exec.CommandContext(t.Context(), dsc, args...).Output()
				if err != nil || !json.Valid(output) || !bytes.Contains(output, []byte("packages.example.org")) {
					t.Fatalf("DSC %s failed to consume repository Export: %v\n%s", strings.Join(args, " "), err, output)
				}
			}
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
				var stderr strings.Builder
				command.Stderr = &stderr
				output, err := command.Output()
				if err != nil {
					t.Fatalf("DSC resource %s failed: %v\n%s\n%s", operation, err, output, stderr.String())
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

func TestDSCConfigCorrectsDockerRepositoryAndConverges(t *testing.T) {
	dsc, resourceType := prepareDSCResource(t, "aptRepositories.dsc.resource.json")
	store := makeStore(t)
	t.Setenv("DSCAPT_TEST_ROOT", store.Root)
	key, err := os.ReadFile(filepath.Join("..", "..", "internal", "apt", "testdata", "docker.asc"))
	if err != nil {
		t.Fatal(err)
	}
	docker := apt.Repository{Name: "docker", Ensure: "Present", URI: "https://download.docker.com/linux/debian",
		Suite: "noble", Components: []string{"stable"}, Architectures: []string{"amd64"}, SigningKey: string(key)}
	other := apt.Repository{Name: "other", Ensure: "Present", URI: "https://unrelated.example", Suite: "stable",
		Components: []string{"main"}, SigningKey: testPublicKey}
	configSet := func() {
		t.Helper()
		config := map[string]any{
			"$schema": "https://aka.ms/dsc/schemas/v3/bundled/config/document.json",
			"resources": []map[string]any{
				{"name": "Docker", "type": resourceType, "properties": docker},
				{"name": "Other", "type": resourceType, "properties": other},
			},
		}
		input, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, dsc, "config", "set", "--file", "-", "--output-format", "json")
		command.Stdin = bytes.NewReader(input)
		var stderr strings.Builder
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			t.Fatalf("dsc config set: %v\n%s\n%s", err, output, stderr.String())
		}
		var result struct {
			HadErrors bool              `json:"hadErrors"`
			Results   []json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(output, &result); err != nil || result.HadErrors || len(result.Results) != 2 {
			t.Fatalf("configuration did not apply both repositories: %s (%v)", output, err)
		}
	}
	sourcePath := filepath.Join(store.Root, "etc", "apt", "sources.list.d", "docker.sources")
	otherPath := filepath.Join(store.Root, "etc", "apt", "sources.list.d", "other.sources")
	configSet()
	initial, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Contains(initial, []byte("URIs: "+docker.URI+"\nSuites: noble\n")) {
		t.Fatalf("incorrect initial Docker definition was not reproduced: %s (%v)", initial, err)
	}
	otherBefore, err := os.Stat(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	docker.URI = "https://download.docker.com/linux/ubuntu"
	docker.Suite = "jammy"
	before, differences, err := store.Test(docker)
	if err != nil || before.URI != "https://download.docker.com/linux/debian" ||
		!slices.Equal(differences, []string{"uri", "suite"}) {
		t.Fatalf("corrected configuration must detect actual drift: %+v, %v, %v", before, differences, err)
	}
	configSet()
	corrected, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Contains(corrected, []byte("URIs: "+docker.URI+"\nSuites: jammy\n")) {
		t.Fatalf("Docker definition was not corrected: %s (%v)", corrected, err)
	}
	input, err := json.Marshal(docker)
	if err != nil {
		t.Fatal(err)
	}
	code, output, stderr := call(t, store, "test", string(input))
	if code != 0 || stderr != "" {
		t.Fatalf("resource test failed: %d, %s", code, stderr)
	}
	state, differences := decodeTestOutput(t, output)
	if !state.InDesiredState || len(differences) != 0 {
		t.Fatalf("corrected Docker repository is not compliant: %s", output)
	}
	paths := []string{sourcePath, otherPath,
		filepath.Join(store.Root, "etc", "apt", "keyrings", "docker.gpg"),
		filepath.Join(store.Root, "etc", "apt", "keyrings", "other.gpg")}
	infos, contents := map[string]os.FileInfo{}, map[string][]byte{}
	for _, path := range paths {
		infos[path], err = os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		contents[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !os.SameFile(otherBefore, infos[otherPath]) || !otherBefore.ModTime().Equal(infos[otherPath].ModTime()) {
		t.Fatal("correcting Docker changed the unrelated repository")
	}
	configSet()
	for _, path := range paths {
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(contents[path], data) || !os.SameFile(infos[path], after) ||
			!infos[path].ModTime().Equal(after.ModTime()) || infos[path].Mode() != after.Mode() {
			t.Fatalf("repeated config set rewrote %s: %v", path, err)
		}
	}
}
