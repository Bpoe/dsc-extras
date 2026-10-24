package dnf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/packages"
)

func (m *PackageManager) Get(ctx context.Context, desired packages.Packages) (packages.Packages, error) {
	result, _, err := m.Test(ctx, desired)
	return result, err
}

func (m *PackageManager) Export(ctx context.Context, filter packages.Packages) ([]packages.Packages, error) {
	seenFilters := map[string]bool{}
	for _, requested := range filter.Name.Values {
		if seenFilters[requested] {
			return nil, fmt.Errorf("duplicate package export filter %q", requested)
		}
		seenFilters[requested] = true
		name, arch, qualified := strings.Cut(requested, ":")
		if !nameToken.MatchString(name) || qualified && !archToken.MatchString(arch) {
			return nil, fmt.Errorf("invalid package export filter %q", requested)
		}
	}
	inventory, err := m.inventory(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]packages.Packages, 0, len(inventory))
	for _, item := range inventory {
		name := item.name + ":" + item.arch
		if filter.Name.Scalar || len(filter.Name.Values) > 0 {
			matched := false
			for _, requested := range filter.Name.Values {
				if requested == item.name || requested == name {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		result = append(result, packages.Packages{
			Name:    packages.PackageNames{Values: []string{name}, Scalar: true},
			Ensure:  "Present",
			Version: item.evr(),
			State:   "Present",
			Packages: []packages.PackageState{
				item.observed(name),
			},
		})
	}
	return result, nil
}

func (m *PackageManager) Test(ctx context.Context, desired packages.Packages) (packages.Packages, []string, error) {
	desired, entries, err := m.resolve(ctx, desired)
	if err != nil {
		return desired, nil, err
	}
	inventory, err := m.inventory(ctx)
	if err != nil {
		return desired, nil, err
	}
	result, _, differences, err := m.inspect(ctx, desired, entries, inventory)
	return result, differences, err
}

func (m *PackageManager) Set(ctx context.Context, desired packages.Packages) (result packages.Packages, err error) {
	desired, entries, err := m.resolve(ctx, desired)
	if err != nil {
		return desired, err
	}
	before, err := m.inventory(ctx)
	if err != nil {
		return desired, err
	}
	result, compliant, differences, err := m.inspect(ctx, desired, entries, before)
	if err != nil || len(differences) == 0 {
		return result, err
	}
	var arguments []string
	for i, entry := range entries {
		if compliant[i] {
			continue
		}
		for _, installed := range before {
			if entry.version == "" || !entry.matches(installed) {
				continue
			}
			cmp, err := m.compare(ctx, installed, entry.identity)
			if err != nil {
				return result, err
			}
			if cmp > 0 && !desired.AllowDowngrade {
				return result, fmt.Errorf("downgrade of %q requires allowDowngrade", entry.input)
			}
		}
		arguments = append(arguments, entry.argument())
	}
	output, err := m.run(ctx, "dnf", "--version")
	if err != nil {
		return result, err
	}
	version := strings.Fields(string(output))
	if len(version) == 0 || !strings.HasPrefix(version[0], "4.") {
		return result, fmt.Errorf("safe installation requires the tested DNF 4 CLI shipped with RHEL 8/9/10; found %q", output)
	}

	// Native protection is defense in depth, not a replacement for preflight:
	// some libdnf versions still allow an obsoleting provider. A file avoids
	// OS argument-length limits on large installed inventories.
	protection, err := os.CreateTemp("", "dscpm-protected-*.conf")
	if err != nil {
		return result, fmt.Errorf("create DNF package protection: %w", err)
	}
	defer func() {
		if removeErr := os.Remove(protection.Name()); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary DNF protection: %w", removeErr))
		}
	}()
	names := []string{"dnf"}
	for _, p := range before {
		names = append(names, p.name)
	}
	slices.Sort(names)
	_, writeErr := protection.WriteString(strings.Join(slices.Compact(names), "\n") + "\n")
	closeErr := protection.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return result, fmt.Errorf("write DNF package protection: %w", err)
	}
	options := []string{
		"--color=never", "--setopt=debuglevel=2",
		"--setopt=obsoletes=False", "--setopt=installonly_limit=0",
		"--setopt=clean_requirements_on_remove=False", "--setopt=strict=True",
		"--setopt=skip_if_unavailable=False", "--setopt=*.skip_if_unavailable=False",
		"--setopt=ignorearch=False",
		"--setopt=protected_packages=glob:" + protection.Name(),
	}
	planOptions := slices.Clone(options)
	if desired.UpdateCache {
		planOptions = append(planOptions, "--refresh")
	}
	plan, err := m.preflight(ctx, planOptions, arguments)
	if err != nil {
		return result, err
	}
	if err := m.validatePlan(ctx, plan, entries, compliant, desired.AllowDowngrade); err != nil {
		return result, err
	}
	// Replace repository specs with the exact resolved NEVRAs. The preflight
	// has verified identities rather than trusting DNF's capability matching.
	arguments = nil
	for i, entry := range entries {
		if compliant[i] {
			continue
		}
		if entry.path != "" {
			arguments = append(arguments, entry.path)
		} else {
			for _, action := range plan {
				if entry.matches(action.identity) {
					arguments = append(arguments, action.nevra())
				}
			}
		}
	}
	repeated, err := m.preflight(ctx, options, arguments)
	if err != nil {
		return result, err
	}
	if !slices.Equal(plan, repeated) {
		return result, errors.New("DNF transaction plan changed before installation; retry after serializing package maintenance")
	}
	for _, entry := range entries {
		if entry.path != "" {
			current, err := m.local(ctx, entry.path)
			if err != nil {
				return result, err
			}
			if current != entry.identity {
				return result, fmt.Errorf("local RPM metadata changed before installation: %q", entry.input)
			}
		}
	}
	current, err := m.inventory(ctx)
	if err != nil {
		return result, err
	}
	if !slices.Equal(before, current) {
		return result, errors.New("RPM inventory changed before installation; retry after serializing package maintenance")
	}
	args := append(slices.Clone(options), "--assumeyes", "--setopt=assumeno=False", "install", "--")
	args = append(args, arguments...)
	if _, err := m.run(ctx, "dnf", args...); err != nil {
		return result, err
	}
	after, err := m.inventory(ctx)
	if err != nil {
		return result, err
	}
	retained := map[string]bool{}
	for _, p := range after {
		retained[p.key()] = true
	}
	for _, p := range before {
		if !retained[p.key()] {
			return result, fmt.Errorf("DNF unexpectedly removed installed identity %q; inspect the host before reconciling again", p.key())
		}
	}
	result, _, differences, err = m.inspect(ctx, desired, entries, after)
	if err == nil && len(differences) != 0 {
		err = fmt.Errorf("DNF transaction did not achieve requested state: %s", strings.Join(differences, ", "))
	}
	return result, err
}
