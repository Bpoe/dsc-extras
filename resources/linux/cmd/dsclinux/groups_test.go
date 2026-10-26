package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-extras/resources/linux/internal/groups"
)

type fakeGroupManager struct {
	actual groups.Group
	err    error
	sets   int
}

func newFakeGroupManager() *fakeGroupManager {
	return &fakeGroupManager{actual: groups.Group{Name: "dev", Ensure: "Absent"}}
}

func (f *fakeGroupManager) Get(_ context.Context, desired groups.Group) (groups.Group, error) {
	actual := f.actual
	actual.Name = desired.Name
	return actual, f.err
}

func (f *fakeGroupManager) Export(_ context.Context, filter groups.Group) ([]groups.Group, error) {
	if filter.Name != "" && filter.Name != f.actual.Name {
		return []groups.Group{}, nil
	}
	actual := f.actual
	return []groups.Group{actual}, f.err
}

func (f *fakeGroupManager) Test(ctx context.Context, desired groups.Group) (groups.Group, []string, error) {
	actual, err := f.Get(ctx, desired)
	diff := []string{}
	ensure := desired.Ensure
	if ensure == "" {
		ensure = "Present"
	}
	if ensure != actual.Ensure {
		diff = append(diff, "ensure")
	}
	if desired.GID != nil && (actual.GID == nil || *desired.GID != *actual.GID) {
		diff = append(diff, "gid")
	}
	members := []string{}
	if actual.Members != nil {
		members = *actual.Members
	}
	if desired.Members != nil {
		want := slices.Clone(*desired.Members)
		slices.Sort(want)
		if !slices.Equal(want, members) {
			diff = append(diff, "members")
		}
	}
	if desired.IncludedMembers != nil {
		for _, name := range *desired.IncludedMembers {
			if !slices.Contains(members, name) {
				diff = append(diff, "includedMembers")
				break
			}
		}
	}
	if desired.ExcludedMembers != nil {
		for _, name := range *desired.ExcludedMembers {
			if slices.Contains(members, name) {
				diff = append(diff, "excludedMembers")
				break
			}
		}
	}
	return actual, diff, err
}

func (f *fakeGroupManager) Set(ctx context.Context, desired groups.Group) (groups.Group, error) {
	f.sets++
	f.actual.Ensure = desired.Ensure
	if f.actual.Ensure == "" {
		f.actual.Ensure = "Present"
	}
	if f.actual.Ensure == "Absent" {
		f.actual.GID, f.actual.Members = nil, nil
		return f.Get(ctx, desired)
	}
	if desired.GID != nil {
		f.actual.GID = desired.GID
	} else if f.actual.GID == nil {
		f.actual.GID = pointer(int64(2000))
	}
	members := []string{}
	if f.actual.Members != nil {
		members = slices.Clone(*f.actual.Members)
	}
	if desired.Members != nil {
		members = slices.Clone(*desired.Members)
	} else {
		if desired.IncludedMembers != nil {
			for _, name := range *desired.IncludedMembers {
				if !slices.Contains(members, name) {
					members = append(members, name)
				}
			}
		}
		if desired.ExcludedMembers != nil {
			members = slices.DeleteFunc(members, func(name string) bool {
				return slices.Contains(*desired.ExcludedMembers, name)
			})
		}
	}
	if members == nil {
		members = []string{}
	}
	slices.Sort(members)
	f.actual.Members = &members
	return f.Get(ctx, desired)
}

func callGroups(t *testing.T, manager groupManager, operation, input string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run(t.Context(), []string{"groups", operation}, strings.NewReader(input), &stdout, &stderr, nil, manager)
	return code, stdout.String(), stderr.String()
}

func TestGroupsCLIContract(t *testing.T) {
	manager := newFakeGroupManager()
	for _, operation := range []string{"get", "test", "set", "test"} {
		code, stdout, stderr := callGroups(t, manager, operation,
			`{"name":"dev","gid":3000,"includedMembers":["bob","alice"],"futureProperty":true}`)
		if code != 0 || stderr != "" {
			t.Fatalf("%s: %d %s", operation, code, stderr)
		}
		decoder := json.NewDecoder(strings.NewReader(stdout))
		var actual struct {
			Name                             string
			IncludedMembers, ExcludedMembers *[]string
			InDesiredState                   bool `json:"_inDesiredState"`
		}
		if err := decoder.Decode(&actual); err != nil || actual.Name != "dev" ||
			actual.IncludedMembers != nil || actual.ExcludedMembers != nil {
			t.Fatalf("invalid state: %s %v", stdout, err)
		}
		if operation == "test" {
			var diff []string
			want := []string{}
			if manager.sets == 0 {
				want = []string{"ensure", "gid", "includedMembers"}
			}
			if err := decoder.Decode(&diff); err != nil || diff == nil || !slices.Equal(diff, want) ||
				actual.InDesiredState != (len(diff) == 0) {
				t.Fatalf("invalid stateAndDiff: %s %v", stdout, err)
			}
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			t.Fatalf("extra JSON output: %s", stdout)
		}
	}
	for _, input := range []string{"", "{", "[]", "null", `{"name":"a"} {}`, `{"name":"a","gid":"2000"}`, strings.Repeat(" ", maxInputBytes+1)} {
		code, stdout, stderr := callGroups(t, manager, "get", input)
		if code == 0 || stdout != "" || !json.Valid([]byte(stderr)) {
			t.Fatalf("invalid input accepted: %d %q %q", code, stdout, stderr)
		}
	}
	for _, input := range []string{`{}`, `{"name":"-bad"}`, `{"name":"dev","members":[],"includedMembers":[]}`} {
		code, stdout, stderr := callGroups(t, groups.NewManager(nil, ""), "get", input)
		if code == 0 || stdout != "" || !json.Valid([]byte(stderr)) {
			t.Fatalf("invalid properties accepted: %d %q %q", code, stdout, stderr)
		}
	}
	manager.err = errors.New("permission denied")
	for _, operation := range []string{"get", "test", "set"} {
		code, stdout, stderr := callGroups(t, manager, operation, `{"name":"dev"}`)
		if code == 0 || stdout != "" || !strings.Contains(stderr, "permission denied") {
			t.Fatalf("error hidden: %d %q %q", code, stdout, stderr)
		}
	}
}

func TestGroupsManifest(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "manifests", "groups.dsc.resource.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Type, Version string
		Export        struct {
			Executable, Input string
			Args              []string
		}
		Get, Test, Set struct {
			Executable, Input, Return string
			Args                      []string
			ImplementsPretest         bool
		}
		Schema struct {
			Embedded struct {
				Required             []string
				Properties           map[string]map[string]any
				AdditionalProperties bool
				AllOf                []any
			}
		}
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Type != "DscExtras.Linux/groups" || manifest.Version != version ||
		manifest.Export.Executable != "dsclinux" || manifest.Export.Input != "stdin" ||
		!slices.Equal(manifest.Export.Args, []string{"groups", "export"}) {
		t.Fatalf("incorrect type/version: %s", data)
	}
	for _, operation := range []struct {
		name       string
		executable string
		input      string
		args       []string
	}{
		{"get", manifest.Get.Executable, manifest.Get.Input, manifest.Get.Args},
		{"test", manifest.Test.Executable, manifest.Test.Input, manifest.Test.Args},
		{"set", manifest.Set.Executable, manifest.Set.Input, manifest.Set.Args},
	} {
		if operation.executable != "dsclinux" || operation.input != "stdin" ||
			!slices.Equal(operation.args, []string{"groups", operation.name}) {
			t.Fatalf("incorrect manifest dispatch: %+v", operation)
		}
	}

	schema := manifest.Schema.Embedded
	if manifest.Test.Return != "stateAndDiff" || manifest.Set.Return != "state" || !manifest.Set.ImplementsPretest ||
		!schema.AdditionalProperties || !slices.Equal(schema.Required, []string{"name"}) ||
		schema.Properties["name"]["x-dsc-key"] != true || schema.Properties["ensure"]["default"] != "Present" ||
		schema.Properties["_inDesiredState"]["readOnly"] != true ||
		schema.Properties["_name"]["type"] != "string" || schema.Properties["_name"]["readOnly"] != true ||
		len(schema.AllOf) != 2 {
		t.Fatalf("incorrect schema/contract: %s", data)
	}
	for _, property := range []string{"gid", "members", "includedMembers", "excludedMembers"} {
		if _, ok := schema.Properties[property]["default"]; ok {
			t.Fatalf("%s must be unmanaged when omitted", property)
		}
		if property != "gid" && schema.Properties[property]["uniqueItems"] != true {
			t.Fatalf("%s must require uniqueness", property)
		}
	}
}

func TestGroupsExportWritesJSONLinesAndFiltersByName(t *testing.T) {
	manager := newFakeGroupManager()
	manager.actual = groups.Group{Name: "dev", Ensure: "Present", GID: pointer(int64(2000))}
	for _, test := range []struct {
		input string
		count int
	}{
		{"", 1},
		{`{"name":"dev"}`, 1},
		{`{"name":"missing"}`, 0},
	} {
		code, stdout, stderr := callGroups(t, manager, "export", test.input)
		if test.count == 0 {
			if code != 0 || stdout != "" || stderr != "" {
				t.Fatalf("empty Export = %d %q %q", code, stdout, stderr)
			}
			continue
		}
		var actual struct {
			Name       string `json:"name"`
			ExportName string `json:"_name"`
		}
		if code != 0 || stderr != "" || json.Unmarshal([]byte(strings.TrimSpace(stdout)), &actual) != nil ||
			actual.Name != "dev" || actual.ExportName != actual.Name {
			t.Fatalf("invalid JSON Lines Export = %d %q %q", code, stdout, stderr)
		}
	}
}
