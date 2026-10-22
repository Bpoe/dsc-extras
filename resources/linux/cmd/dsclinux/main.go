package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Bpoe/dsc-extras/resources/linux/internal/services"
)

const maxInputBytes = 1 << 20

var version = "0.1.0"

type serviceManager interface {
	Get(context.Context, services.Unit) (services.Unit, error)
	Test(context.Context, services.Unit) (services.Unit, []string, error)
	Set(context.Context, services.Unit) (services.Unit, error)
}

type testResult struct {
	services.Unit
	InDesiredState bool `json:"_inDesiredState"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, services.NewManager(nil, ""))
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, manager serviceManager) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, _ = fmt.Fprintln(stdout, "Usage: dsclinux <resource> <get|test|set>\nResources: services")
		return 0
	}
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	if len(args) != 2 || args[0] != "services" {
		return reportError(stderr, "expected services and one operation: get, test, or set")
	}
	if args[1] != "get" && args[1] != "test" && args[1] != "set" {
		return reportError(stderr, "unknown operation")
	}
	desired, err := readInput(stdin)
	if err != nil {
		return reportError(stderr, "invalid JSON input: "+err.Error())
	}
	var actual services.Unit
	var differences []string
	switch args[1] {
	case "get":
		actual, err = manager.Get(ctx, desired)
	case "test":
		actual, differences, err = manager.Test(ctx, desired)
	case "set":
		actual, err = manager.Set(ctx, desired)
	}
	if err != nil {
		return reportError(stderr, err.Error())
	}
	if args[1] == "test" {
		if code := writeJSON(stdout, testResult{Unit: actual, InDesiredState: len(differences) == 0}, stderr); code != 0 {
			return code
		}
		return writeJSON(stdout, differences, stderr)
	}
	return writeJSON(stdout, actual, stderr)
}

func readInput(input io.Reader) (services.Unit, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxInputBytes+1))
	if err != nil {
		return services.Unit{}, err
	}
	if len(data) > maxInputBytes {
		return services.Unit{}, errors.New("input is too large")
	}
	var instance services.Unit
	if err := json.Unmarshal(data, &instance); err != nil {
		return services.Unit{}, err
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
