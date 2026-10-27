//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Run the real dispatch and filter readers in a subprocess with isolated backends.
func TestExportStdinProcess(t *testing.T) {
	resource := os.Getenv("EXPORT_STDIN_TEST_RESOURCE")
	if resource == "" {
		return
	}

	os.Exit(run(context.Background(), []string{resource, "export"}, os.Stdin, os.Stdout, os.Stderr, newFakeManager(), newFakeGroupManager()))
}

func TestExportStdin(t *testing.T) {
	for _, resource := range []string{"services", "groups"} {
		t.Run(resource, func(t *testing.T) {
			for _, mode := range []string{"terminal", "devnull", "empty-file", "empty-pipe", "filter-pipe", "matching-pipe", "invalid-pipe"} {
				t.Run(mode, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExportStdinProcess$")
					cmd.Env = append(os.Environ(), "EXPORT_STDIN_TEST_RESOURCE="+resource)
					var stdout, stderr bytes.Buffer
					cmd.Stdout, cmd.Stderr = &stdout, &stderr
					switch mode {
					case "terminal":
						master, slave, err := pty.Open()
						if err != nil {
							t.Fatal(err)
						}
						defer master.Close() // Keep the terminal open: no input and no EOF.
						defer slave.Close()
						cmd.Stdin = slave
					case "devnull", "empty-file":
						path := os.DevNull
						if mode == "empty-file" {
							path = filepath.Join(t.TempDir(), "empty")
							if err := os.WriteFile(path, nil, 0600); err != nil {
								t.Fatal(err)
							}
						}
						input, err := os.Open(path)
						if err != nil {
							t.Fatal(err)
						}
						defer input.Close()
						cmd.Stdin = input
					default:
						input := ""
						if mode == "filter-pipe" {
							input = `{"name":"missing"}`
						}
						if mode == "matching-pipe" {
							input = `{"name":"example"}`
							if resource == "groups" {
								input = `{"name":"dev"}`
							}
						}
						if mode == "invalid-pipe" {
							input = `{`
						}
						cmd.Stdin = strings.NewReader(input) // exec copies this through an OS pipe.
					}
					err := cmd.Run()
					if ctx.Err() != nil {
						t.Fatalf("export blocked waiting for stdin: %v", ctx.Err())
					}
					if mode == "invalid-pipe" {
						exit, ok := err.(*exec.ExitError)
						if !ok || exit.ExitCode() != 1 || stdout.Len() != 0 || !json.Valid(stderr.Bytes()) || !strings.Contains(stderr.String(), "export filter") {
							t.Fatalf("invalid filter: %v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
						}
						return
					}
					if err != nil || stderr.Len() != 0 {
						t.Fatalf("export: %v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
					}
					if mode == "filter-pipe" {
						if stdout.Len() != 0 {
							t.Fatalf("filter was ignored: %s", &stdout)
						}
						return
					}
					lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
					if len(lines) != 1 {
						t.Fatalf("expected one JSON Lines instance: %q", stdout.String())
					}
					var instance map[string]any
					if err := json.Unmarshal([]byte(lines[0]), &instance); err != nil || instance["_name"] == nil || instance["name"] == nil {
						t.Fatalf("invalid exported instance: %s (%v)", lines[0], err)
					}
				})
			}
		})
	}
}
