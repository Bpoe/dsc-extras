package apt

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type removalSnapshot struct {
	packages, automatic, config string
	active                      map[string]PackageState
	auto                        map[string]bool
}

func removalArguments(simulate, cleanup bool, targets []string) []string {
	args := []string{"-o", "Dpkg::Use-Pty=0", "-o", "APT::Cmd::Pattern-Only=true",
		"-o", "APT::Get::Purge=false", "-o", "APT::Get::AutomaticRemove=false", "--yes"}
	if simulate {
		args = append(args, "--simulate")
	}
	if cleanup {
		args = append(args, "--auto-remove")
	}
	return append(append(args, "remove", "--"), targets...)
}

// Unqualified APT identities are accepted only when the installed architecture
// is unambiguous. Never collapse two installed architectures into one name.
func removalIdentity(token string, active map[string]PackageState) (string, error) {
	if state, ok := active[token]; ok && strings.Contains(token, ":") {
		return state.Name + ":" + state.Architecture, nil
	}
	if !packageToken.MatchString(token) {
		return "", fmt.Errorf("unknown or ambiguous APT package identity %q", token)
	}
	var matches []string
	for identity, state := range active {
		if state.Name == token {
			matches = append(matches, identity)
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("unknown or ambiguous APT package identity %q", token)
	}
	return matches[0], nil
}

func (m *PackageManager) removalState(ctx context.Context) (removalSnapshot, error) {
	state := removalSnapshot{active: map[string]PackageState{}, auto: map[string]bool{}}
	output, err := m.run(ctx, "dpkg-query", "--show", packageQueryFormat)
	if err != nil {
		return state, err
	}
	state.packages = string(output)
	for _, line := range strings.Split(strings.TrimSuffix(state.packages, "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			return state, errors.New("cannot validate removal: malformed dpkg inventory")
		}
		name, _, _ := strings.Cut(fields[0], ":")
		status := strings.Fields(fields[1])
		if !packageToken.MatchString(name) || len(status) != 3 ||
			!slices.Contains([]string{"unknown", "install", "hold", "deinstall", "purge"}, status[0]) ||
			!slices.Contains([]string{"ok", "reinstreq"}, status[1]) ||
			!slices.Contains([]string{"not-installed", "config-files", "half-installed", "unpacked", "half-configured", "triggers-awaited", "triggers-pending", "installed"}, status[2]) {
			return state, errors.New("cannot validate removal: malformed dpkg inventory")
		}
		if status[2] == "not-installed" || status[2] == "config-files" {
			continue
		}
		if !packageArchitecture.MatchString(fields[3]) || fields[2] == "" ||
			(fields[0] != name && fields[0] != name+":"+fields[3]) {
			return state, errors.New("cannot validate removal: invalid installed identity")
		}
		identity := name + ":" + fields[3]
		if _, exists := state.active[identity]; exists {
			return state, fmt.Errorf("duplicate installed identity %q", identity)
		}
		state.active[identity] = PackageState{Name: name, Architecture: fields[3], Status: fields[1], Version: fields[2]}
	}
	output, err = m.run(ctx, "apt-mark", "showauto")
	if err != nil {
		return state, err
	}
	state.automatic = string(output)
	for _, token := range strings.Fields(state.automatic) {
		// apt-mark can also report residual/uninstalled packages. They cannot
		// authorize a removal, but their syntax must still be valid.
		parts := strings.Split(token, ":")
		if len(parts) > 2 || !packageToken.MatchString(parts[0]) ||
			(len(parts) == 2 && !packageArchitecture.MatchString(parts[1])) {
			return state, fmt.Errorf("cannot determine automatic-package status: invalid identity %q", token)
		}
		matches := 0
		for identity, installed := range state.active {
			if installed.Name == parts[0] && (len(parts) == 1 || installed.Architecture == parts[1]) {
				matches++
				state.auto[identity] = true
			}
		}
		if matches > 1 {
			return state, fmt.Errorf("cannot determine automatic-package status: ambiguous identity %q", token)
		}
	}
	output, err = m.run(ctx, "apt-config", "dump")
	if err != nil {
		return state, err
	}
	state.config = string(output)
	if strings.TrimSpace(state.config) == "" {
		return state, errors.New("cannot validate removal: empty APT configuration")
	}
	return state, nil
}

var removalOperation = regexp.MustCompile(`^Remv ([a-z0-9][a-z0-9+.:_-]*)(?: \[([^\s\]]+)\])?(?: \[[^\r\n]*\])?$`)
var removalSummary = regexp.MustCompile(`^0 upgraded, 0 newly installed, ([0-9]+) to remove and [0-9]+ not upgraded\.$`)

func parseRemoval(output []byte, state removalSnapshot) (map[string]bool, error) {
	removed := map[string]bool{}
	count, summaries := -1, 0
	for _, line := range strings.Split(strings.TrimSuffix(string(output), "\n"), "\n") {
		if match := removalOperation.FindStringSubmatch(line); match != nil {
			identity, err := removalIdentity(match[1], state.active)
			if err != nil {
				return nil, err
			}
			if removed[identity] || (match[2] != "" && match[2] != state.active[identity].Version) {
				return nil, fmt.Errorf("cannot validate APT removal operation %q", line)
			}
			removed[identity] = true
		} else if match := removalSummary.FindStringSubmatch(line); match != nil {
			var err error
			count, err = strconv.Atoi(match[1])
			if err != nil {
				return nil, err
			}
			summaries++
		} else if line == "" || strings.HasPrefix(line, "Reading package lists...") ||
			strings.HasPrefix(line, "Building dependency tree...") ||
			strings.HasPrefix(line, "Reading state information...") ||
			line == "The following packages will be REMOVED:" ||
			line == "The following package was automatically installed and is no longer required:" ||
			line == "The following packages were automatically installed and are no longer required:" ||
			line == "Use 'apt autoremove' to remove them." ||
			strings.HasPrefix(line, "  ") {
			continue
		} else {
			return nil, fmt.Errorf("cannot validate unexpected APT simulation output %q", line)
		}
	}
	if summaries != 1 || count != len(removed) {
		return nil, errors.New("cannot validate APT simulation: missing or inconsistent removal operations")
	}
	return removed, nil
}

func (m *PackageManager) removalPlan(ctx context.Context, targets []string, cleanup bool, state removalSnapshot) (map[string]bool, error) {
	output, err := m.run(ctx, "apt-get", removalArguments(true, cleanup, targets)...)
	if err != nil {
		return nil, err
	}
	plan, err := parseRemoval(output, state)
	if err != nil {
		return nil, err
	}
	requested := map[string]bool{}
	for _, target := range targets {
		requested[target] = true
		if !plan[target] {
			return nil, fmt.Errorf("APT simulation did not remove requested package %s", target)
		}
	}
	var unexpected []string
	for identity := range plan {
		if !requested[identity] && (!cleanup || !state.auto[identity]) {
			unexpected = append(unexpected, identity)
		}
	}
	if len(unexpected) != 0 {
		slices.Sort(unexpected)
		return nil, fmt.Errorf("cannot remove %s: APT would also remove %s, which was not explicitly requested. A dependent package requires the requested package, or cleanup would remove a manually installed package. Remove the dependent package explicitly or change the requested configuration",
			strings.Join(targets, ", "), strings.Join(unexpected, ", "))
	}
	return plan, nil
}

func (m *PackageManager) validateRemoval(ctx context.Context, targets []string, before []PackageState) error {
	state, err := m.removalState(ctx)
	if err != nil {
		return err
	}
	for _, observed := range before {
		if !packageActive(observed) {
			continue
		}
		current, ok := state.active[observed.Name+":"+observed.Architecture]
		if !ok || current.Version != observed.Version || current.Status != observed.Status {
			return errors.New("package state changed before removal planning; reconcile again")
		}
	}
	base, err := m.removalPlan(ctx, targets, false, state)
	if err != nil {
		return err
	}
	cleanup, err := m.removalPlan(ctx, targets, true, state)
	if err != nil {
		return err
	}
	// Recompute both plans and re-read package/mark/configuration state. These
	// checks detect changes, but simulations do NOT hold APT's exclusive lock:
	// an external writer can still race the final check and apt-get execution.
	for _, plan := range []struct {
		cleanup bool
		want    map[string]bool
	}{{false, base}, {true, cleanup}} {
		got, err := m.removalPlan(ctx, targets, plan.cleanup, state)
		if err != nil {
			return err
		}
		if !maps.Equal(got, plan.want) {
			return errors.New("APT removal plan changed during validation; reconcile again")
		}
	}
	current, err := m.removalState(ctx)
	if err != nil {
		return err
	}
	if current.packages != state.packages || current.automatic != state.automatic || current.config != state.config {
		return errors.New("APT package, automatic-mark, or configuration state changed during validation; reconcile again")
	}
	return nil
}
