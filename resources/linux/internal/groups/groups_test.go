package groups

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func ptr[T any](value T) *T { return &value }

type exitStatus int

func (s exitStatus) Error() string { return fmt.Sprintf("exit status %d", s) }
func (s exitStatus) ExitCode() int { return int(s) }

type response struct {
	output string
	err    error
}

type fakeNative struct {
	t          *testing.T
	manager    *Manager
	users      map[string]int64
	external   map[string]int64
	responses  map[string]response
	calls      []string
	mutations  []string
	fail       map[string]bool
	ignore     bool
	conflictID int64
}

func fixture(t *testing.T, entries string) (*Manager, *fakeNative) {
	t.Helper()
	f := &fakeNative{
		t: t, users: map[string]int64{"alice": 100, "bob": 100, "carol": 100, "old": 100},
		external: map[string]int64{}, responses: map[string]response{}, fail: map[string]bool{},
		conflictID: 9999,
	}
	m := NewManager(f, t.TempDir())
	f.manager = m
	if err := os.MkdirAll(filepath.Join(m.root, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	f.write(entries)
	return m, f
}

func (f *fakeNative) write(entries string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.manager.root, "etc", "group"), []byte(entries), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeNative) Execute(ctx context.Context, program string, args []string) ([]byte, []byte, error) {
	f.t.Helper()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	call := program + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if program == "getent" {
		if len(args) != 2 {
			f.t.Fatalf("unexpected lookup: %s", call)
		}
		if r, ok := f.responses[call]; ok {
			return []byte(r.output), []byte("lookup diagnostic"), r.err
		}
		name := args[1]
		if args[0] == "passwd" {
			if gid, ok := f.users[name]; ok {
				return []byte(fmt.Sprintf("%s:x:1000:%d::/home/%s:/bin/sh\n", name, gid, name)), nil, nil
			}
		} else if args[0] == "group" {
			if gid, ok := f.external[name]; ok {
				return []byte(fmt.Sprintf("%s:x:%d:\n", name, gid)), nil, nil
			}
		} else {
			f.t.Fatalf("unexpected database: %s", call)
		}
		return nil, nil, exitStatus(2)
	}
	f.mutations = append(f.mutations, call)
	if f.fail[program] {
		return nil, []byte("permission denied"), exitStatus(1)
	}
	if f.ignore {
		return nil, nil, nil
	}
	name := args[len(args)-1]
	actual, err := f.manager.observe(name)
	if err != nil {
		f.t.Fatal(err)
	}
	switch program {
	case "groupadd", "groupmod":
		if args[len(args)-2] != "--" {
			f.t.Fatalf("missing option terminator: %s", call)
		}
		gid := int64(2000)
		if len(args) == 4 && args[0] == "-g" {
			gid, err = strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				f.t.Fatal(err)
			}
		} else if program == "groupmod" || len(args) != 2 {
			f.t.Fatalf("unexpected native arguments: %s", call)
		}
		if gid == f.conflictID {
			return nil, []byte("GID already exists"), exitStatus(4)
		}
		if program == "groupmod" {
			for user, primary := range f.users {
				if primary == *actual.GID {
					f.users[user] = gid
				}
			}
		}
		actual.Ensure, actual.GID = "Present", &gid
		if actual.Members == nil {
			actual.Members = ptr([]string{})
		}
	case "groupdel":
		if !slices.Equal(args, []string{"--", name}) {
			f.t.Fatalf("unexpected deletion arguments: %s", call)
		}
		for _, primary := range f.users {
			if primary == *actual.GID {
				return nil, []byte("cannot remove the primary group of user"), exitStatus(8)
			}
		}
		f.write("")
		return nil, nil, nil
	case "gpasswd":
		if len(args) != 3 {
			f.t.Fatalf("unexpected membership arguments: %s", call)
		}
		members := slices.Clone(memberList(actual))
		switch args[0] {
		case "-a":
			members = append(members, args[1])
		case "-d":
			members = slices.DeleteFunc(members, func(name string) bool { return name == args[1] })
		default:
			f.t.Fatalf("unexpected membership operation: %s", call)
		}
		actual.Members = &members
	default:
		f.t.Fatalf("unexpected mutation: %s", call)
	}
	f.write(fmt.Sprintf("%s:x:%d:%s\n", actual.Name, *actual.GID, strings.Join(memberList(actual), ",")))
	return nil, nil, nil
}

func desired(t *testing.T, input string) Group {
	t.Helper()
	var value Group
	if err := json.Unmarshal([]byte(input), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestSetReconciliation(t *testing.T) {
	for _, test := range []struct {
		name, before, input string
		want                []string
		members             []string
	}{
		{"create automatic", "", `{"name":"dev"}`, []string{"groupadd -- dev"}, []string{}},
		{"create explicit", "", `{"name":"dev","gid":3000}`, []string{"groupadd -g 3000 -- dev"}, []string{}},
		{"adopt", "dev:x:2000:old\n", `{"name":"dev"}`, nil, []string{"old"}},
		{"change gid", "dev:x:2000:old\n", `{"name":"dev","gid":3000}`, []string{"groupmod -g 3000 -- dev"}, []string{"old"}},
		{"delete", "dev:x:2000:old\n", `{"name":"dev","ensure":"Absent"}`, []string{"groupdel -- dev"}, nil},
		{"absent deletion", "", `{"name":"dev","ensure":"Absent"}`, nil, nil},
		{"add", "dev:x:2000:old\n", `{"name":"dev","includedMembers":["bob","alice"]}`, []string{"gpasswd -a alice dev", "gpasswd -a bob dev"}, []string{"alice", "bob", "old"}},
		{"remove", "dev:x:2000:alice,bob,old\n", `{"name":"dev","excludedMembers":["old"]}`, []string{"gpasswd -d old dev"}, []string{"alice", "bob"}},
		{"exact", "dev:x:2000:old,bob\n", `{"name":"dev","members":["bob","alice"]}`, []string{"gpasswd -a alice dev", "gpasswd -d old dev"}, []string{"alice", "bob"}},
		{"empty", "dev:x:2000:bob,alice\n", `{"name":"dev","members":[]}`, []string{"gpasswd -d alice dev", "gpasswd -d bob dev"}, []string{}},
		{"combined", "dev:x:2000:old,carol\n", `{"name":"dev","includedMembers":["alice"],"excludedMembers":["old"]}`, []string{"gpasswd -a alice dev", "gpasswd -d old dev"}, []string{"alice", "carol"}},
		{"compliant order", "dev:x:2000:alice,bob\n", `{"name":"dev","members":["bob","alice"]}`, nil, []string{"alice", "bob"}},
		{"empty selective", "dev:x:2000:old\n", `{"name":"dev","includedMembers":[],"excludedMembers":[]}`, nil, []string{"old"}},
		{"create members", "", `{"name":"dev","members":["alice"]}`, []string{"groupadd -- dev", "gpasswd -a alice dev"}, []string{"alice"}},
		{"unknown properties", "dev:x:2000:old\n", `{"name":"dev","future":{"ignored":true}}`, nil, []string{"old"}},
		{"split compliant", "dev:x:2000:alice,bob\ndev:x:2000:bob,carol\n", `{"name":"dev","members":["carol","alice","bob"]}`, nil, []string{"alice", "bob", "carol"}},
		{"split reconcile", "dev:x:2000:alice\ndev:x:2000:bob\n", `{"name":"dev","members":["carol","bob"]}`, []string{"gpasswd -a carol dev", "gpasswd -d alice dev"}, []string{"bob", "carol"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, f := fixture(t, test.before)
			want := desired(t, test.input)
			before, err := os.ReadFile(filepath.Join(m.root, "etc", "group"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Get(t.Context(), want); err != nil {
				t.Fatal(err)
			}
			if _, _, err := m.Test(t.Context(), want); err != nil {
				t.Fatal(err)
			}
			unchanged, err := os.ReadFile(filepath.Join(m.root, "etc", "group"))
			if err != nil || !slices.Equal(before, unchanged) || len(f.mutations) != 0 {
				t.Fatalf("Get/Test mutated fixture: %v, %v", f.mutations, err)
			}
			actual, err := m.Set(t.Context(), want)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(f.mutations, test.want) || !slices.Equal(memberList(actual), test.members) {
				t.Fatalf("mutations %v, members %v; want %v, %v", f.mutations, memberList(actual), test.want, test.members)
			}
			if actual.IncludedMembers != nil || actual.ExcludedMembers != nil {
				t.Fatal("observed state contains desired constraints")
			}
			if want.GID != nil && (actual.GID == nil || *want.GID != *actual.GID) {
				t.Fatalf("GID was not applied: %+v", actual)
			}
			_, diff, err := m.Test(t.Context(), want)
			if err != nil || diff == nil || len(diff) != 0 {
				t.Fatalf("not compliant: %v, %v", diff, err)
			}
			count := len(f.mutations)
			if _, err := m.Set(t.Context(), want); err != nil || len(f.mutations) != count {
				t.Fatalf("repeated Set not idempotent: %v, %v", f.mutations, err)
			}
		})
	}
}

func TestObservedStateAndDifferences(t *testing.T) {
	m, _ := fixture(t, "dev:x:2000:bob,alice,alice\ndev:x:2000:bob,carol\n")
	for _, test := range []struct {
		input string
		diff  []string
	}{
		{`{"name":"dev"}`, []string{}},
		{`{"name":"dev","gid":3000,"members":[]}`, []string{"gid", "members"}},
		{`{"name":"dev","includedMembers":["old"],"excludedMembers":["bob"]}`, []string{"includedMembers", "excludedMembers"}},
		{`{"name":"missing","gid":3000,"includedMembers":["alice"]}`, []string{"ensure", "gid", "includedMembers"}},
		{`{"name":"missing","members":[]}`, []string{"ensure"}},
		{`{"name":"dev","ensure":"Absent"}`, []string{"ensure"}},
	} {
		actual, diff, err := m.Test(t.Context(), desired(t, test.input))
		if err != nil || !reflect.DeepEqual(diff, test.diff) {
			t.Fatalf("%s: %v %v, want %v", test.input, diff, err, test.diff)
		}
		output, err := json.Marshal(actual)
		if err != nil {
			t.Fatal(err)
		}
		if actual.Ensure == "Absent" && string(output) != `{"name":"missing","ensure":"Absent"}` {
			t.Fatalf("invented absent properties: %s", output)
		}
		if actual.Ensure == "Present" && !slices.Equal(memberList(actual), []string{"alice", "bob", "carol"}) {
			t.Fatalf("split membership not unioned: %s", output)
		}
	}
	m, _ = fixture(t, "dev:x:2000:\n")
	actual, err := m.Get(t.Context(), Group{Name: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	output, _ := json.Marshal(actual)
	if !strings.Contains(string(output), `"members":[]`) {
		t.Fatalf("empty observed members must be an array: %s", output)
	}
}

func TestValidation(t *testing.T) {
	for _, input := range []string{
		`{}`, `null`, `[]`, `{"name":null}`, `{"name":"dev","ensure":null}`, `{"name":"dev","members":null}`,
		`{"name":"dev","gid":null}`, `{"name":"dev","includedMembers":null}`, `{"name":"dev","excludedMembers":null}`,
		`{"name":"dev","ensure":"present"}`, `{"name":"dev","ensure":""}`, `{"name":"dev","gid":-1}`, `{"name":"dev","gid":4294967295}`,
		`{"name":"dev","gid":1.5}`, `{"name":"dev","members":["alice","alice"]}`,
		`{"name":"dev","includedMembers":["alice","alice"]}`, `{"name":"dev","excludedMembers":["old","old"]}`,
		`{"name":"dev","members":[],"includedMembers":[]}`, `{"name":"dev","members":[],"excludedMembers":[]}`,
		`{"name":"dev","includedMembers":["alice"],"excludedMembers":["alice"]}`,
		`{"name":"dev","ensure":"Absent","gid":0}`, `{"name":"dev","ensure":"Absent","members":[]}`,
		`{"name":"dev","ensure":"Absent","includedMembers":[]}`, `{"name":"dev","ensure":"Absent","excludedMembers":[]}`,
		`{"name":"-option"}`, `{"name":"+ldap"}`, `{"name":"#comment"}`, `{"name":"../dev"}`, `{"name":"a:b"}`, `{"name":"a\nb"}`,
		`{"name":"a b"}`, `{"name":"a,b"}`, `{"name":"a\u0000b"}`, `{"name":"."}`, `{"name":"dev","members":["-a"]}`,
		`{"name":"dev","members":[""]}`, `{"name":"dev","members":["a:b"]}`,
	} {
		t.Run(input, func(t *testing.T) {
			m, f := fixture(t, "")
			var value Group
			err := json.Unmarshal([]byte(input), &value)
			if err == nil {
				if _, err := m.Get(t.Context(), value); err == nil {
					t.Fatal("Get accepted invalid properties")
				}
				if _, _, err := m.Test(t.Context(), value); err == nil {
					t.Fatal("Test accepted invalid properties")
				}
				_, err = m.Set(t.Context(), value)
			}
			if err == nil || len(f.calls) != 0 {
				t.Fatalf("invalid properties accepted or commands executed: %v, %v", err, f.calls)
			}
		})
	}
	for _, gid := range []int64{0, maxGID} {
		if _, err := normalize(Group{Name: "dev", GID: &gid}); err != nil {
			t.Fatalf("valid GID rejected: %d: %v", gid, err)
		}
	}
}

func TestLookupResults(t *testing.T) {
	for _, database := range []string{"passwd", "group"} {
		for _, test := range []struct {
			name, output string
			status       error
			found, fail  bool
		}{
			{"missing", "", exitStatus(2), false, false},
			{"invocation", "", exitStatus(1), false, true},
			{"enumeration", "", exitStatus(3), false, true},
			{"execution", "", errors.New("not executable"), false, true},
			{"empty", "", nil, false, true},
			{"malformed", "dev:x", nil, false, true},
			{"mismatch", "other:x:1000:100::/:/bin/sh", nil, false, true},
			{"invalid gid", "dev:x:invalid:invalid", nil, false, true},
		} {
			t.Run(database+"/"+test.name, func(t *testing.T) {
				m, f := fixture(t, "")
				f.responses["getent "+database+" dev"] = response{test.output, test.status}
				_, found, err := m.lookup(t.Context(), database, "dev")
				if found != test.found || (err != nil) != test.fail {
					t.Fatalf("lookup found=%v err=%v", found, err)
				}
			})
		}
	}
	for _, test := range []struct{ database, output string }{
		{"passwd", "dev:x:1000:2000::/home/dev:/bin/sh\n"},
		{"group", "dev:x:2000:alice\n"},
		{"group", "dev:x:2000:alice\ndev:x:2000:bob\n"},
	} {
		m, f := fixture(t, "")
		f.responses["getent "+test.database+" dev"] = response{output: test.output}
		if _, found, err := m.lookup(t.Context(), test.database, "dev"); err != nil || !found {
			t.Fatalf("successful lookup rejected: %v, %v", found, err)
		}
	}
	for _, test := range []struct{ database, output string }{
		{"passwd", "dev:x:bad:2000::/:/bin/sh\n"},
		{"passwd", "dev:x:1000:bad::/:/bin/sh\n"},
		{"passwd", "dev:x:1000:2000::/:/bin/sh\ndev:x:1000:2000::/:/bin/sh\n"},
		{"group", "dev:x:2000:alice\ndev:x:3000:bob\n"},
		{"group", "dev:x:2000:alice\nother:x:2000:bob\n"},
		{"group", "dev:x:2000:alice\nbroken\n"},
	} {
		m, f := fixture(t, "")
		f.responses["getent "+test.database+" dev"] = response{output: test.output}
		if _, _, err := m.lookup(t.Context(), test.database, "dev"); err == nil {
			t.Fatalf("malformed lookup accepted: %+v", test)
		}
	}
}

func TestPrimaryMembership(t *testing.T) {
	m, f := fixture(t, "dev:x:2000:\n")
	f.users["alice"] = 2000
	want := desired(t, `{"name":"dev","excludedMembers":["alice"]}`)
	_, diff, err := m.Test(t.Context(), want)
	if err != nil || !slices.Equal(diff, []string{"excludedMembers"}) || len(f.mutations) != 0 {
		t.Fatalf("primary exclusion Test: %v %v", diff, err)
	}
	for _, input := range []string{
		`{"name":"dev","excludedMembers":["alice"]}`,
		`{"name":"dev","gid":3000,"excludedMembers":["alice"]}`,
	} {
		if _, err := m.Set(t.Context(), desired(t, input)); err == nil || !strings.Contains(err.Error(), "primary group") || len(f.mutations) != 0 {
			t.Fatalf("primary exclusion must fail before mutation: %v", err)
		}
	}
	if _, err := m.Set(t.Context(), desired(t, `{"name":"dev","members":[]}`)); err != nil {
		t.Fatalf("exact membership must not manage primary membership: %v", err)
	}
	if _, err := m.Set(t.Context(), desired(t, `{"name":"dev","gid":3000}`)); err != nil || f.users["alice"] != 3000 {
		t.Fatalf("native primary-GID updates must be preserved: %v, %v", f.users, err)
	}
	m, f = fixture(t, "")
	f.users["alice"] = 2000
	if _, err := m.Set(t.Context(), want); err == nil || !strings.Contains(err.Error(), "primary group") ||
		!slices.Equal(f.mutations, []string{"groupadd -- dev"}) {
		t.Fatalf("allocated GID must be checked: %v %v", err, f.mutations)
	}
	m, f = fixture(t, "dev:x:2000:\n")
	f.users["alice"] = 3000
	if _, err := m.Set(t.Context(), desired(t, `{"name":"dev","gid":3000,"excludedMembers":["alice"]}`)); err == nil ||
		!strings.Contains(err.Error(), "primary group") || len(f.mutations) != 0 {
		t.Fatalf("known future-GID exclusion must fail before mutation: %v %v", err, f.mutations)
	}
}

func TestFailuresAndLocalBoundary(t *testing.T) {
	for _, test := range []struct {
		name, before, input, message string
		setup                        func(*fakeNative)
	}{
		{"missing member", "", `{"name":"dev","members":["missing"]}`, "does not exist", nil},
		{"missing inclusion", "", `{"name":"dev","includedMembers":["missing"]}`, "does not exist", nil},
		{"conflict creation", "", `{"name":"dev","gid":9999}`, "GID already exists", nil},
		{"conflict update", "dev:x:2000:\n", `{"name":"dev","gid":9999}`, "GID already exists", nil},
		{"primary deletion", "dev:x:2000:\n", `{"name":"dev","ensure":"Absent"}`, "primary group", func(f *fakeNative) { f.users["alice"] = 2000 }},
		{"external", "", `{"name":"dev"}`, "externally managed", func(f *fakeNative) { f.external["dev"] = 2000 }},
		{"create failure", "", `{"name":"dev"}`, "permission denied", func(f *fakeNative) { f.fail["groupadd"] = true }},
		{"update failure", "dev:x:2000:\n", `{"name":"dev","gid":3000}`, "permission denied", func(f *fakeNative) { f.fail["groupmod"] = true }},
		{"delete failure", "dev:x:2000:\n", `{"name":"dev","ensure":"Absent"}`, "permission denied", func(f *fakeNative) { f.fail["groupdel"] = true }},
		{"membership failure", "dev:x:2000:\n", `{"name":"dev","members":["alice"]}`, "permission denied", func(f *fakeNative) { f.fail["gpasswd"] = true }},
		{"create ignored", "", `{"name":"dev"}`, "still absent", func(f *fakeNative) { f.ignore = true }},
		{"membership ignored", "dev:x:2000:\n", `{"name":"dev","members":["alice"]}`, "did not converge", func(f *fakeNative) { f.ignore = true }},
		{"delete ignored", "dev:x:2000:\n", `{"name":"dev","ensure":"Absent"}`, "did not converge", func(f *fakeNative) { f.ignore = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, f := fixture(t, test.before)
			if test.setup != nil {
				test.setup(f)
			}
			if _, err := m.Set(t.Context(), desired(t, test.input)); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("wanted %q, got %v", test.message, err)
			}
		})
	}
	m, f := fixture(t, "")
	f.external["dev"] = 2000
	for _, operation := range []string{"get", "test", "set"} {
		want := Group{Name: "dev", Ensure: "Absent"}
		var actual Group
		var err error
		switch operation {
		case "get":
			actual, err = m.Get(t.Context(), want)
		case "test":
			var diff []string
			actual, diff, err = m.Test(t.Context(), want)
			if len(diff) != 0 {
				t.Fatal(diff)
			}
		case "set":
			actual, err = m.Set(t.Context(), want)
		}
		if err != nil || actual.Ensure != "Absent" || len(f.calls) != 0 {
			t.Fatalf("external absence must be local-only: %+v %v %v", actual, err, f.calls)
		}
	}
	m, f = fixture(t, "dev:x:2000:\n")
	if _, err := m.Set(t.Context(), desired(t, `{"name":"dev","excludedMembers":["missing"]}`)); err != nil || len(f.mutations) != 0 {
		t.Fatalf("missing excluded user should be compliant: %v", err)
	}
	m, f = fixture(t, "")
	f.fail["gpasswd"] = true
	want := desired(t, `{"name":"dev","members":["alice"]}`)
	if _, err := m.Set(t.Context(), want); err == nil {
		t.Fatal("membership failure hidden")
	}
	f.fail["gpasswd"] = false
	if _, err := m.Set(t.Context(), want); err != nil ||
		!slices.Equal(f.mutations, []string{"groupadd -- dev", "gpasswd -a alice dev", "gpasswd -a alice dev"}) {
		t.Fatalf("partial reconciliation could not resume: %v %v", err, f.mutations)
	}
	for _, database := range []string{"passwd alice", "group dev"} {
		m, f := fixture(t, "")
		f.responses["getent "+database] = response{err: exitStatus(1)}
		input := Group{Name: "dev"}
		if strings.HasPrefix(database, "passwd") {
			input.IncludedMembers = ptr([]string{"alice"})
			if _, _, err := m.Test(t.Context(), input); err == nil {
				t.Fatal("Test hid a failed user lookup")
			}
		}
		if _, err := m.Set(t.Context(), input); err == nil || len(f.mutations) != 0 {
			t.Fatalf("Set hid a failed lookup: %v %v", err, f.mutations)
		}
	}
}

func TestDatabaseErrors(t *testing.T) {
	if root := NewManager(nil, "").root; root != string(filepath.Separator) {
		t.Fatalf("default group inspection must be rooted at the filesystem root, not the working directory: %q", root)
	}
	for _, data := range []string{"broken\n", "dev:x:no:\n", "dev:x:2000:alice\ndev:x:3000:bob\n", "dev:x:2000:alice,,bob\n"} {
		m, f := fixture(t, data)
		for _, operation := range []string{"get", "test", "set"} {
			var err error
			switch operation {
			case "get":
				_, err = m.Get(t.Context(), Group{Name: "dev"})
			case "test":
				_, _, err = m.Test(t.Context(), Group{Name: "dev"})
			case "set":
				_, err = m.Set(t.Context(), Group{Name: "dev"})
			}

			if err == nil || len(f.calls) != 0 {
				t.Fatalf("invalid database accepted: %s, %v", data, err)
			}
		}
	}
	m, _ := fixture(t, "")
	if err := os.Remove(filepath.Join(m.root, "etc", "group")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(t.Context(), Group{Name: "dev"}); err == nil {
		t.Fatal("read failure must not look absent")
	}
}

func TestNativeExecutorProcess(t *testing.T) {
	if os.Getenv("DSCLINUX_GROUPS_EXECUTOR_HELPER") != "1" {
		return
	}
	for index, arg := range os.Args {
		if arg != "--" {
			continue
		}
		if os.Args[index+1] == "fail" {
			fmt.Fprint(os.Stderr, "native failure")
			os.Exit(7)
		}
		if err := json.NewEncoder(os.Stdout).Encode(struct {
			Args   []string
			Locale string
		}{os.Args[index+1:], os.Getenv("LC_ALL")}); err != nil {
			os.Exit(1)
		}
		fmt.Fprint(os.Stderr, "native diagnostic")
		os.Exit(0)
	}
	os.Exit(1)
}

func TestNativeExecutor(t *testing.T) {
	t.Setenv("DSCLINUX_GROUPS_EXECUTOR_HELPER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil, t.TempDir())
	literal := []string{"spaces stay together", "$(not-a-command); & literal"}
	args := append([]string{"-test.run=^TestNativeExecutorProcess$", "--"}, literal...)
	stdout, stderr, err := m.executor.Execute(t.Context(), executable, args)
	if err != nil || string(stderr) != "native diagnostic" {
		t.Fatalf("executor: %v, %s", err, stderr)
	}
	var result struct {
		Args   []string
		Locale string
	}
	if err := json.Unmarshal(stdout, &result); err != nil || !slices.Equal(result.Args, literal) || result.Locale != "C" {
		t.Fatalf("literal arguments or locale lost: %s %v", stdout, err)
	}
	_, stderr, err = m.executor.Execute(t.Context(), executable, []string{"-test.run=^TestNativeExecutorProcess$", "--", "fail"})
	var status *exec.ExitError
	if !errors.As(err, &status) || status.ExitCode() != 7 || string(stderr) != "native failure" {
		t.Fatalf("native error was lost: %v %s", err, stderr)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := m.executor.Execute(ctx, executable, args); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}
