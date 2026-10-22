package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

type Unit struct {
	Name          string  `json:"name"`
	State         *string `json:"state,omitempty"`
	Enabled       *bool   `json:"enabled,omitempty"`
	Masked        *bool   `json:"masked,omitempty"`
	Scope         string  `json:"scope"`
	ActiveState   string  `json:"activeState,omitempty"`
	SubState      string  `json:"subState,omitempty"`
	LoadState     string  `json:"loadState,omitempty"`
	UnitFileState string  `json:"unitFileState,omitempty"`
}

type Executor interface {
	Execute(context.Context, []string) ([]byte, []byte, error)
}

type systemctlExecutor struct{}

func (systemctlExecutor) Execute(ctx context.Context, args []string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, "systemctl", args...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type Manager struct {
	executor Executor
	root     string
}

// root redirects persistent filesystem inspection for isolated tests.
func NewManager(executor Executor, root string) *Manager {
	if executor == nil {
		executor = systemctlExecutor{}
	}
	return &Manager{executor: executor, root: root}
}

var unitName = regexp.MustCompile(`^[A-Za-z0-9:_.@\\-]+$`)

var unitTypes = []string{
	".service", ".timer", ".socket", ".mount", ".target", ".automount",
	".path", ".slice", ".swap", ".device", ".scope",
}

func normalize(desired Unit) (Unit, string, error) {
	name := desired.Name
	if name == "" || !unitName.MatchString(name) || strings.Count(name, "@") > 1 || strings.HasPrefix(name, "@") {
		return desired, "", errors.New("name must be a literal systemd unit name, not a path, pattern, or option")
	}
	if !slices.Contains(unitTypes, "."+suffix(name)) {
		name += ".service"
	}
	if len(name) > 255 || len(name) == len("."+suffix(name)) {
		return desired, "", errors.New("unit name must have a nonempty prefix and be at most 255 bytes")
	}
	if desired.State != nil && *desired.State != "active" && *desired.State != "inactive" {
		return desired, "", errors.New("state must be active or inactive; restart and reload are not desired states")
	}
	if desired.Scope == "" {
		desired.Scope = "system"
	}
	if desired.Scope != "system" && desired.Scope != "user" {
		return desired, "", errors.New("scope must be system or user")
	}
	return desired, name, nil
}

func suffix(name string) string {
	if index := strings.LastIndexByte(name, '.'); index >= 0 {
		return name[index+1:]
	}
	return ""
}

func (m *Manager) run(ctx context.Context, scope, operation string, args ...string) ([]byte, error) {
	command := []string{"--" + scope, "--no-pager", "--no-ask-password", operation}
	command = append(command, args...)
	stdout, stderr, err := m.executor.Execute(ctx, command)
	if err != nil {
		hint := ""
		if scope == "user" {
			hint = "; run as the intended logged-in user with a running systemd user manager and valid XDG_RUNTIME_DIR/DBUS_SESSION_BUS_ADDRESS (no implicit user switching)"
		}
		return nil, fmt.Errorf("systemctl %s %s: %w: %s%s", operation, strings.Join(args, " "), err, strings.TrimSpace(string(stderr)), hint)
	}
	return stdout, nil
}

type observation struct {
	Unit
	needReload  bool
	runtimeMask bool
	enablement  []configLink
	maskSource  string
}

func (m *Manager) observe(ctx context.Context, desired Unit, name string) (observation, error) {
	output, err := m.run(ctx, desired.Scope, "show",
		"--property=Id,Names,ActiveState,SubState,LoadState,UnitFileState,NeedDaemonReload,FragmentPath", "--", name)
	if err != nil {
		return observation{}, err
	}
	properties := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			properties[key] = strings.TrimSpace(value)
		}
	}
	_, hasUnitFileState := properties["UnitFileState"]
	if properties["ActiveState"] == "" || properties["LoadState"] == "" || !hasUnitFileState {
		return observation{}, errors.New("systemctl show did not return ActiveState, LoadState and UnitFileState")
	}
	names := strings.Fields(properties["Names"])
	names = append(names, name)
	if properties["Id"] != "" {
		names = append(names, properties["Id"])
	}
	enabled, enablement, err := m.persistentEnablement(desired.Scope, names)
	if err != nil {
		return observation{}, err
	}
	// UnitFileState distinguishes persistent/runtime masks, including template
	// and empty-file masks. Only its hidden enablement requires filesystem reads.
	masked := properties["UnitFileState"] == "masked"
	actual := Unit{
		Name: desired.Name, Scope: desired.Scope, Enabled: &enabled, Masked: &masked,
		ActiveState: properties["ActiveState"], SubState: properties["SubState"],
		LoadState: properties["LoadState"], UnitFileState: properties["UnitFileState"],
	}
	if actual.ActiveState == "active" || actual.ActiveState == "inactive" {
		actual.State = &actual.ActiveState
	}
	return observation{
		Unit: actual, needReload: properties["NeedDaemonReload"] == "yes",
		runtimeMask: properties["UnitFileState"] == "masked-runtime",
		enablement:  enablement,
		maskSource:  properties["FragmentPath"],
	}, nil
}

func (m *Manager) Get(ctx context.Context, desired Unit) (Unit, error) {
	desired, name, err := normalize(desired)
	if err != nil {
		return Unit{}, err
	}
	actual, err := m.observe(ctx, desired, name)
	return actual.Unit, err
}

func differences(desired, actual Unit) []string {
	result := []string{}
	if desired.State != nil && *desired.State != actual.ActiveState {
		result = append(result, "state")
	}
	if desired.Enabled != nil && *desired.Enabled != *actual.Enabled {
		result = append(result, "enabled")
	}
	if desired.Masked != nil && *desired.Masked != *actual.Masked {
		result = append(result, "masked")
	}
	return result
}

func (m *Manager) Test(ctx context.Context, desired Unit) (Unit, []string, error) {
	actual, err := m.Get(ctx, desired)
	if err != nil {
		return Unit{}, nil, err
	}
	return actual, differences(desired, actual), nil
}

func (m *Manager) Set(ctx context.Context, desired Unit) (result Unit, resultErr error) {
	desired, name, err := normalize(desired)
	if err != nil {
		return Unit{}, err
	}
	actual, err := m.observe(ctx, desired, name)
	if err != nil {
		return Unit{}, err
	}
	if len(differences(desired, actual.Unit)) == 0 {
		return actual.Unit, nil
	}
	if actual.needReload || actual.LoadState == "not-found" {
		if _, err := m.run(ctx, desired.Scope, "daemon-reload"); err != nil {
			return Unit{}, err
		}
		actual, err = m.observe(ctx, desired, name)
		if err != nil {
			return Unit{}, err
		}
	}
	if len(differences(desired, actual.Unit)) == 0 {
		return actual.Unit, nil
	}
	if actual.LoadState == "not-found" {
		return Unit{}, fmt.Errorf("unit %s was not found; install its definition before managing it", name)
	}
	if actual.runtimeMask && ((desired.State != nil && *desired.State == "active" && actual.ActiveState != "active") ||
		(desired.Enabled != nil && *desired.Enabled && !*actual.Enabled)) {
		return Unit{}, fmt.Errorf("unit %s has a runtime mask; remove that transient mask explicitly before this persistent transition", name)
	}

	wantMask := *actual.Masked
	if desired.Masked != nil {
		wantMask = *desired.Masked
	}
	changeEnabled := desired.Enabled != nil && *desired.Enabled != *actual.Enabled
	changeState := desired.State != nil && *desired.State != actual.ActiveState
	unmask := *actual.Masked && (!wantMask || changeEnabled || (changeState && *desired.State == "active"))
	if unmask && actual.maskSource != "" && filepath.Base(actual.maskSource) != name {
		return Unit{}, fmt.Errorf("unit %s inherits its mask from %s; remove that mask explicitly using its defining unit name to avoid changing other units", name, actual.maskSource)
	}
	remask := false
	// A failed transition must not leave an unmanaged (or requested) mask removed.
	defer func() {
		if !remask {
			return
		}
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, restoreErr := m.run(restoreCtx, desired.Scope, "mask", "--", name)
		if restoreErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("restore persistent mask: %w", restoreErr))
		}
	}()
	if unmask {
		remask = wantMask
		_, unmaskErr := m.run(ctx, desired.Scope, "unmask", "--", name)
		var restoreErr error
		if desired.Enabled == nil || *desired.Enabled {
			var restored bool
			restored, restoreErr = restoreEnablement(actual.enablement)
			if restored {
				_, reloadErr := m.run(ctx, desired.Scope, "daemon-reload")
				restoreErr = errors.Join(restoreErr, reloadErr)
			}
		}
		if err := errors.Join(unmaskErr, restoreErr); err != nil {
			return Unit{}, err
		}
	}
	if changeEnabled {
		operation := "disable"
		if *desired.Enabled {
			operation = "enable"
		}
		if _, err := m.run(ctx, desired.Scope, operation, "--", name); err != nil {
			return Unit{}, err
		}
	}
	if changeState {
		operation := "stop"
		if *desired.State == "active" {
			operation = "start"
		}
		if _, err := m.run(ctx, desired.Scope, operation, "--", name); err != nil {
			return Unit{}, err
		}
	}
	if wantMask && (!*actual.Masked || unmask) {
		if _, err := m.run(ctx, desired.Scope, "mask", "--", name); err != nil {
			return Unit{}, err
		}
		remask = false
	}
	after, err := m.observe(ctx, desired, name)
	if err != nil {
		return Unit{}, err
	}
	if desired.State != nil && *desired.State == "inactive" && after.ActiveState == "failed" {
		if _, err := m.run(ctx, desired.Scope, "reset-failed", "--", name); err != nil {
			return Unit{}, err
		}
		after, err = m.observe(ctx, desired, name)
		if err != nil {
			return Unit{}, err
		}
	}
	// Omitted persistent properties must survive transitions, too.
	expected := desired
	if expected.Enabled == nil {
		expected.Enabled = actual.Enabled
	}
	if expected.Masked == nil {
		expected.Masked = actual.Masked
	}
	if diff := differences(expected, after.Unit); len(diff) != 0 {
		hint := ""
		if after.UnitFileState == "static" && expected.Enabled != nil && *expected.Enabled {
			hint = "; static units cannot be enabled without install rules"
		}
		return Unit{}, fmt.Errorf("unit %s did not converge (or an unmanaged property changed): %s; ActiveState=%s, LoadState=%s, UnitFileState=%s%s",
			name, strings.Join(diff, ", "), after.ActiveState, after.LoadState, after.UnitFileState, hint)
	}
	return after.Unit, nil
}
