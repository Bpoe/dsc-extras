package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/apt"
	"github.com/Bpoe/dsc-extras/resources/package-management/internal/packages"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

const testPublicKey = `-----BEGIN PGP PUBLIC KEY BLOCK-----

mDMEask7FRYJKwYBBAHaRw8BAQdASUQbPPk5g1WOCGZPo9Q23h5uyJfSlZgeWA1s
vAr/RjW0GWRzY2FwdCBhdXRvbWF0ZWQgdGVzdCBrZXmIkwQTFgoAOxYhBLxsjLom
ELpZUi2U2KgBEKDNrRCcBQJqyTsVAhsDBQsJCAcCAiICBhUKCQgLAgQWAgMBAh4H
AheAAAoJEKgBEKDNrRCcZ08BAL/uurviv01+D+CAQZ9tJlrsA1+F5fZUO/INIILK
TxkEAP0SyQl4tdiUJgP6HlHWZy4fJfHLAr6K34wAOxhaYXf8CQ==
=Qv6R
-----END PGP PUBLIC KEY BLOCK-----`

func makeStore(t *testing.T) apt.Store {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "var", "lib", "dpkg", "info"), 0755); err != nil {
		t.Fatal(err)
	}
	return apt.NewStore(root)
}

func call(t *testing.T, store apt.Store, operation, input string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	args := []string{"aptRepositories", operation}
	if strings.HasPrefix(operation, "-") {
		args = []string{operation}
	}
	code := run(args, strings.NewReader(input), &stdout, &stderr, store)
	return code, stdout.String(), stderr.String()
}

func TestVersion(t *testing.T) {
	code, stdout, stderr := call(t, makeStore(t), "--version", "not JSON")
	if code != 0 || stdout != version+"\n" || stderr != "" {
		t.Fatalf("--version returned (%d, %q, %q), want (0, %q, \"\")", code, stdout, stderr, version+"\n")
	}
}

func TestDispatch(t *testing.T) {
	for _, args := range [][]string{
		nil, {"get"}, {"packages", "get"}, {"dnfRepositories", "get"},
		{"aptRepositories"}, {"aptRepositories", "remove"}, {"aptRepositories", "get", "extra"},
	} {
		var stdout, stderr strings.Builder
		code := run(args, strings.NewReader("{}"), &stdout, &stderr, makeStore(t))
		if code == 0 || stdout.Len() != 0 || !json.Valid([]byte(stderr.String())) {
			t.Fatalf("run(%v) = (%d, %q, %q), want JSON error only", args, code, stdout.String(), stderr.String())
		}
	}
	for _, flag := range []string{"--help", "-h"} {
		code, stdout, stderr := call(t, makeStore(t), flag, "not JSON")
		if code != 0 || stderr != "" || !strings.Contains(stdout, "dscpm") ||
			!strings.Contains(stdout, "aptRepositories") {
			t.Fatalf("%s = (%d, %q, %q)", flag, code, stdout, stderr)
		}
	}
}

func TestManifestsShareImplementation(t *testing.T) {
	var manifests []map[string]any
	for _, name := range []string{"aptRepositories", "dscapt"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "manifests", name+".dsc.resource.json"))
		if err != nil {
			t.Fatal(err)
		}
		var manifest map[string]any
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		for _, operation := range []string{"get", "test", "set"} {
			command := manifest[operation].(map[string]any)
			if command["executable"] != "dscpm" || command["input"] != "stdin" ||
				!reflect.DeepEqual(command["args"], []any{"aptRepositories", operation}) {
				t.Fatalf("%s %s does not invoke the shared handler: %v", name, operation, command)
			}
		}
		export := manifest["export"].(map[string]any)
		if export["executable"] != "dscpm" || export["input"] != "stdin" ||
			!reflect.DeepEqual(export["args"], []any{"aptRepositories", "export"}) {
			t.Fatalf("%s export does not invoke the shared handler: %v", name, export)
		}
		schema := manifest["schema"].(map[string]any)["embedded"].(map[string]any)
		exportName := schema["properties"].(map[string]any)["_name"].(map[string]any)
		if exportName["type"] != "string" || exportName["readOnly"] != true {
			t.Fatalf("%s must declare _name as a read-only string: %v", name, exportName)
		}
		if manifest["test"].(map[string]any)["return"] != "stateAndDiff" ||
			manifest["set"].(map[string]any)["return"] != "state" ||
			manifest["set"].(map[string]any)["implementsPretest"] != true {
			t.Fatalf("%s changed DSC operation contracts", name)
		}
		manifests = append(manifests, manifest)
	}
	if manifests[0]["type"] != "DscExtras.PackageManagement/aptRepositories" ||
		manifests[1]["type"] != "DscExtras.Apt/Repository" {
		t.Fatal("unexpected resource types")
	}
	delete(manifests[0], "type")
	delete(manifests[1], "type")
	if !reflect.DeepEqual(manifests[0], manifests[1]) {
		t.Fatal("new and compatibility manifests differ beyond resource type")
	}
}

func TestAPTRepositoryExportCLI(t *testing.T) {
	store := makeStore(t)
	source := filepath.Join(store.Root, "etc", "apt", "sources.list.d", "manual.list")
	if err := os.MkdirAll(filepath.Dir(source), 0755); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(store.Root, "etc", "apt", "keyrings", "manual.asc")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(testPublicKey), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("deb [arch=amd64 signed-by=/etc/apt/keyrings/manual.asc] https://repo.example/debian stable main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := call(t, store, "export", "")
	var item struct {
		apt.Repository
		ExportName string `json:"_name"`
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if code != 0 || stderr != "" || len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &item) != nil ||
		item.Ensure != "Present" || item.URI != "https://repo.example/debian" || item.Suite != "stable" ||
		item.ExportName == "" || item.ExportName != item.Name {
		t.Fatalf("invalid repository JSON Lines: %d %q %q", code, stdout, stderr)
	}
	filter := `{"name":"` + item.Name + `","uri":"` + item.URI + `","suite":"stable"}`
	code, filtered, stderr := call(t, store, "export", filter)
	if code != 0 || stderr != "" || strings.Count(filtered, "\n") != 1 {
		t.Fatalf("filtered repository export: %d %q %q", code, filtered, stderr)
	}
	after, err := os.ReadFile(source)
	if err != nil || string(before) != string(after) {
		t.Fatalf("Export modified source: %v", err)
	}
}

type exportPackagesFixture struct{ items []packages.Packages }

func (f exportPackagesFixture) Get(context.Context, packages.Packages) (packages.Packages, error) {
	return packages.Packages{}, nil
}
func (f exportPackagesFixture) Test(context.Context, packages.Packages) (packages.Packages, []string, error) {
	return packages.Packages{}, nil, nil
}
func (f exportPackagesFixture) Set(context.Context, packages.Packages) (packages.Packages, error) {
	return packages.Packages{}, nil
}
func (f exportPackagesFixture) Export(_ context.Context, filter packages.Packages) ([]packages.Packages, error) {
	if len(filter.Name.Values) == 0 {
		return f.items, nil
	}
	var result []packages.Packages
	for _, item := range f.items {
		for _, name := range filter.Name.Values {
			if item.Name.Values[0] == name {
				result = append(result, item)
			}
		}
	}
	return result, nil
}

func TestPackagesExportCLIUsesJSONLinesAndFilter(t *testing.T) {
	manager := exportPackagesFixture{items: []packages.Packages{
		{Name: packages.PackageNames{Values: []string{"alpha"}, Scalar: true}, Version: "1.0"},
		{Name: packages.PackageNames{Values: []string{"beta:arm64"}, Scalar: true}, Version: "2.0"},
		{Name: packages.PackageNames{Values: []string{"libstdc++6:amd64"}, Scalar: true}, Version: "3.0"},
	}}
	exportNames := map[string]string{
		"alpha":            "alpha",
		"beta:arm64":       "beta%3Aarm64",
		"libstdc++6:amd64": "libstdc%2B%2B6%3Aamd64",
	}
	for _, test := range []struct {
		input string
		lines int
	}{
		{"", 3},
		{`{"name":"beta:arm64"}`, 1},
		{`{"name":["beta:arm64","libstdc++6:amd64"]}`, 2},
		{`{"name":"missing"}`, 0},
	} {
		var stdout, stderr strings.Builder
		code := runWithPackages(context.Background(), []string{"packages", "export"},
			strings.NewReader(test.input), &stdout, &stderr, apt.NewStore(t.TempDir()), manager)
		output := strings.TrimSuffix(stdout.String(), "\n")
		if test.lines == 0 {
			if code != 0 || output != "" || stderr.Len() != 0 {
				t.Fatalf("empty export = %d %q %q", code, output, stderr.String())
			}
			continue
		}
		lines := strings.Split(output, "\n")
		if code != 0 || stderr.Len() != 0 || len(lines) != test.lines {
			t.Fatalf("export = %d %q %q", code, output, stderr.String())
		}
		for _, line := range lines {
			var item struct {
				Name       string `json:"name"`
				ExportName string `json:"_name"`
			}
			if err := json.Unmarshal([]byte(line), &item); err != nil ||
				item.Name == "" || item.ExportName == "" || item.ExportName != exportNames[item.Name] {
				t.Fatalf("invalid export name in JSONL line %q: %v", line, err)
			}
		}
	}
}

func TestPackagesManifestDeclaresExport(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "manifests", "packages.dsc.resource.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	export := manifest["export"].(map[string]any)
	if export["executable"] != "dscpm" || export["input"] != "stdin" ||
		!reflect.DeepEqual(export["args"], []any{"packages", "export"}) {
		t.Fatalf("invalid package Export declaration: %s", data)
	}
	schema := manifest["schema"].(map[string]any)["embedded"].(map[string]any)
	exportName := schema["properties"].(map[string]any)["_name"].(map[string]any)
	if exportName["type"] != "string" || exportName["readOnly"] != true {
		t.Fatalf("packages must declare _name as a read-only string: %v", exportName)
	}
}

func decodeTestOutput(t *testing.T, output string) (testResult, []string) {
	t.Helper()
	lines := strings.Split(output, "\n")
	if len(lines) != 3 || lines[2] != "" {
		t.Fatalf("Test must emit exactly two newline-delimited JSON values: %q", output)
	}
	var state struct {
		apt.Repository
		InDesiredState *bool `json:"_inDesiredState"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &state); err != nil || state.InDesiredState == nil {
		t.Fatalf("invalid Test state: %q, %v", lines[0], err)
	}
	var differences []string
	if err := json.Unmarshal([]byte(lines[1]), &differences); err != nil || differences == nil {
		t.Fatalf("Test differences must be an array, not null: %q, %v", lines[1], err)
	}
	return testResult{Repository: state.Repository, InDesiredState: *state.InDesiredState}, differences
}

func externalRepository(t *testing.T, store apt.Store, extension string) apt.Repository {
	t.Helper()
	key := []byte(testPublicKey)
	if extension == ".gpg" {
		block, err := armor.Decode(strings.NewReader(testPublicKey))
		if err != nil {
			t.Fatal(err)
		}
		key, err = io.ReadAll(block.Body)
		if err != nil {
			t.Fatal(err)
		}
	}
	for path, contents := range map[string][]byte{
		filepath.Join(store.Root, "usr", "share", "keyrings", "external"+extension): key,
		filepath.Join(store.Root, "etc", "apt", "sources.list.d", "external.sources"): []byte(
			"Types: deb\nURIs: https://packages.example.org/debian/\nSuites: stable\n" +
				"Components: contrib main\nArchitectures: arm64 amd64\n" +
				"Signed-By: /usr/share/keyrings/external" + extension + "\nEnabled: yes\n"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return apt.Repository{
		Name:          "management-label",
		Ensure:        "Present",
		URI:           "https://packages.example.org/debian",
		Suite:         "stable",
		Components:    []string{"main", "contrib"},
		Architectures: []string{"amd64", "arm64"},
		SigningKey:    strings.Replace(testPublicKey, "\n\n", "\nComment: desired armor\n\n", 1),
	}
}

func differentSigningKey(t *testing.T) string {
	t.Helper()
	key, err := os.ReadFile(filepath.Join("..", "..", "internal", "apt", "testdata", "docker.asc"))
	if err != nil {
		t.Fatal(err)
	}
	return string(key)
}

func TestGetAcceptsLogicalSelectorAndReturnsAbsentState(t *testing.T) {
	store := makeStore(t)
	code, stdout, stderr := call(t, store, "get", `{"name":"example","uri":"https://packages.example.org/debian","suite":"stable"}`)
	if code != 0 || stderr != "" {
		t.Fatalf("Get returned (%d, %q), want success", code, stderr)
	}
	var actual apt.Repository
	if err := json.Unmarshal([]byte(stdout), &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Name != "example" || actual.Ensure != "Absent" {
		t.Fatalf("Get returned %#v, want absent state", actual)
	}
}

func TestSetGetAndTestUseDSCJSONContract(t *testing.T) {
	store := makeStore(t)
	input, err := json.Marshal(apt.Repository{
		Name:       "example",
		URI:        "https://packages.example.org/debian",
		Suite:      "stable",
		Components: []string{"main"},
		SigningKey: testPublicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := call(t, store, "set", string(input))
	if code != 0 || stderr != "" {
		t.Fatalf("Set returned (%d, %q)", code, stderr)
	}
	var actual apt.Repository
	if err := json.Unmarshal([]byte(stdout), &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Ensure != "Present" || actual.SigningKey == "" {
		t.Fatalf("Set returned incomplete state: %#v", actual)
	}

	code, stdout, stderr = call(t, store, "test", string(input))
	if code != 0 || stderr != "" {
		t.Fatalf("Test returned (%d, %q)", code, stderr)
	}
	testOutput, differences := decodeTestOutput(t, stdout)
	if !testOutput.InDesiredState || len(differences) != 0 {
		t.Fatalf("Test = (%t, %v), want compliant with no differences", testOutput.InDesiredState, differences)
	}
	if !reflect.DeepEqual(testOutput.Repository, actual) {
		t.Fatalf("Test state differs from Set: %+v, want %+v", testOutput.Repository, actual)
	}

	code, stdout, stderr = call(t, store, "get", `{"name":"example","uri":"https://packages.example.org/debian","suite":"stable"}`)
	if code != 0 || stderr != "" {
		t.Fatalf("Get returned (%d, %q)", code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &actual); err != nil {
		t.Fatal(err)
	}
	if actual.URI != "https://packages.example.org/debian" || actual.SigningKey == "" {
		t.Fatalf("Get returned incorrect current state: %#v", actual)
	}
}

func TestStateAndDiffUsesSemanticComparison(t *testing.T) {
	disabled := false
	otherKey := differentSigningKey(t)
	for _, extension := range []string{".gpg", ".asc"} {
		for _, test := range []struct {
			name   string
			change func(*apt.Repository)
			want   []string
		}{
			{name: "equivalent key armor URI and unordered sets"},
			{name: "default ensure", change: func(desired *apt.Repository) { desired.Ensure = "" }},
			{name: "different key", change: func(desired *apt.Repository) { desired.SigningKey = otherKey }, want: []string{"signingKey"}},
			{name: "components", change: func(desired *apt.Repository) { desired.Components = []string{"main"} }, want: []string{"components"}},
			{name: "architectures", change: func(desired *apt.Repository) { desired.Architectures = []string{"amd64"} }, want: []string{"architectures"}},
			{name: "enabled", change: func(desired *apt.Repository) { desired.Enabled = &disabled }, want: []string{"enabled"}},
			{name: "multiple differences", change: func(desired *apt.Repository) {
				desired.Components = []string{"main"}
				desired.Architectures = nil
				desired.SigningKey = otherKey
				desired.Enabled = &disabled
			}, want: []string{"components", "architectures", "signingKey", "enabled"}},
			{name: "missing", change: func(desired *apt.Repository) { desired.URI += "/missing" }, want: []string{"ensure"}},
			{name: "removal required", change: func(desired *apt.Repository) {
				desired.Ensure, desired.SigningKey = "Absent", ""
			}, want: []string{"ensure"}},
			{name: "already absent", change: func(desired *apt.Repository) {
				desired.URI += "/missing"
				desired.Ensure, desired.SigningKey = "Absent", ""
			}},
		} {
			t.Run(extension+"/"+test.name, func(t *testing.T) {
				store := makeStore(t)
				desired := externalRepository(t, store, extension)
				if test.change != nil {
					test.change(&desired)
				}
				input, err := json.Marshal(desired)
				if err != nil {
					t.Fatal(err)
				}
				code, stdout, stderr := call(t, store, "test", string(input))
				if code != 0 || stderr != "" {
					t.Fatalf("Test returned (%d, %q)", code, stderr)
				}
				output, differences := decodeTestOutput(t, stdout)
				if !slices.Equal(differences, test.want) || output.InDesiredState != (len(test.want) == 0) {
					t.Fatalf("Test = (%t, %v), want differences %v", output.InDesiredState, differences, test.want)
				}
				actual, storeDifferences, err := store.Test(desired)
				if err != nil || storeDifferences == nil || !slices.Equal(storeDifferences, differences) ||
					!reflect.DeepEqual(actual, output.Repository) {
					t.Fatalf("Store.Test disagrees with command: (%+v, %v, %v)", actual, storeDifferences, err)
				}
				observed, err := store.Get(desired)
				if err != nil || !reflect.DeepEqual(observed, output.Repository) {
					t.Fatalf("Test did not preserve Get state: (%+v, %v)", observed, err)
				}
				if len(test.want) == 0 && actual.Ensure == "Present" && actual.SigningKey == desired.SigningKey {
					t.Fatal("regression requires different actual and desired armor strings")
				}
				if err := apt.ValidateDesired(&desired); err != nil {
					t.Fatal(err)
				}
				if apt.Equal(desired, actual) != output.InDesiredState ||
					!slices.Equal(apt.DifferingProperties(desired, actual), differences) {
					t.Fatal("semantic comparison disagrees with command compliance or differences")
				}
			})
		}
	}
}

func TestCLIRejectsInvalidJSONAndUnknownOperation(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation string
		input     string
	}{
		{name: "invalid JSON", operation: "get", input: "{"},
		{name: "unknown operation", operation: "remove", input: `{"name":"example"}`},
		{name: "unsafe name", operation: "get", input: `{"name":"../escape"}`},
		{name: "filename-only selector", operation: "get", input: `{"name":"example"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := call(t, makeStore(t), test.operation, test.input)
			if code == 0 || stdout != "" || stderr == "" {
				t.Fatalf("run()=(%d,%q,%q), want error and empty stdout", code, stdout, stderr)
			}
		})
	}
}
