package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/apt"
	"github.com/Bpoe/dsc-extras/resources/package-management/internal/dnf"
	"github.com/Bpoe/dsc-extras/resources/package-management/internal/packages"
	"github.com/Bpoe/dsc-extras/resources/package-management/internal/platform"
)

func packageBackend(detect func() (string, error)) (packages.Manager, error) {
	backend, err := detect()
	if err != nil {
		return nil, err
	}
	switch backend {
	case "apt":
		return apt.NewPackageManager(nil), nil
	case "dnf":
		return dnf.NewPackageManager(nil), nil
	default:
		return nil, fmt.Errorf("unsupported package backend %q", backend)
	}
}

func runPackages(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, manager packages.Manager) int {
	if len(args) != 1 || (args[0] != "get" && args[0] != "test" && args[0] != "set" && args[0] != "export") {
		return reportError(stderr, "expected one packages operation: get, test, set, or export")
	}
	if args[0] == "export" {
		filter, err := readPackageExportFilter(stdin)
		if err != nil {
			return reportError(stderr, "invalid packages export filter: "+err.Error())
		}
		if manager == nil {
			manager, err = packageBackend(platform.Detect)
			if err != nil {
				return reportError(stderr, err.Error())
			}
		}
		exporter, ok := manager.(interface {
			Export(context.Context, packages.Packages) ([]packages.Packages, error)
		})
		if !ok {
			return reportError(stderr, "package manager does not support export")
		}
		items, err := exporter.Export(ctx, filter)
		if err != nil {
			return reportError(stderr, err.Error())
		}
		for _, item := range items {
			result := struct {
				packages.Packages
				ExportName string `json:"_name"`
			}{item, item.Name.Values[0]}
			if code := writeJSON(stdout, result, stderr); code != 0 {
				return code
			}
		}
		return 0
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
	var desired packages.Packages
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

	if manager == nil {
		manager, err = packageBackend(platform.Detect)
		if err != nil {
			return reportError(stderr, err.Error())
		}
	}
	var actual packages.Packages
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
			packages.Packages
			InDesiredState bool `json:"_inDesiredState"`
		}{actual, len(differences) == 0}
		if code := writeJSON(stdout, result, stderr); code != 0 {
			return code
		}
		return writeJSON(stdout, differences, stderr)
	}
	return writeJSON(stdout, actual, stderr)
}

func readPackageExportFilter(input io.Reader) (packages.Packages, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxInputBytes+1))
	if err != nil {
		return packages.Packages{}, err
	}
	if len(data) > maxInputBytes {
		return packages.Packages{}, errors.New("input is too large")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return packages.Packages{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var filter packages.Packages
	if err := decoder.Decode(&filter); err != nil {
		return packages.Packages{}, err
	}
	var trailing any
	if !errors.Is(decoder.Decode(&trailing), io.EOF) {
		return packages.Packages{}, errors.New("expected a single JSON object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return packages.Packages{}, errors.New("filter must be a JSON object")
	}
	if len(object) > 0 && object["name"] == nil {
		return packages.Packages{}, errors.New("filter must include the package name identity")
	}
	if len(filter.Name.Values) == 0 && object["name"] != nil {
		return packages.Packages{}, errors.New("name filter must not be empty")
	}
	return filter, nil
}
