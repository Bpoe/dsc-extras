package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/apt"
	"github.com/Bpoe/dsc-extras/resources/package-management/internal/packages"
)

const maxInputBytes = 1 << 20

var version = "0.1.0"

type testResult struct {
	apt.Repository
	InDesiredState bool `json:"_inDesiredState"`
}

func main() {
	store := apt.NewStore("")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runWithPackages(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, store, nil)
	stop()
	os.Exit(code)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, store apt.Store) int {
	return runWithPackages(context.Background(), args, stdin, stdout, stderr, store, nil)
}

func runWithPackages(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, store apt.Store, packages packages.Manager) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, _ = fmt.Fprintln(stdout, "Usage: dscpm <resource> <get|set|test|export>\nResources: aptRepositories, packages")
		return 0
	}
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	handlers := map[string]func([]string, io.Reader, io.Writer, io.Writer) int{
		"aptRepositories": func(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
			return runAPTRepositories(args, stdin, stdout, stderr, store)
		},
		"packages": func(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
			return runPackages(ctx, args, stdin, stdout, stderr, packages)
		},
	}
	if len(args) == 0 {
		return reportError(stderr, "expected a resource type and operation")
	}
	handler, ok := handlers[args[0]]
	if !ok {
		return reportError(stderr, "unknown resource type")
	}
	return handler(args[1:], stdin, stdout, stderr)
}

func runAPTRepositories(args []string, stdin io.Reader, stdout, stderr io.Writer, store apt.Store) int {
	if len(args) != 1 {
		return reportError(stderr, "expected one operation: get, set, test, or export")
	}
	if args[0] != "get" && args[0] != "set" && args[0] != "test" && args[0] != "export" {
		return reportError(stderr, "unknown operation")
	}
	if args[0] == "export" {
		filter, err := readOptionalAPTInput(stdin)
		if err != nil {
			return reportError(stderr, "invalid export filter: "+err.Error())
		}
		items, err := store.Export(filter)
		if err != nil {
			return reportError(stderr, err.Error())
		}
		for _, item := range items {
			result := struct {
				apt.Repository
				ExportName string `json:"_name"`
			}{item, encodeExportName(item.Name)}
			if code := writeJSON(stdout, result, stderr); code != 0 {
				return code
			}
		}
		return 0
	}

	instance, err := readInput(stdin)
	if err != nil {
		return reportError(stderr, "invalid JSON input")
	}
	if err := apt.ValidateName(instance.Name); err != nil {
		return reportError(stderr, err.Error())
	}

	switch args[0] {
	case "get":
		actual, err := store.Get(instance)
		if err != nil {
			return reportError(stderr, err.Error())
		}
		return writeJSON(stdout, actual, stderr)
	case "test":
		actual, differences, err := store.Test(instance)
		if err != nil {
			return reportError(stderr, err.Error())
		}
		if code := writeJSON(stdout, testResult{
			Repository:     actual,
			InDesiredState: len(differences) == 0,
		}, stderr); code != 0 {
			return code
		}
		return writeJSON(stdout, differences, stderr)
	default:
		actual, err := store.Set(instance)
		if err != nil {
			return reportError(stderr, err.Error())
		}
		return writeJSON(stdout, actual, stderr)
	}
}

func readOptionalAPTInput(input io.Reader) (apt.Repository, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxInputBytes+1))
	if err != nil {
		return apt.Repository{}, err
	}
	if len(data) > maxInputBytes {
		return apt.Repository{}, errors.New("input is too large")
	}
	var instance apt.Repository
	if len(bytes.TrimSpace(data)) == 0 {
		return instance, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&instance); err != nil {
		return apt.Repository{}, err
	}
	var trailing any
	if !errors.Is(decoder.Decode(&trailing), io.EOF) {
		return apt.Repository{}, errors.New("expected a single JSON object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return apt.Repository{}, errors.New("filter must be a JSON object")
	}
	if len(object) != 0 {
		if _, ok := object["name"]; !ok {
			return apt.Repository{}, errors.New("filter must include its resource identity")
		}
	}
	return instance, nil
}

func readInput(input io.Reader) (apt.Repository, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxInputBytes+1))
	if err != nil {
		return apt.Repository{}, err
	}
	if len(data) > maxInputBytes {
		return apt.Repository{}, errors.New("input is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var instance apt.Repository
	if err := decoder.Decode(&instance); err != nil {
		return apt.Repository{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return apt.Repository{}, errors.New("multiple JSON values")
		}
		return apt.Repository{}, err
	}
	return instance, nil
}

func encodeExportName(name string) string {
	return strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
}

func writeJSON(stdout io.Writer, value any, stderr io.Writer) int {
	if err := json.NewEncoder(stdout).Encode(value); err != nil {
		return reportError(stderr, "failed to write JSON output")
	}
	return 0
}

func reportError(stderr io.Writer, message string) int {
	diagnostic, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: strings.TrimSpace(message)})
	_, _ = fmt.Fprintln(stderr, string(diagnostic))
	return 1
}
