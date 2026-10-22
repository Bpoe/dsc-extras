package groups

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

const maxGID = 1<<32 - 2

type Group struct {
	Name            string    `json:"name"`
	Ensure          string    `json:"ensure,omitempty"`
	GID             *int64    `json:"gid,omitempty"`
	Members         *[]string `json:"members,omitempty"`
	IncludedMembers *[]string `json:"includedMembers,omitempty"`
	ExcludedMembers *[]string `json:"excludedMembers,omitempty"`
}

func (g *Group) UnmarshalJSON(data []byte) error {
	type properties Group
	var value properties
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("properties must be a JSON object")
	}
	for _, name := range []string{"name", "ensure", "gid", "members", "includedMembers", "excludedMembers"} {
		if raw, ok := fields[name]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("%s must not be null; omit unmanaged properties", name)
		}
		if _, supplied := fields["ensure"]; supplied && value.Ensure == "" {
			return errors.New("ensure must be Present or Absent")
		}
	}
	*g = Group(value)
	return nil
}

type Executor interface {
	Execute(context.Context, string, []string) ([]byte, []byte, error)
}

type nativeExecutor struct{}

func (nativeExecutor) Execute(ctx context.Context, program string, args []string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, program, args...)
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

// root redirects local group inspection to an isolated fixture in tests.
func NewManager(executor Executor, root string) *Manager {
	if executor == nil {
		executor = nativeExecutor{}
	}
	if root == "" {
		root = string(filepath.Separator)
	}
	return &Manager{executor: executor, root: root}
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.HasPrefix(name, "-") &&
		!strings.ContainsAny(name, ":,/\x00") && !strings.ContainsFunc(name, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

func normalize(desired Group) (Group, error) {
	if !validName(desired.Name) || strings.HasPrefix(desired.Name, "+") || strings.HasPrefix(desired.Name, "#") {
		return Group{}, errors.New("name must be a literal local group name, not an option, path, or account-database directive")
	}
	if desired.Ensure == "" {
		desired.Ensure = "Present"
	}
	if desired.Ensure != "Present" && desired.Ensure != "Absent" {
		return Group{}, errors.New("ensure must be Present or Absent")
	}
	if desired.Ensure == "Absent" && (desired.GID != nil || desired.Members != nil ||
		desired.IncludedMembers != nil || desired.ExcludedMembers != nil) {
		return Group{}, errors.New("ensure Absent cannot specify gid or membership constraints")
	}
	if desired.GID != nil && (*desired.GID < 0 || *desired.GID > maxGID) {
		return Group{}, fmt.Errorf("gid must be an integer between 0 and %d", maxGID)
	}
	if desired.Members != nil && (desired.IncludedMembers != nil || desired.ExcludedMembers != nil) {
		return Group{}, errors.New("members cannot be combined with includedMembers or excludedMembers")
	}
	for _, property := range []struct {
		name  string
		value *[]string
	}{
		{"members", desired.Members}, {"includedMembers", desired.IncludedMembers}, {"excludedMembers", desired.ExcludedMembers},
	} {
		seen := make(map[string]bool)
		if property.value == nil {
			continue
		}
		for _, member := range *property.value {
			if !validName(member) {
				return Group{}, fmt.Errorf("%s contains invalid user name %q", property.name, member)
			}
			if seen[member] {
				return Group{}, fmt.Errorf("%s contains duplicate user %q", property.name, member)
			}
			seen[member] = true
		}
	}
	if desired.IncludedMembers != nil && desired.ExcludedMembers != nil {
		for _, member := range *desired.IncludedMembers {
			if slices.Contains(*desired.ExcludedMembers, member) {
				return Group{}, fmt.Errorf("user %q appears in both includedMembers and excludedMembers", member)
			}
		}
	}
	return desired, nil
}

func parseGID(text string) (int64, error) {
	value, err := strconv.ParseUint(text, 10, 32)
	if err != nil || value > maxGID {
		return 0, fmt.Errorf("invalid numeric GID %q", text)
	}
	return int64(value), nil
}

func parseGroup(data []byte, name string) (Group, error) {
	actual := Group{Name: name, Ensure: "Absent"}
	members := []string{}
	for index, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 4 {
			return Group{}, fmt.Errorf("invalid group database entry at line %d", index+1)
		}
		gid, err := parseGID(fields[2])
		if err != nil {
			return Group{}, fmt.Errorf("group database line %d: %w", index+1, err)
		}
		if fields[0] != name {
			continue
		}
		if actual.GID != nil && *actual.GID != gid {
			return Group{}, fmt.Errorf("local group %q has conflicting GIDs %d and %d", name, *actual.GID, gid)
		}
		actual.Ensure, actual.GID = "Present", &gid
		if fields[3] != "" {
			for _, member := range strings.Split(fields[3], ",") {
				if !validName(member) {
					return Group{}, fmt.Errorf("invalid supplementary member %q at group database line %d", member, index+1)
				}
				members = append(members, member)
			}
		}
	}
	if actual.Ensure == "Present" {
		slices.Sort(members)
		members = slices.Compact(members)
		actual.Members = &members
	}
	return actual, nil
}

func (m *Manager) observe(name string) (Group, error) {
	data, err := os.ReadFile(filepath.Join(m.root, "etc", "group"))
	if err != nil {
		return Group{}, fmt.Errorf("read local group database: %w", err)
	}
	actual, err := parseGroup(data, name)
	if err != nil {
		return Group{}, fmt.Errorf("read local group database: %w", err)
	}
	return actual, nil
}

func (m *Manager) lookup(ctx context.Context, database, name string) ([]string, bool, error) {
	stdout, stderr, err := m.executor.Execute(ctx, "getent", []string{database, name})
	if err != nil {
		var status interface{ ExitCode() int }
		if errors.As(err, &status) && status.ExitCode() == 2 {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("getent %s %s: %w: %s", database, name, err, strings.TrimSpace(string(stderr)))
	}
	lines := strings.Split(strings.TrimRight(string(stdout), "\r\n"), "\n")
	fields := strings.Split(lines[0], ":")
	wantFields, gidIndex := 7, 3
	if database == "group" {
		wantFields, gidIndex = 4, 2
	}
	if len(fields) != wantFields || fields[0] != name {
		return nil, false, fmt.Errorf("getent %s %s returned a malformed or mismatched identity", database, name)
	}
	if _, err := parseGID(fields[gidIndex]); err != nil {
		return nil, false, fmt.Errorf("getent %s %s: %w", database, name, err)
	}
	if database == "passwd" {
		if _, err := strconv.ParseUint(fields[2], 10, 32); err != nil || len(lines) != 1 {
			return nil, false, fmt.Errorf("getent passwd %s returned a malformed identity", name)
		}
	} else {
		// getent may also return split entries for a single logical group.
		if _, err := parseGroup(stdout, name); err != nil {
			return nil, false, fmt.Errorf("getent group %s: %w", name, err)
		}
		for _, line := range lines {
			if strings.Split(line, ":")[0] != name {
				return nil, false, fmt.Errorf("getent group %s returned a mismatched identity", name)
			}
		}
	}
	return fields, true, nil
}

func (m *Manager) primaryGIDs(ctx context.Context, desired Group) (map[string]int64, error) {
	result := make(map[string]int64)
	for _, list := range []*[]string{desired.Members, desired.IncludedMembers, desired.ExcludedMembers} {
		if list == nil {
			continue
		}
		for _, name := range *list {
			fields, found, err := m.lookup(ctx, "passwd", name)
			if err != nil {
				return nil, err
			}
			if !found {
				if list == desired.ExcludedMembers {
					continue
				}
				return nil, fmt.Errorf("user %q does not exist; create the user before requesting supplementary membership", name)
			}
			gid, err := parseGID(fields[3])
			if err != nil {
				return nil, err
			}
			result[name] = gid
		}
	}
	return result, nil
}

func memberList(group Group) []string {
	if group.Members == nil {
		return nil
	}
	return *group.Members
}

func differences(desired, actual Group, primary map[string]int64) []string {
	diff := []string{}
	if desired.Ensure != actual.Ensure {
		diff = append(diff, "ensure")
	}
	if desired.GID != nil && (actual.GID == nil || *desired.GID != *actual.GID) {
		diff = append(diff, "gid")
	}
	if desired.Members != nil {
		want := slices.Clone(*desired.Members)
		slices.Sort(want)
		if !slices.Equal(want, memberList(actual)) {
			diff = append(diff, "members")
		}
	}
	if desired.IncludedMembers != nil {
		for _, name := range *desired.IncludedMembers {
			if !slices.Contains(memberList(actual), name) {
				diff = append(diff, "includedMembers")
				break
			}
		}
	}
	if desired.ExcludedMembers != nil {
		for _, name := range *desired.ExcludedMembers {
			gid, found := primary[name]
			if slices.Contains(memberList(actual), name) || (found && actual.GID != nil && gid == *actual.GID) {
				diff = append(diff, "excludedMembers")
				break
			}
		}
	}
	return diff
}

func (m *Manager) Get(_ context.Context, desired Group) (Group, error) {
	desired, err := normalize(desired)
	if err != nil {
		return Group{}, err
	}
	return m.observe(desired.Name)
}

func (m *Manager) Test(ctx context.Context, desired Group) (Group, []string, error) {
	desired, err := normalize(desired)
	if err != nil {
		return Group{}, nil, err
	}
	actual, err := m.observe(desired.Name)
	if err != nil {
		return Group{}, nil, err
	}
	primary, err := m.primaryGIDs(ctx, desired)
	if err != nil {
		return Group{}, nil, err
	}
	return actual, differences(desired, actual, primary), nil
}

func (m *Manager) mutate(ctx context.Context, program string, args ...string) error {
	_, stderr, err := m.executor.Execute(ctx, program, args)
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", program, strings.Join(args, " "), err, strings.TrimSpace(string(stderr)))
	}
	return nil
}

func exclusionError(desired, actual Group, primary map[string]int64) error {
	if desired.ExcludedMembers != nil && actual.GID != nil {
		for _, name := range *desired.ExcludedMembers {
			if gid, found := primary[name]; found && gid == *actual.GID {
				return fmt.Errorf("cannot exclude user %q: group %q is the user's primary group; explicitly reassign the primary group outside this resource", name, desired.Name)
			}
		}
	}
	return nil
}

func (m *Manager) Set(ctx context.Context, desired Group) (Group, error) {
	desired, err := normalize(desired)
	if err != nil {
		return Group{}, err
	}
	actual, err := m.observe(desired.Name)
	if err != nil {
		return Group{}, err
	}
	primary, err := m.primaryGIDs(ctx, desired)
	if err != nil {
		return Group{}, err
	}
	if len(differences(desired, actual, primary)) == 0 {
		return actual, nil
	}
	if err := exclusionError(desired, actual, primary); err != nil {
		return Group{}, err
	}
	if desired.GID != nil {
		proposed := actual
		proposed.GID = desired.GID
		if err := exclusionError(desired, proposed, primary); err != nil {
			return Group{}, err
		}
	}
	if desired.Ensure == "Absent" {
		if err := m.mutate(ctx, "groupdel", "--", desired.Name); err != nil {
			return Group{}, err
		}
	} else {
		if actual.Ensure == "Absent" {
			if _, found, err := m.lookup(ctx, "group", desired.Name); err != nil {
				return Group{}, err
			} else if found {
				return Group{}, fmt.Errorf("group %q is externally managed through NSS; only local groups can be managed", desired.Name)
			}
			args := []string{}
			if desired.GID != nil {
				args = append(args, "-g", strconv.FormatInt(*desired.GID, 10))
			}
			args = append(args, "--", desired.Name)
			if err := m.mutate(ctx, "groupadd", args...); err != nil {
				return Group{}, err
			}
		} else if desired.GID != nil && *desired.GID != *actual.GID {
			if err := m.mutate(ctx, "groupmod", "-g", strconv.FormatInt(*desired.GID, 10), "--", desired.Name); err != nil {
				return Group{}, err
			}
		}
		actual, err = m.observe(desired.Name)
		if err != nil {
			return Group{}, err
		}
		if actual.Ensure != "Present" {
			return Group{}, fmt.Errorf("group %q is still absent after creation", desired.Name)
		}
		primary, err = m.primaryGIDs(ctx, desired)
		if err != nil {
			return Group{}, err
		}
		if err := exclusionError(desired, actual, primary); err != nil {
			return Group{}, err
		}
		add, remove := []string{}, []string{}
		if desired.Members != nil {
			for _, name := range *desired.Members {
				if !slices.Contains(memberList(actual), name) {
					add = append(add, name)
				}
			}
			for _, name := range memberList(actual) {
				if !slices.Contains(*desired.Members, name) {
					remove = append(remove, name)
				}
			}
		} else {
			if desired.IncludedMembers != nil {
				for _, name := range *desired.IncludedMembers {
					if !slices.Contains(memberList(actual), name) {
						add = append(add, name)
					}
				}
			}
			if desired.ExcludedMembers != nil {
				for _, name := range *desired.ExcludedMembers {
					if slices.Contains(memberList(actual), name) {
						remove = append(remove, name)
					}
				}
			}
		}
		slices.Sort(add)
		slices.Sort(remove)
		for _, change := range []struct {
			option string
			names  []string
		}{{"-a", add}, {"-d", remove}} {
			for _, name := range change.names {
				if err := m.mutate(ctx, "gpasswd", change.option, name, desired.Name); err != nil {
					return Group{}, err
				}
			}
		}
	}
	after, diff, err := m.Test(ctx, desired)
	if err != nil {
		return Group{}, err
	}
	if len(diff) != 0 {
		return Group{}, fmt.Errorf("group %q did not converge; differing properties: %s", desired.Name, strings.Join(diff, ", "))
	}
	return after, nil
}
