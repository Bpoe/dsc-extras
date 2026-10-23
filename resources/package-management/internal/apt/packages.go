package apt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/packages"
)

type PackageNames = packages.PackageNames
type Packages = packages.Packages
type PackageState = packages.PackageState
type PackageExecutor = packages.Executor
type packageOSExecutor = packages.OSExecutor
type PackageCommandError = packages.CommandError

type PackageManager struct {
	executor PackageExecutor
}

func NewPackageManager(executor PackageExecutor) *PackageManager {
	if executor == nil {
		executor = packageOSExecutor{}
	}
	return &PackageManager{executor: executor}
}

func (m *PackageManager) run(ctx context.Context, program string, args ...string) ([]byte, error) {
	timeout := 30 * time.Second
	if program == "apt-get" {
		timeout = 15 * time.Minute
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := commandCtx.Err(); err != nil {
		return nil, err
	}
	stdout, stderr, err := m.executor.Execute(commandCtx, program, args,
		[]string{"LC_ALL=C", "LANG=C", "DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none"})
	if commandCtx.Err() != nil {
		err = errors.Join(err, commandCtx.Err())
	}
	if err != nil {
		return nil, &PackageCommandError{
			Program: program, Args: slices.Clone(args), Stdout: string(stdout), Stderr: string(stderr), Err: err,
		}
	}
	return stdout, nil
}

var packageToken = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
var packageArchitecture = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type resolvedPackage struct {
	input, name, architecture, version, argument string
	local                                        bool
	// missing marks a local archive that does not exist; only Test permits it,
	// and its package identity remains unknown.
	missing bool
}

// resolve validates and resolves the request. allowMissing permits local
// archives that do not exist (os.ErrNotExist) so Test can report noncompliance;
// Get and Set must not set it because they need the archive's identity.
func (m *PackageManager) resolve(ctx context.Context, desired Packages, allowMissing bool) (Packages, []resolvedPackage, error) {
	desired.Name.Values = slices.Clone(desired.Name.Values)
	slices.Sort(desired.Name.Values)
	desired.Packages, desired.State = nil, ""
	if desired.Ensure == "" {
		desired.Ensure = "Present"
	}
	if desired.Ensure != "Present" && desired.Ensure != "Absent" {
		return desired, nil, errors.New("ensure must be Present or Absent")
	}
	if desired.Ensure == "Absent" && (desired.AllowDowngrade || desired.UpdateCache) {
		return desired, nil, errors.New("allowDowngrade and updateCache require ensure Present")
	}
	if len(desired.Name.Values) == 0 || (desired.Name.Scalar && len(desired.Name.Values) != 1) {
		return desired, nil, errors.New("name must contain at least one package")
	}
	if desired.Version != "" && (!desired.Name.Scalar || len(desired.Name.Values) != 1 || desired.Ensure != "Present") {
		return desired, nil, errors.New("version requires one scalar repository package with ensure Present")
	}
	// Validate every argument before invoking even the read-only tools.
	resolved := make([]resolvedPackage, 0, len(desired.Name.Values))
	for index, input := range desired.Name.Values {
		if index > 0 && desired.Name.Values[index-1] == input {
			return desired, nil, fmt.Errorf("duplicate package input %q", input)
		}
		entry := resolvedPackage{input: input, argument: input}
		if strings.HasSuffix(input, ".deb") || strings.ContainsAny(input, `/\`) {
			if desired.Ensure == "Absent" || desired.Version != "" {
				return desired, nil, errors.New("local .deb inputs require ensure Present and no version")
			}
			if !strings.HasSuffix(input, ".deb") || strings.ContainsAny(input, "\x00\r\n") {
				return desired, nil, fmt.Errorf("invalid local .deb path %q", input)
			}
			path, err := filepath.Abs(input)
			if err != nil {
				return desired, nil, err
			}
			entry.local, entry.argument = true, path
			info, err := os.Stat(path)
			if allowMissing && errors.Is(err, os.ErrNotExist) {
				entry.missing = true
			} else if err != nil {
				return desired, nil, fmt.Errorf("local .deb %q: %w", input, err)
			} else if !info.Mode().IsRegular() {
				return desired, nil, fmt.Errorf("local .deb %q is not a regular file", input)
			}
		} else {
			parts := strings.Split(input, ":")
			if len(parts) > 2 || !packageToken.MatchString(parts[0]) {
				return desired, nil, fmt.Errorf("invalid repository package name %q", input)
			}
			entry.name = parts[0]
			if len(parts) == 2 {
				if !packageArchitecture.MatchString(parts[1]) {
					return desired, nil, fmt.Errorf("invalid architecture in %q", input)
				}
				entry.architecture = parts[1]
			}
			entry.version = desired.Version
		}
		resolved = append(resolved, entry)
	}
	nativeOutput, err := m.run(ctx, "dpkg", "--print-architecture")
	if err != nil {
		return desired, nil, err
	}
	native := strings.TrimSpace(string(nativeOutput))
	if !packageArchitecture.MatchString(native) || native == "all" {
		return desired, nil, errors.New("dpkg returned an invalid native architecture")
	}
	foreignOutput, err := m.run(ctx, "dpkg", "--print-foreign-architectures")
	if err != nil {
		return desired, nil, err
	}
	architectures := map[string]bool{native: true, "all": true}
	for _, arch := range strings.Fields(string(foreignOutput)) {
		if !packageArchitecture.MatchString(arch) {
			return desired, nil, errors.New("dpkg returned an invalid foreign architecture")
		}
		architectures[arch] = true
	}
	identities := map[string]bool{}
	allArchitectures := map[string]bool{}
	resolvedNames := map[string]bool{}
	for index := range resolved {
		entry := &resolved[index]
		if entry.missing {
			continue
		}
		if entry.local {
			output, err := m.run(ctx, "dpkg-deb", "--show", "--showformat=${Package}\t${Version}\t${Architecture}\n", "--", entry.argument)
			if err != nil {
				return desired, nil, err
			}
			fields := strings.Split(strings.TrimSuffix(string(output), "\n"), "\t")
			if len(fields) != 3 || !packageToken.MatchString(fields[0]) || !packageArchitecture.MatchString(fields[2]) {
				return desired, nil, fmt.Errorf("invalid .deb metadata for %q", entry.input)
			}
			entry.name, entry.version, entry.architecture = fields[0], fields[1], fields[2]
		}
		if entry.architecture == "" {
			entry.architecture = native
		}
		if !architectures[entry.architecture] {
			return desired, nil, fmt.Errorf("architecture %q is not native or registered foreign", entry.architecture)
		}
		if !entry.local {
			// A qualified name cannot be interpreted as APT's trailing +/- modifier.
			entry.argument = entry.name + ":" + entry.architecture
		}
		identityArchitecture := entry.architecture
		if identityArchitecture == "all" {
			identityArchitecture = native
		}
		identity := entry.name + ":" + identityArchitecture
		if identities[identity] || allArchitectures[entry.name] || (entry.architecture == "all" && resolvedNames[entry.name]) {
			return desired, nil, fmt.Errorf("duplicate resolved package identity %q", identity)
		}
		identities[identity] = true
		resolvedNames[entry.name] = true
		allArchitectures[entry.name] = entry.architecture == "all"
		if entry.version != "" {
			if strings.ContainsAny(entry.version, "\x00\r\n\t ") || strings.HasPrefix(entry.version, "-") {
				return desired, nil, errors.New("invalid package version")
			}
			if _, err := m.run(ctx, "dpkg", "--validate-version", entry.version); err != nil {
				return desired, nil, err
			}
			if !entry.local {
				entry.argument += "=" + entry.version
			}
		} else if entry.local {
			return desired, nil, errors.New("local .deb has an empty version")
		}
	}
	return desired, resolved, nil
}

const packageQueryFormat = "--showformat=${binary:Package}\t${Status}\t${Version}\t${Architecture}\n"

func (m *PackageManager) observe(ctx context.Context, entry resolvedPackage) (PackageState, error) {
	state := PackageState{RequestedName: entry.input, Name: entry.name}
	output, err := m.run(ctx, "dpkg-query", "--show", packageQueryFormat, "--", entry.name)
	if err != nil {
		var commandErr *PackageCommandError
		if errors.As(err, &commandErr) && commandErr.ExitCode() == 1 && commandErr.Stdout == "" &&
			!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
			strings.TrimSpace(commandErr.Stderr) == "dpkg-query: no packages found matching "+entry.name {
			return state, nil
		}
		return state, err
	}
	found := false
	for _, line := range strings.Split(strings.TrimSuffix(string(output), "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 || (fields[0] != entry.name && fields[0] != entry.name+":"+fields[3]) {
			return state, fmt.Errorf("unexpected dpkg-query output for %q", entry.name)
		}
		status := strings.Fields(fields[1])
		if len(status) != 3 || !slices.Contains([]string{"unknown", "install", "hold", "deinstall", "purge"}, status[0]) ||
			!slices.Contains([]string{"ok", "reinstreq"}, status[1]) ||
			!slices.Contains([]string{"not-installed", "config-files", "half-installed", "unpacked", "half-configured", "triggers-awaited", "triggers-pending", "installed"}, status[2]) {
			return state, fmt.Errorf("unexpected dpkg status for %q", entry.name)
		}
		if fields[3] == "" && status[2] == "not-installed" {
			continue
		}
		if !packageArchitecture.MatchString(fields[3]) {
			return state, fmt.Errorf("unexpected dpkg architecture for %q", entry.name)
		}
		if fields[3] != entry.architecture && (entry.local || fields[3] != "all") {
			continue
		}
		if found {
			return state, fmt.Errorf("ambiguous installed architecture for %q", entry.name)
		}
		found = true
		state.Status, state.Version, state.Architecture = fields[1], fields[2], fields[3]
		state.Installed = status[1] == "ok" && status[2] == "installed"
	}
	return state, nil
}

func (m *PackageManager) Export(ctx context.Context, filter Packages) ([]Packages, error) {
	seenFilters := map[string]bool{}
	for _, requested := range filter.Name.Values {
		if seenFilters[requested] {
			return nil, fmt.Errorf("duplicate package export filter %q", requested)
		}
		seenFilters[requested] = true
		name := requested
		if before, _, qualified := strings.Cut(requested, ":"); qualified {
			name = before
		}
		if !packageToken.MatchString(name) || strings.Contains(requested, ":") &&
			!packageArchitecture.MatchString(strings.SplitN(requested, ":", 2)[1]) {
			return nil, fmt.Errorf("invalid package export filter %q", requested)
		}
	}
	nativeOutput, err := m.run(ctx, "dpkg", "--print-architecture")
	if err != nil {
		return nil, err
	}
	native := strings.TrimSpace(string(nativeOutput))
	if !packageArchitecture.MatchString(native) || native == "all" {
		return nil, errors.New("dpkg returned an invalid native architecture")
	}
	output, err := m.run(ctx, "dpkg-query", "--show", "--showformat=${binary:Package}\t${Status}\t${Version}\t${Architecture}\n")
	if err != nil {
		return nil, err
	}
	type installedPackage struct{ name, version, architecture string }
	var installed []installedPackage
	archCount := map[string]int{}
	for _, line := range strings.Split(strings.TrimSuffix(string(output), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			return nil, fmt.Errorf("invalid dpkg package inventory row %q", line)
		}
		status := strings.Fields(fields[1])
		if len(status) != 3 {
			return nil, fmt.Errorf("invalid dpkg package status for %q", fields[0])
		}
		if (status[0] != "install" && status[0] != "hold") || status[1] != "ok" || status[2] != "installed" {
			continue
		}
		name := strings.TrimSuffix(fields[0], ":"+fields[3])
		if !packageToken.MatchString(name) || !packageArchitecture.MatchString(fields[3]) || fields[2] == "" {
			return nil, fmt.Errorf("invalid installed dpkg package identity %q", fields[0])
		}
		installed = append(installed, installedPackage{name, fields[2], fields[3]})
		archCount[name]++
	}
	for _, item := range installed {
		if item.architecture == "all" && archCount[item.name] > 1 {
			return nil, fmt.Errorf("cannot export %q installed as architecture all alongside another architecture; package identity is ambiguous", item.name)
		}
	}
	slices.SortFunc(installed, func(a, b installedPackage) int {
		if order := strings.Compare(a.name, b.name); order != 0 {
			return order
		}
		return strings.Compare(a.architecture, b.architecture)
	})
	result := make([]Packages, 0, len(installed))
	for _, item := range installed {
		name := item.name
		if item.architecture != "all" && (item.architecture != native || archCount[name] > 1) {
			name += ":" + item.architecture
		}
		if filter.Name.Scalar || len(filter.Name.Values) > 0 {
			matched := false
			for _, requested := range filter.Name.Values {
				if requested == name || requested == item.name || requested == item.name+":"+item.architecture {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		result = append(result, Packages{
			Name:    PackageNames{Values: []string{name}, Scalar: true},
			Version: item.version,
		})
	}
	return result, nil
}

func packageActive(state PackageState) bool {
	fields := strings.Fields(state.Status)
	return len(fields) == 3 && fields[2] != "not-installed" && fields[2] != "config-files"
}

func (m *PackageManager) compare(ctx context.Context, left, operator, right string) (bool, error) {
	_, err := m.run(ctx, "dpkg", "--compare-versions", left, operator, right)
	if err == nil {
		return true, nil
	}
	var commandErr *PackageCommandError
	if errors.As(err, &commandErr) && commandErr.ExitCode() == 1 &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return false, nil
	}
	return false, err
}

func (m *PackageManager) inspect(ctx context.Context, desired Packages, entries []resolvedPackage) (Packages, []bool, []string, error) {
	result := desired
	result.Packages = make([]PackageState, 0, len(entries))
	compliant := make([]bool, len(entries))
	allInstalled, allAbsent, anyMissing := true, true, false
	differences := make([]string, 0)
	for index, entry := range entries {
		if entry.missing {
			// The archive's package identity, version, and architecture cannot be
			// determined, so it cannot be confirmed as installed.
			anyMissing = true
			result.Packages = append(result.Packages, PackageState{RequestedName: entry.input})
			if !slices.Contains(differences, "ensure") {
				differences = append(differences, "ensure")
			}
			continue
		}
		state, err := m.observe(ctx, entry)
		if err != nil {
			return result, nil, nil, err
		}
		result.Packages = append(result.Packages, state)
		allInstalled = allInstalled && state.Installed
		allAbsent = allAbsent && !packageActive(state)
		if desired.Ensure == "Absent" {
			compliant[index] = !packageActive(state)
		} else {
			compliant[index] = state.Installed
		}
		if !compliant[index] && !slices.Contains(differences, "ensure") {
			differences = append(differences, "ensure")
		}
		if desired.Ensure == "Present" && state.Installed && entry.version != "" {
			equal, err := m.compare(ctx, state.Version, "eq", entry.version)
			if err != nil {
				return result, nil, nil, err
			}
			if !equal {
				compliant[index] = false
				if !slices.Contains(differences, "version") {
					differences = append(differences, "version")
				}
			}
		}
	}
	switch {
	case anyMissing && (allInstalled || allAbsent):
		// A missing archive's installation is unknown; omit the aggregate
		// unless the observed packages are already mixed.
		result.State = ""
	case allInstalled:
		result.State = "Present"
	case allAbsent:
		result.State = "Absent"
	default:
		result.State = "Mixed"
	}
	slices.Sort(differences)
	return result, compliant, differences, nil
}

func (m *PackageManager) Get(ctx context.Context, desired Packages) (Packages, error) {
	desired, entries, err := m.resolve(ctx, desired, false)
	if err != nil {
		return desired, err
	}
	result, _, _, err := m.inspect(ctx, desired, entries)
	return result, err
}

// Test reports a missing local archive as noncompliant rather than an error, so
// a configuration can be tested before a dependency downloads the archive.
func (m *PackageManager) Test(ctx context.Context, desired Packages) (Packages, []string, error) {
	desired, entries, err := m.resolve(ctx, desired, true)
	if err != nil {
		return desired, nil, err
	}
	result, _, differences, err := m.inspect(ctx, desired, entries)
	return result, differences, err
}

func (m *PackageManager) Set(ctx context.Context, desired Packages) (Packages, error) {
	desired, entries, err := m.resolve(ctx, desired, false)
	if err != nil {
		return desired, err
	}
	before, compliant, differences, err := m.inspect(ctx, desired, entries)
	if err != nil || len(differences) == 0 {
		return before, err
	}
	var arguments []string
	for index, entry := range entries {
		if compliant[index] {
			if desired.Ensure == "Present" && entry.local {
				// Keep dependency resolution from upgrading an exact artifact that
				// already complies; this pins the installed version, not a reinstall.
				state := before.Packages[index]
				arguments = append(arguments, entry.name+":"+state.Architecture+"="+state.Version)
			}
			continue
		}
		if desired.Ensure == "Present" && entry.version != "" && packageActive(before.Packages[index]) && before.Packages[index].Version != "" {
			downgrade, err := m.compare(ctx, before.Packages[index].Version, "gt", entry.version)
			if err != nil {
				return before, err
			}
			if downgrade && !desired.AllowDowngrade {
				return before, fmt.Errorf("downgrade of %q requires allowDowngrade", entry.input)
			}
		}
		if desired.Ensure == "Absent" {
			// An unqualified removal could remove other installed architectures.
			arguments = append(arguments, entry.name+":"+before.Packages[index].Architecture)
		} else {
			arguments = append(arguments, entry.argument)
		}
	}
	if desired.Ensure == "Present" && desired.UpdateCache {
		if _, err := m.run(ctx, "apt-get", "-o", "Dpkg::Use-Pty=0", "-o", "APT::Update::Error-Mode=any", "update"); err != nil {
			return before, err
		}
	}
	// Disable APT's legacy regex fallback for literal names containing dots/pluses.
	args := []string{"-o", "Dpkg::Use-Pty=0", "-o", "APT::Cmd::Pattern-Only=true", "--yes"}
	if desired.Ensure == "Absent" {
		if err := m.validateRemoval(ctx, arguments, before.Packages); err != nil {
			return before, err
		}
		args = removalArguments(false, true, arguments)
	} else {
		args = append(args, "--no-remove")
		if desired.AllowDowngrade {
			args = append(args, "--allow-downgrades")
		}
		args = append(args, "install")
	}
	if desired.Ensure != "Absent" {
		args = append(args, "--")
		args = append(args, arguments...)
	}
	if _, err := m.run(ctx, "apt-get", args...); err != nil {
		return before, err
	}
	after, _, remaining, err := m.inspect(ctx, desired, entries)
	if err != nil {
		return after, err
	}
	if len(remaining) != 0 {
		return after, fmt.Errorf("APT transaction did not achieve requested state: %s", strings.Join(remaining, ", "))
	}
	return after, nil
}
