package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-extras/resources/apt/internal/apt"
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
	return apt.NewStore(root+"/sources.list.d", root+"/keyrings")
}

func call(t *testing.T, store apt.Store, operation, input string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run([]string{operation}, strings.NewReader(input), &stdout, &stderr, store)
	return code, stdout.String(), stderr.String()
}

func TestGetAcceptsOnlyNameAndReturnsAbsentState(t *testing.T) {
	store := makeStore(t)
	code, stdout, stderr := call(t, store, "get", `{"name":"example"}`)
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
	var testOutput map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &testOutput); err != nil {
		t.Fatal(err)
	}
	var compliant bool
	if err := json.Unmarshal(testOutput["_inDesiredState"], &compliant); err != nil || !compliant {
		t.Fatalf("Test _inDesiredState=%s err=%v, want true", testOutput["_inDesiredState"], err)
	}
	for _, property := range []string{"name", "ensure", "uri", "suite", "components", "signingKey"} {
		if _, ok := testOutput[property]; !ok {
			t.Errorf("Test output omitted schema property %q", property)
		}
	}

	code, stdout, stderr = call(t, store, "get", `{"name":"example"}`)
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

func TestCLIRejectsInvalidJSONAndUnknownOperation(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation string
		input     string
	}{
		{name: "invalid JSON", operation: "get", input: "{"},
		{name: "unknown operation", operation: "remove", input: `{"name":"example"}`},
		{name: "unsafe name", operation: "get", input: `{"name":"../escape"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := call(t, makeStore(t), test.operation, test.input)
			if code == 0 || stdout != "" || stderr == "" {
				t.Fatalf("run()=(%d,%q,%q), want error and empty stdout", code, stdout, stderr)
			}
		})
	}
}
