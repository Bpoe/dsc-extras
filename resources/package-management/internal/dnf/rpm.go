package dnf

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

type PackageManager struct{ executor packages.Executor }

func NewPackageManager(executor packages.Executor) *PackageManager {
	if executor == nil {
		executor = packages.OSExecutor{}
	}
	return &PackageManager{executor: executor}
}

func (m *PackageManager) run(ctx context.Context, program string, args ...string) ([]byte, error) {
	timeout := 30 * time.Second
	if program == "dnf" {
		timeout = 15 * time.Minute
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := commandCtx.Err(); err != nil {
		return nil, err
	}
	stdout, stderr, err := m.executor.Execute(commandCtx, program, args,
		[]string{"LC_ALL=C", "LANG=C", "COLUMNS=4096"})
	if commandCtx.Err() != nil {
		err = errors.Join(err, commandCtx.Err())
	}
	if err != nil {
		return stdout, &packages.CommandError{
			Program: program, Args: slices.Clone(args), Stdout: string(stdout), Stderr: string(stderr), Err: err,
		}
	}
	return stdout, nil
}

var (
	nameToken  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	archToken  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]*$`)
	evrToken   = regexp.MustCompile(`^[A-Za-z0-9._+~^]+$`)
	epochToken = regexp.MustCompile(`^[0-9]+$`)
)

type identity struct{ name, epoch, version, release, arch string }

func (p identity) evr() string   { return p.epoch + ":" + p.version + "-" + p.release }
func (p identity) nevra() string { return p.name + "-" + p.evr() + "." + p.arch }
func (p identity) key() string   { return p.name + ":" + p.arch }

func (p identity) observed(input string) packages.PackageState {
	return packages.PackageState{
		RequestedName: input, Name: p.name, Version: p.evr(), Architecture: p.arch,
		Installed: true, Status: "installed",
	}
}

func normalizeEpoch(epoch string) (string, error) {
	if epoch == "" || epoch == "(none)" {
		return "0", nil
	}
	if !epochToken.MatchString(epoch) {
		return "", fmt.Errorf("invalid RPM epoch %q", epoch)
	}
	epoch = strings.TrimLeft(epoch, "0")
	if epoch == "" {
		epoch = "0"
	}
	return epoch, nil
}

func parseEVR(value string) (identity, error) {
	var p identity
	rest := value
	epoch := "0"
	if before, after, ok := strings.Cut(value, ":"); ok {
		epoch, rest = before, after
		if !epochToken.MatchString(epoch) {
			return p, fmt.Errorf("invalid RPM EVR %q: epoch must be numeric", value)
		}
	}
	var ok bool
	p.version, p.release, ok = strings.Cut(rest, "-")
	if !ok || !evrToken.MatchString(p.version) || !evrToken.MatchString(p.release) {
		return p, fmt.Errorf("invalid RPM EVR %q: expected [epoch:]version-release, without ranges or wildcards", value)
	}
	p.epoch, _ = normalizeEpoch(epoch)
	return p, nil
}

// RPM's embedded Lua exposes native rpmvercmp even on RHEL 8. Only validated
// literal components enter the expression: quotes, macros and code are rejected.
func (m *PackageManager) compare(ctx context.Context, left, right identity) (int, error) {
	if left.evr() == right.evr() {
		return 0, nil
	}
	for _, p := range []identity{left, right} {
		if _, err := parseEVR(p.evr()); err != nil {
			return 0, err
		}
	}
	expression := fmt.Sprintf(`%%{lua:local c = rpm.vercmp("%s", "%s"); if c == 0 then c = rpm.vercmp("%s", "%s") end; if c == 0 then c = rpm.vercmp("%s", "%s") end; print(c)}`,
		left.epoch, right.epoch, left.version, right.version, left.release, right.release)
	output, err := m.run(ctx, "rpm", "--eval", expression)
	if err != nil {
		return 0, err
	}
	switch strings.TrimSpace(string(output)) {
	case "-1":
		return -1, nil
	case "0":
		return 0, nil
	case "1":
		return 1, nil
	default:
		return 0, fmt.Errorf("RPM native version comparison unavailable or invalid: %q", output)
	}
}

const queryFormat = "%{NAME}\t%{EPOCH}\t%{VERSION}\t%{RELEASE}\t%{ARCH}\n"

func parseMetadata(output []byte, inventory bool) ([]identity, error) {
	var result []identity
	for _, line := range strings.Split(strings.TrimSuffix(string(output), "\n"), "\n") {
		if line == "" && len(output) == 0 && inventory {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 5 {
			return nil, fmt.Errorf("invalid RPM metadata row %q", line)
		}
		// Imported signing keys are rpmdb records, not installed packages.
		if inventory && fields[0] == "gpg-pubkey" && fields[4] == "(none)" {
			continue
		}
		if !nameToken.MatchString(fields[0]) || !archToken.MatchString(fields[4]) ||
			!evrToken.MatchString(fields[2]) || !evrToken.MatchString(fields[3]) {
			return nil, fmt.Errorf("invalid RPM metadata row %q", line)
		}
		epoch, err := normalizeEpoch(fields[1])
		if err != nil {
			return nil, err
		}
		result = append(result, identity{fields[0], epoch, fields[2], fields[3], fields[4]})
	}
	return result, nil
}

func (m *PackageManager) inventory(ctx context.Context) ([]identity, error) {
	output, err := m.run(ctx, "rpm", "-qa", "--queryformat", queryFormat)
	if err != nil {
		return nil, err
	}
	items, err := parseMetadata(output, true)
	slices.SortFunc(items, func(a, b identity) int { return strings.Compare(a.nevra(), b.nevra()) })
	return items, err
}

type request struct {
	input, path string
	identity
	anyArch bool
}

func (r request) matches(p identity) bool {
	return p.name == r.name && (p.arch == r.arch || (r.anyArch && p.arch == "noarch"))
}

func (r request) argument() string {
	if r.path != "" {
		return r.path
	}
	name := r.name
	if r.version != "" {
		name += "-" + r.evr()
	}
	if !r.anyArch {
		name += "." + r.arch
	}
	return name
}

func (m *PackageManager) local(ctx context.Context, path string) (identity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return identity{}, fmt.Errorf("local RPM %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return identity{}, fmt.Errorf("local RPM %q is not a regular file", path)
	}
	output, err := m.run(ctx, "rpm", "-qp", "--queryformat", queryFormat, "--", path)
	if err != nil {
		return identity{}, err
	}
	items, err := parseMetadata(output, false)
	if err != nil {
		return identity{}, fmt.Errorf("local RPM %q: %w", path, err)
	}
	if len(items) != 1 {
		return identity{}, fmt.Errorf("local RPM %q must describe exactly one package", path)
	}
	return items[0], nil
}

func (m *PackageManager) resolve(ctx context.Context, desired packages.Packages) (packages.Packages, []request, error) {
	desired.Name.Values = slices.Clone(desired.Name.Values)
	slices.Sort(desired.Name.Values)
	desired.State, desired.Packages = "", nil
	if desired.Ensure == "Absent" {
		return desired, nil, errors.New("DNF ensure Absent is not supported: safe DNF removal is separate future work; no removal was attempted")
	}
	if desired.Ensure != "" && desired.Ensure != "Present" {
		return desired, nil, errors.New("ensure must be Present or Absent")
	}
	desired.Ensure = "Present"
	if len(desired.Name.Values) == 0 || (desired.Name.Scalar && len(desired.Name.Values) != 1) {
		return desired, nil, errors.New("name must contain at least one package")
	}
	var version identity
	if desired.Version != "" {
		if !desired.Name.Scalar || len(desired.Name.Values) != 1 {
			return desired, nil, errors.New("version requires one scalar repository package with ensure Present")
		}
		var err error
		version, err = parseEVR(desired.Version)
		if err != nil {
			return desired, nil, err
		}
	}
	entries := make([]request, 0, len(desired.Name.Values))
	for index, input := range desired.Name.Values {
		if index > 0 && input == desired.Name.Values[index-1] {
			return desired, nil, fmt.Errorf("duplicate package input %q", input)
		}
		entry := request{input: input, identity: version}
		if strings.HasSuffix(input, ".rpm") || strings.ContainsAny(input, `/\`) {
			if desired.Version != "" || !strings.HasSuffix(input, ".rpm") || strings.ContainsAny(input, "\x00\r\n") {
				return desired, nil, errors.New("local RPM inputs require an accessible .rpm file and no version property")
			}
			path, err := filepath.Abs(input)
			if err != nil {
				return desired, nil, err
			}
			entry.path = path
		} else {
			name, arch, qualified := strings.Cut(input, ":")
			if !nameToken.MatchString(name) || (qualified && !archToken.MatchString(arch)) {
				return desired, nil, fmt.Errorf("invalid RPM package name %q: use a literal name or name:architecture", input)
			}
			entry.name, entry.arch, entry.anyArch = name, arch, !qualified
		}
		entries = append(entries, entry)
	}
	output, err := m.run(ctx, "rpm", "--eval", "%{_arch}")
	if err != nil {
		return desired, nil, err
	}
	native := strings.TrimSpace(string(output))
	if !archToken.MatchString(native) || native == "noarch" {
		return desired, nil, errors.New("RPM returned an invalid native architecture")
	}
	output, err = m.run(ctx, "rpm", "--showrc")
	if err != nil {
		return desired, nil, err
	}
	var compatible []string
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(key) == "compatible archs" {
			if compatible != nil {
				return desired, nil, errors.New("RPM returned ambiguous compatible architectures")
			}
			compatible = strings.Fields(value)
		}
	}
	if !slices.Contains(compatible, native) || !slices.Contains(compatible, "noarch") {
		return desired, nil, errors.New("RPM compatible architectures unavailable")
	}
	for index := range entries {
		entry := &entries[index]
		if entry.path != "" {
			entry.identity, err = m.local(ctx, entry.path)
			if err != nil {
				return desired, nil, err
			}
		} else if entry.anyArch {
			entry.arch = native
		}
		if !slices.Contains(compatible, entry.arch) {
			return desired, nil, fmt.Errorf("RPM architecture %q is not compatible with this host", entry.arch)
		}
		for _, previous := range entries[:index] {
			if previous.name == entry.name && (previous.arch == entry.arch ||
				previous.arch == "noarch" || entry.arch == "noarch") {
				return desired, nil, fmt.Errorf("duplicate or conflicting RPM package identity %q", entry.name)
			}
		}
	}
	return desired, entries, nil
}

func (m *PackageManager) inspect(ctx context.Context, desired packages.Packages, entries []request, inventory []identity) (packages.Packages, []bool, []string, error) {
	result := desired
	result.Packages = make([]packages.PackageState, 0, len(entries))
	compliant := make([]bool, len(entries))
	differences := []string{}
	installed := 0
	for i, entry := range entries {
		state := packages.PackageState{RequestedName: entry.input, Name: entry.name, Status: "not-installed"}
		var selected *identity
		exact := false
		for _, p := range inventory {
			if !entry.matches(p) {
				continue
			}
			match := entry.version == ""
			if !match {
				cmp, err := m.compare(ctx, p, entry.identity)
				if err != nil {
					return result, nil, nil, err
				}
				match = cmp == 0
			}
			if selected == nil || (match && !exact) {
				copy := p
				selected, exact = &copy, match
			} else if match == exact {
				cmp, err := m.compare(ctx, p, *selected)
				if err != nil {
					return result, nil, nil, err
				}
				if cmp > 0 {
					copy := p
					selected = &copy
				}
			}
		}
		if selected != nil {
			state = selected.observed(entry.input)
			installed++
			compliant[i] = exact
			if !exact && !slices.Contains(differences, "version") {
				differences = append(differences, "version")
			}
		} else if !slices.Contains(differences, "ensure") {
			differences = append(differences, "ensure")
		}
		result.Packages = append(result.Packages, state)
	}
	result.State = "Mixed"
	if installed == len(entries) {
		result.State = "Present"
	} else if installed == 0 {
		result.State = "Absent"
	}
	slices.Sort(differences)
	return result, compliant, differences, nil
}
