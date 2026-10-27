package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/apt"
)

func TestDSCAPTTransportExportAndTest(t *testing.T) {
	for _, manifest := range []string{"aptRepositories.dsc.resource.json", "dscapt.dsc.resource.json"} {
		t.Run(manifest, func(t *testing.T) {
			dsc, resourceType := prepareDSCResource(t, manifest)
			store := makeStore(t)
			seedAPTTransports(t, store, "sources")
			t.Setenv("DSCAPT_TEST_ROOT", store.Root)
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			invoke := func(operation string, input []byte) []byte {
				t.Helper()
				args := []string{"resource", operation, "--resource", resourceType, "--output-format", "json"}
				if input != nil {
					args = append(args, "--file", "-")
				}
				command := exec.CommandContext(ctx, dsc, args...)
				command.Stdin = bytes.NewReader(input)
				var stderr bytes.Buffer
				command.Stderr = &stderr
				output, err := command.Output()
				if err != nil {
					t.Fatalf("DSC %s failed: %v\n%s", operation, err, string(output)+stderr.String())
				}
				return output
			}
			output := invoke("export", nil)
			var config struct {
				Resources []struct {
					Properties apt.Repository `json:"properties"`
				} `json:"resources"`
			}
			if err := json.Unmarshal(output, &config); err != nil || len(config.Resources) != len(aptTransportURIs) {
				t.Fatalf("DSC Export: %s (%v)", output, err)
			}
			seen := map[string]bool{}
			for _, resource := range config.Resources {
				desired := resource.Properties
				seen[desired.URI] = true
				input, err := json.Marshal(desired)
				if err != nil {
					t.Fatal(err)
				}
				output := invoke("test", input)
				var result struct {
					InDesiredState      bool           `json:"inDesiredState"`
					ActualState         apt.Repository `json:"actualState"`
					DifferingProperties []string       `json:"differingProperties"`
				}
				if err := json.Unmarshal(output, &result); err != nil || !result.InDesiredState ||
					len(result.DifferingProperties) != 0 || result.ActualState.URI != desired.URI {
					t.Fatalf("DSC Export -> Test: %s (%v)", output, err)
				}
				for _, operation := range []string{"get", "set"} {
					output := invoke(operation, input)
					var result struct {
						ActualState apt.Repository `json:"actualState"`
						AfterState  apt.Repository `json:"afterState"`
					}
					if err := json.Unmarshal(output, &result); err != nil {
						t.Fatal(err)
					}
					state := result.ActualState
					if operation == "set" {
						state = result.AfterState
					}
					if state.URI != desired.URI {
						t.Fatalf("DSC %s changed URI: %s", operation, output)
					}
				}
			}
			for _, uri := range aptTransportURIs {
				if !seen[uri] {
					t.Errorf("DSC Export lost original URI %s", uri)
				}
			}
		})
	}
}
