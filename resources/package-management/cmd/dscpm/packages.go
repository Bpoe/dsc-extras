package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/apt"
)

func runPackages(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, manager *apt.PackageManager) int {
	if len(args) != 1 || (args[0] != "get" && args[0] != "test" && args[0] != "set") {
		return reportError(stderr, "expected one packages operation: get, test, or set")
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxInputBytes+1))
	if err != nil {
		return reportError(stderr, fmt.Sprintf("read packages input: %v", err))
	}
	if len(data) > maxInputBytes {
		return reportError(stderr, "packages input is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var desired apt.Packages
	if err := decoder.Decode(&desired); err != nil {
		return reportError(stderr, fmt.Sprintf("invalid packages JSON input: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return reportError(stderr, "expected a single packages JSON object")
	}
	var properties map[string]json.RawMessage
	if err := json.Unmarshal(data, &properties); err != nil || properties == nil {
		return reportError(stderr, "expected a packages JSON object")
	}
	for _, property := range []string{"ensure", "version", "allowDowngrade", "updateCache"} {
		if value, present := properties[property]; present {
			value = bytes.TrimSpace(value)
			if bytes.Equal(value, []byte("null")) || bytes.Equal(value, []byte(`""`)) {
				return reportError(stderr, property+" must not be null or empty")
			}
		}
	}

	var actual apt.Packages
	var differences []string
	switch args[0] {
	case "get":
		actual, err = manager.Get(ctx, desired)
	case "test":
		actual, differences, err = manager.Test(ctx, desired)
	case "set":
		actual, err = manager.Set(ctx, desired)
	}
	if err != nil {
		reportError(stderr, err.Error())
		var exitError interface{ ExitCode() int }
		if errors.As(err, &exitError) && exitError.ExitCode() > 0 {
			return exitError.ExitCode()
		}
		return 1
	}
	if args[0] == "test" {
		result := struct {
			apt.Packages
			InDesiredState bool `json:"_inDesiredState"`
		}{actual, len(differences) == 0}
		if code := writeJSON(stdout, result, stderr); code != 0 {
			return code
		}
		return writeJSON(stdout, differences, stderr)
	}
	return writeJSON(stdout, actual, stderr)
}
