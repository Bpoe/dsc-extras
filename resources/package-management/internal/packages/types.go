// Package packages defines the shared DSC wire contract and command boundary.
package packages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// PackageNames retains whether the caller supplied a scalar or an array.
type PackageNames struct {
	Values []string
	Scalar bool
}

func (n *PackageNames) UnmarshalJSON(data []byte) error {
	var scalar string
	if err := json.Unmarshal(data, &scalar); err == nil && !bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*n = PackageNames{Values: []string{scalar}, Scalar: true}
		return nil
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil || values == nil {
		return errors.New("name must be a string or an array of strings")
	}
	*n = PackageNames{Values: values}
	return nil
}

func (n PackageNames) MarshalJSON() ([]byte, error) {
	if n.Scalar {
		if len(n.Values) != 1 {
			return nil, errors.New("scalar name must contain exactly one value")
		}
		return json.Marshal(n.Values[0])
	}
	if n.Values == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(n.Values)
}

type Packages struct {
	Name           PackageNames   `json:"name"`
	Ensure         string         `json:"ensure,omitempty"`
	Version        string         `json:"version,omitempty"`
	AllowDowngrade bool           `json:"allowDowngrade,omitempty"`
	UpdateCache    bool           `json:"updateCache,omitempty"`
	State          string         `json:"state,omitempty"`
	Packages       []PackageState `json:"packages,omitempty"`
}

type PackageState struct {
	RequestedName string `json:"requestedName"`
	Name          string `json:"name"`
	Version       string `json:"version,omitempty"`
	Architecture  string `json:"architecture,omitempty"`
	Status        string `json:"status"`
	Installed     bool   `json:"installed"`
}

type Manager interface {
	Get(context.Context, Packages) (Packages, error)
	Test(context.Context, Packages) (Packages, []string, error)
	Set(context.Context, Packages) (Packages, error)
}

// Executor executes directly, without a shell. Environment entries override
// inherited variables; errors should expose ExitCode.
type Executor interface {
	Execute(context.Context, string, []string, []string) ([]byte, []byte, error)
}

type OSExecutor struct{}

func (OSExecutor) Execute(ctx context.Context, program string, args, env []string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, program, args...)
	command.Env = append(os.Environ(), env...)
	command.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type CommandError struct {
	Program        string
	Args           []string
	Stdout, Stderr string
	Err            error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("%s %q: %v (stdout: %s; stderr: %s)", e.Program, e.Args, e.Err, e.Stdout, e.Stderr)
}
func (e *CommandError) Unwrap() error { return e.Err }
func (e *CommandError) ExitCode() int {
	var exit interface{ ExitCode() int }
	if errors.As(e.Err, &exit) {
		return exit.ExitCode()
	}
	return -1
}
