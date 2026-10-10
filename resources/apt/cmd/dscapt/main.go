package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Bpoe/dsc-extras/resources/apt/internal/apt"
)

const (
	version       = "0.1.0"
	maxInputBytes = 1 << 20
)

type testResult struct {
	apt.Repository
	InDesiredState bool `json:"_inDesiredState"`
}

func main() {
	store := apt.NewStore("", "")
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, store))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, store apt.Store) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, _ = fmt.Fprintln(stdout, "Usage: dscapt <get|set|test>")
		return 0
	}
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	if len(args) != 1 {
		return reportError(stderr, "expected one operation: get, set, or test")
	}
	if args[0] != "get" && args[0] != "set" && args[0] != "test" {
		return reportError(stderr, "unknown operation")
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
		actual, err := store.Get(instance.Name)
		if err != nil {
			return reportError(stderr, err.Error())
		}
		return writeJSON(stdout, actual, stderr)
	case "test":
		actual, inDesiredState, err := store.Test(instance)
		if err != nil {
			return reportError(stderr, err.Error())
		}
		return writeJSON(stdout, testResult{
			Repository:     actual,
			InDesiredState: inDesiredState,
		}, stderr)
	default:
		actual, err := store.Set(instance)
		if err != nil {
			return reportError(stderr, err.Error())
		}
		return writeJSON(stdout, actual, stderr)
	}
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
