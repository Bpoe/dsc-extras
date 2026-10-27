package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/apt"
)

var aptTransportURIs = []string{
	"mirror+file:///etc/apt/mirrors/debian.list",
	"mirror+file:///etc/apt/mirrors/debian-security.list",
	"mirror+http://mirrors.example/debian.list",
	"mirror+https://mirrors.example/debian.list",
	"mirror://mirrors.example/debian.list",
	"mirror+copy:/etc/apt/mirrors/debian.list",
	"http://archive.ubuntu.com/ubuntu",
	"https://security.ubuntu.com/ubuntu/",
	"file:///srv/apt/repo", "copy:/srv/apt/repo",
	"mirror+https://MIRRORS.example:443/a/../debian%2elist?channel=stable",
}

// Deliberately do not create mirror lists: repository management must not read them.
func seedAPTTransports(t *testing.T, store apt.Store, format string) {
	t.Helper()
	keyPath := filepath.Join(store.Root, "etc", "apt", "keyrings", "debian.asc")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(testPublicKey), 0644); err != nil {
		t.Fatal(err)
	}
	var sources strings.Builder
	for _, uri := range aptTransportURIs {
		suite := "trixie"
		if strings.Contains(uri, "debian-security") {
			suite = "trixie-security"
		} else if strings.Contains(uri, "ubuntu") {
			suite = "noble"
		}
		if format == "list" {
			fmt.Fprintf(&sources, "deb [arch=amd64 signed-by=/etc/apt/keyrings/debian.asc] %s %s main\n", uri, suite)
		} else {
			fmt.Fprintf(&sources, "Types: deb deb-src\nURIs: %s\nSuites: %s\nComponents: main\nArchitectures: amd64\nSigned-By: /etc/apt/keyrings/debian.asc\n\n", uri, suite)
		}
	}
	path := filepath.Join(store.Root, "etc", "apt", "sources.list.d", "debian."+format)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(sources.String()), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestAPTTransportCLIExportRoundTrip(t *testing.T) {
	for _, format := range []string{"list", "sources"} {
		t.Run(format, func(t *testing.T) {
			store := makeStore(t)
			seedAPTTransports(t, store, format)
			code, output, stderr := call(t, store, "export", "")
			if code != 0 || stderr != "" {
				t.Fatalf("Export: %d %s %s", code, output, stderr)
			}
			lines := strings.Split(strings.TrimSpace(output), "\n")
			if len(lines) != len(aptTransportURIs) {
				t.Fatalf("Export returned %d repositories", len(lines))
			}
			seen := map[string]bool{}
			fresh := makeStore(t)
			for _, line := range lines {
				var exported apt.Repository
				if err := json.Unmarshal([]byte(line), &exported); err != nil {
					t.Fatal(err)
				}
				seen[exported.URI] = true
				input, err := json.Marshal(exported)
				if err != nil {
					t.Fatal(err)
				}
				for _, operation := range []string{"get", "test", "set", "export"} {
					code, result, stderr := call(t, store, operation, string(input))
					if code != 0 || stderr != "" {
						t.Fatalf("%s %s: %d %s %s", operation, exported.URI, code, result, stderr)
					}
					if operation == "test" {
						state, diff := decodeTestOutput(t, result)
						if !state.InDesiredState || len(diff) != 0 || state.URI != exported.URI {
							t.Fatalf("Export -> Test: %s", result)
						}
					} else {
						var state apt.Repository
						if err := json.Unmarshal([]byte(result), &state); err != nil || state.URI != exported.URI {
							t.Fatalf("%s changed URI: %s (%v)", operation, result, err)
						}
					}
				}
				// Exercise actual creation rather than only the compliant Set path.
				code, result, stderr := call(t, fresh, "set", string(input))
				if code != 0 || stderr != "" {
					t.Fatalf("Set on empty root: %d %s %s", code, result, stderr)
				}
				code, result, stderr = call(t, fresh, "test", string(input))
				if code != 0 || stderr != "" {
					t.Fatalf("Test new source: %d %s %s", code, result, stderr)
				}
				state, diff := decodeTestOutput(t, result)
				if !state.InDesiredState || len(diff) != 0 || state.URI != exported.URI {
					t.Fatalf("new source did not preserve URI: %s", result)
				}
			}
			for _, uri := range aptTransportURIs {
				if !seen[uri] {
					t.Errorf("original URI missing from Export: %s", uri)
				}
			}
		})
	}
}

func TestAPTTransportCLIRejectsUnsafeURI(t *testing.T) {
	for _, uri := range []string{
		"mirror+file:relative.list",
		"mirror+https:///missing-host.list",
		"mirror+file://remote/etc/apt/mirrors/debian.list",
		"mirror+https://user:password@mirrors.example/debian.list",
		"mirror://user@mirrors.example/debian.list",
		"mirror+file:///etc/apt/mirrors/debian.list#fragment",
	} {
		t.Run(uri, func(t *testing.T) {
			store := makeStore(t)
			seedAPTTransports(t, store, "sources")
			input, err := json.Marshal(apt.Repository{Name: "unsafe", URI: uri, Suite: "trixie",
				Components: []string{"main"}, SigningKey: testPublicKey})
			if err != nil {
				t.Fatal(err)
			}
			for _, operation := range []string{"get", "set", "test", "export"} {
				code, output, stderr := call(t, store, operation, string(input))
				if code == 0 || output != "" || stderr == "" {
					t.Fatalf("%s accepted unsafe URI: %d %s %s", operation, code, output, stderr)
				}
			}
			// The same shared validation applies to URIs discovered during Export.
			path := filepath.Join(store.Root, "etc", "apt", "sources.list.d", "unsafe.sources")
			if err := os.WriteFile(path, []byte("Types: deb\nURIs: "+uri+"\nSuites: trixie\nComponents: main\nSigned-By: /etc/apt/keyrings/debian.asc\n"), 0644); err != nil {
				t.Fatal(err)
			}
			code, output, stderr := call(t, store, "export", "")
			if code == 0 || output != "" || stderr == "" {
				t.Fatalf("Export accepted unsafe on-disk URI: %d %s %s", code, output, stderr)
			}
		})
	}
}
