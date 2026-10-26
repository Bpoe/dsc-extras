package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-extras/resources/linux/internal/groups"
	"github.com/Bpoe/dsc-extras/resources/linux/internal/services"
)

func pointer[T any](value T) *T { return &value }

type fakeManager struct {
	actual services.Unit
	err    error
	sets   int
}

func newFakeManager() *fakeManager {
	return &fakeManager{actual: services.Unit{
		Name: "example", Scope: "system", State: pointer("inactive"),
		Enabled: pointer(false), Masked: pointer(false), ActiveState: "inactive",
		SubState: "dead", LoadState: "loaded", UnitFileState: "disabled",
	}}
}

func (f *fakeManager) Get(_ context.Context, desired services.Unit) (services.Unit, error) {
	actual := f.actual
	actual.Name = desired.Name
	if desired.Scope != "" {
		actual.Scope = desired.Scope
	}
	return actual, f.err
}

func (f *fakeManager) Export(_ context.Context, filter services.Unit) ([]services.Unit, error) {
	if filter.Name != "" && filter.Name != f.actual.Name {
		return []services.Unit{}, nil
	}
	actual := f.actual
	if actual.Name == "" {
		actual = newFakeManager().actual
	}
	if filter.Scope != "" {
		actual.Scope = filter.Scope
	}
	return []services.Unit{actual}, f.err
}

func (f *fakeManager) Test(ctx context.Context, desired services.Unit) (services.Unit, []string, error) {
	actual, err := f.Get(ctx, desired)
	differences := []string{}
	if desired.State != nil && *desired.State != actual.ActiveState {
		differences = append(differences, "state")
	}
	if desired.Enabled != nil && *desired.Enabled != *actual.Enabled {
		differences = append(differences, "enabled")
	}
	if desired.Masked != nil && *desired.Masked != *actual.Masked {
		differences = append(differences, "masked")
	}
	return actual, differences, err
}

func (f *fakeManager) Set(ctx context.Context, desired services.Unit) (services.Unit, error) {
	f.sets++
	if desired.State != nil {
		f.actual.State, f.actual.ActiveState = desired.State, *desired.State
	}
	if desired.Enabled != nil {
		f.actual.Enabled = desired.Enabled
	}
	if desired.Masked != nil {
		f.actual.Masked = desired.Masked
	}
	return f.Get(ctx, desired)
}

func call(t *testing.T, manager serviceManager, args []string, input string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run(t.Context(), args, strings.NewReader(input), &stdout, &stderr, manager, nil)
	return code, stdout.String(), stderr.String()
}

func TestFlagsAndDispatch(t *testing.T) {
	for _, args := range [][]string{nil, {"get"}, {"users", "get"}, {"services"}, {"services", "restart"}, {"services", "get", "extra"}} {
		code, stdout, stderr := call(t, newFakeManager(), args, `{}`)
		if code == 0 || stdout != "" || !json.Valid([]byte(stderr)) {
			t.Fatalf("%v = %d %q %q", args, code, stdout, stderr)
		}
	}
	for _, flag := range []string{"--help", "-h", "--version"} {
		code, stdout, stderr := call(t, newFakeManager(), []string{flag}, "not JSON")
		if code != 0 || stderr != "" || (flag == "--version" && stdout != version+"\n") ||
			(flag != "--version" && !strings.Contains(stdout, "Resources: services")) {
			t.Fatalf("%s = %d %q %q", flag, code, stdout, stderr)
		}
	}
}

func TestCLIContract(t *testing.T) {
	manager := newFakeManager()
	for _, operation := range []string{"get", "test", "set", "test"} {
		code, stdout, stderr := call(t, manager, []string{"services", operation},
			`{"name":"example","state":"active","enabled":true,"masked":true,"futureProperty":{"ignored":true}}`)
		if code != 0 || stderr != "" {
			t.Fatalf("%s: %d %s", operation, code, stderr)
		}
		decoder := json.NewDecoder(strings.NewReader(stdout))
		var actual testResult
		if err := decoder.Decode(&actual); err != nil || actual.Name != "example" {
			t.Fatalf("%s state: %s %v", operation, stdout, err)
		}
		if operation == "test" {
			var differences []string
			if err := decoder.Decode(&differences); err != nil || differences == nil {
				t.Fatalf("Test must emit a second JSON array: %s %v", stdout, err)
			}
			want := []string{}
			if manager.sets == 0 {
				want = []string{"state", "enabled", "masked"}
			}
			if !slices.Equal(differences, want) || actual.InDesiredState != (len(want) == 0) {
				t.Fatalf("stateAndDiff = %s; want %v", stdout, want)
			}
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			t.Fatalf("extra output: %s", stdout)
		}
	}
}

func TestServiceExportWritesJSONLinesAndSupportsIdentityFilter(t *testing.T) {
	manager := newFakeManager()
	for _, test := range []struct {
		input      string
		count      int
		exportName string
	}{
		{"", 1, "example"},
		{`{"name":"example","scope":"system"}`, 1, "example"},
		{`{"name":"example","scope":"user"}`, 1, "example%2Cuser"},
		{`{"name":"missing"}`, 0, ""},
	} {
		code, stdout, stderr := call(t, manager, []string{"services", "export"}, test.input)
		lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
		if test.count == 0 {
			if stdout != "" || code != 0 || stderr != "" {
				t.Fatalf("empty Export = %d %q %q", code, stdout, stderr)
			}
			continue
		}
		var actual struct {
			services.Unit
			ExportName string `json:"_name"`
		}
		if code != 0 || stderr != "" || len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &actual) != nil ||
			actual.Name != "example" || actual.ExportName != test.exportName {
			t.Fatalf("invalid JSON Lines Export = %d %q %q", code, stdout, stderr)
		}
	}
}

func TestExportNamesAreURLEncoded(t *testing.T) {
	for _, test := range []struct {
		name    string
		encoded string
	}{
		{"safe-name_1.service", "safe-name_1.service"},
		{"AZaz09-._~", "AZaz09-._~"},
		{"example@unit:instance+.service", "example%40unit%3Ainstance%2B.service"},
		{"space name", "space%20name"},
		{"space+name", "space%2Bname"},
		{"literal%2Fname", "literal%252Fname"},
		{`literal\x2d.service`, "literal%5Cx2d.service"},
		{"caf\u00e9/ops?#&=", "caf%C3%A9%2Fops%3F%23%26%3D"},
	} {
		for _, resource := range []struct {
			name       string
			items      any
			exportName string
		}{
			{"services-system", []services.Unit{{Name: test.name, Scope: "system"}}, test.encoded},
			{"services-default", []services.Unit{{Name: test.name}}, test.encoded},
			{"services-user", []services.Unit{{Name: test.name, Scope: "user"}}, test.encoded + "%2Cuser"},
			{"groups", []groups.Group{{Name: test.name}}, test.encoded},
		} {
			t.Run(resource.name+"/"+test.name, func(t *testing.T) {
				var stdout, stderr strings.Builder
				code := writeJSONLines(&stdout, resource.items, &stderr)
				var actual struct {
					Name       string `json:"name"`
					ExportName string `json:"_name"`
				}
				if code != 0 || stderr.Len() != 0 || json.Unmarshal([]byte(stdout.String()), &actual) != nil ||
					actual.Name != test.name || actual.ExportName != resource.exportName {
					t.Fatalf("Export = %d %q %q; want name %q and _name %q",
						code, stdout.String(), stderr.String(), test.name, resource.exportName)
				}
			})
		}
	}
}

func TestInputAndErrors(t *testing.T) {
	for _, input := range []string{"", "{", "[]", `{"name":"a"} {}`, `{"name":"a","enabled":"true"}`, strings.Repeat(" ", maxInputBytes+1)} {
		code, stdout, stderr := call(t, newFakeManager(), []string{"services", "get"}, input)
		if code == 0 || stdout != "" || !json.Valid([]byte(stderr)) {
			t.Fatalf("invalid input returned %d %q %q", code, stdout, stderr)
		}
	}
	for _, input := range []string{`{}`, `null`, `{"name":"../bad"}`, `{"name":"a","state":"failed"}`, `{"name":"a","scope":"global"}`} {
		code, stdout, stderr := call(t, services.NewManager(nil, ""), []string{"services", "get"}, input)
		if code == 0 || stdout != "" || !json.Valid([]byte(stderr)) {
			t.Fatalf("invalid properties returned %d %q %q", code, stdout, stderr)
		}
	}
	manager := newFakeManager()
	manager.err = errors.New("Permission denied")
	for _, operation := range []string{"get", "test", "set"} {
		code, stdout, stderr := call(t, manager, []string{"services", operation}, `{"name":"example"}`)
		if code == 0 || stdout != "" || !strings.Contains(stderr, "Permission denied") {
			t.Fatalf("failure hidden: %d %q %q", code, stdout, stderr)
		}
	}
}

func TestManifest(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "manifests", "services.dsc.resource.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Type    string
		Version string
		Export  struct {
			Executable string
			Args       []string
			Input      string
		}
		Get struct {
			Executable string
			Args       []string
			Input      string
		}
		Test struct {
			Executable string
			Args       []string
			Input      string
			Return     string
		}
		Set struct {
			Executable        string
			Args              []string
			Input             string
			Return            string
			ImplementsPretest bool
		}
		Schema struct {
			Embedded struct {
				Required             []string
				AdditionalProperties bool
				Properties           map[string]map[string]any
			}
		}
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Type != "DscExtras.Linux/services" || manifest.Version != version ||
		manifest.Export.Executable != "dsclinux" ||
		!slices.Equal(manifest.Export.Args, []string{"services", "export"}) || manifest.Export.Input != "stdin" ||
		manifest.Get.Executable != "dsclinux" || manifest.Test.Executable != "dsclinux" || manifest.Set.Executable != "dsclinux" ||
		!slices.Equal(manifest.Get.Args, []string{"services", "get"}) ||
		!slices.Equal(manifest.Test.Args, []string{"services", "test"}) ||
		!slices.Equal(manifest.Set.Args, []string{"services", "set"}) ||
		manifest.Get.Input != "stdin" || manifest.Test.Input != "stdin" || manifest.Set.Input != "stdin" ||
		manifest.Test.Return != "stateAndDiff" || manifest.Set.Return != "state" || !manifest.Set.ImplementsPretest {
		t.Fatalf("incorrect resource contract: %s", data)
	}
	schema := manifest.Schema.Embedded
	if !schema.AdditionalProperties || !slices.Equal(schema.Required, []string{"name"}) ||
		schema.Properties["name"]["x-dsc-key"] != true ||
		!reflect.DeepEqual(schema.Properties["state"]["enum"], []any{"active", "inactive"}) ||
		schema.Properties["scope"]["default"] != "system" {
		t.Fatal("incorrect resource schema")
	}
	for _, property := range []string{"state", "enabled", "masked"} {
		if _, ok := schema.Properties[property]["default"]; ok {
			t.Fatalf("%s must be unmanaged when omitted", property)
		}
	}
	if schema.Properties["_name"]["type"] != "string" {
		t.Fatal("_name must be a string")
	}
	for _, property := range []string{"activeState", "subState", "loadState", "unitFileState", "_name", "_inDesiredState"} {
		if schema.Properties[property]["readOnly"] != true {
			t.Fatalf("%s must be read-only", property)
		}
	}
}
