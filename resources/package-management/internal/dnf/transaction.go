package dnf

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Bpoe/dsc-extras/resources/package-management/internal/packages"
)

type action struct {
	identity
	kind string
}

func (m *PackageManager) preflight(ctx context.Context, options, targets []string) ([]action, error) {
	args := append(slices.Clone(options), "--assumeno", "--setopt=assumeyes=False", "install", "--")
	args = append(args, targets...)
	output, err := m.run(ctx, "dnf", args...)
	var commandErr *packages.CommandError
	if !errors.As(err, &commandErr) || commandErr.ExitCode() != 1 ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("DNF preflight did not abort at its confirmation prompt")
	}
	if strings.TrimSpace(commandErr.Stderr) != "Operation aborted." &&
		!(strings.TrimSpace(commandErr.Stderr) == "" && strings.HasSuffix(strings.TrimSpace(string(output)), "Operation aborted.")) {
		return nil, commandErr
	}
	plan, parseErr := parsePlan(string(output))
	if parseErr != nil {
		return nil, fmt.Errorf("cannot verify DNF transaction; refusing installation: %w (stdout: %s; stderr: %s)", parseErr, output, commandErr.Stderr)
	}
	return plan, nil
}

var sizeToken = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// Only the DNF 4 C-locale table is accepted. Counts must agree with parsed
// actions, including wrapped rows; new output/actions fail closed.
func parsePlan(output string) ([]action, error) {
	var plan []action
	header, summary, section := false, false, ""
	counts, reported := map[string]int{}, map[string]int{}
	var pending []string
	identities := map[string]bool{}
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if strings.Join(strings.Fields(line), " ") == "Package Architecture Version Repository Size" {
			if header {
				return nil, errors.New("duplicate transaction header")
			}
			header = true
			continue
		}
		if !header {
			continue
		}
		if line == "Transaction Summary" {
			if summary || len(pending) != 0 {
				return nil, errors.New("invalid transaction summary")
			}
			summary = true
			continue
		}
		if line == "" || strings.Trim(line, "=") == "" {
			continue
		}
		if summary {
			fields := strings.Fields(line)
			if len(fields) == 3 && (fields[2] == "Package" || fields[2] == "Packages") {
				if fields[0] != "Install" && fields[0] != "Upgrade" && fields[0] != "Downgrade" {
					return nil, fmt.Errorf("unsafe transaction summary %q", line)
				}
				count, err := strconv.Atoi(fields[1])
				if err != nil || count <= 0 || reported[fields[0]] != 0 {
					return nil, fmt.Errorf("invalid transaction count %q", line)
				}
				reported[fields[0]] = count
			} else if !strings.HasPrefix(line, "Total download size: ") &&
				!strings.HasPrefix(line, "Total size: ") &&
				!strings.HasPrefix(line, "Installed size: ") &&
				line != "Operation aborted." {
				return nil, fmt.Errorf("unknown transaction trailer %q", line)
			}
			continue
		}
		if strings.HasSuffix(line, ":") {
			if len(pending) != 0 {
				return nil, errors.New("incomplete transaction row")
			}
			switch line {
			case "Installing:", "Installing dependencies:", "Installing weak dependencies:":
				section = "Install"
			case "Upgrading:":
				section = "Upgrade"
			case "Downgrading:":
				section = "Downgrade"
			default:
				return nil, fmt.Errorf("unsafe or unknown DNF action %q", line)
			}
			continue
		}
		if section == "" || strings.HasPrefix(line, "replacing ") {
			return nil, fmt.Errorf("unsafe or unknown transaction row %q", line)
		}
		pending = append(pending, strings.Fields(line)...)
		if len(pending) < 5 {
			continue
		}
		if len(pending) == 5 && pending[4] != "0" {
			continue
		}
		if (len(pending) != 5 && len(pending) != 6) || !sizeToken.MatchString(pending[4]) ||
			(len(pending) == 6 && !slices.Contains([]string{"B", "k", "M", "G", "T"}, pending[5])) ||
			!nameToken.MatchString(pending[0]) || !archToken.MatchString(pending[1]) {
			return nil, fmt.Errorf("invalid DNF transaction row %q", pending)
		}
		p, err := parseEVR(pending[2])
		if err != nil {
			return nil, err
		}
		p.name, p.arch = pending[0], pending[1]
		if identities[p.key()] {
			return nil, fmt.Errorf("ambiguous DNF transaction identity %q", p.key())
		}
		identities[p.key()] = true
		plan = append(plan, action{p, section})
		counts[section]++
		pending = nil
	}
	if !summary || len(plan) == 0 || len(pending) != 0 {
		return nil, errors.New("missing or incomplete DNF transaction")
	}
	for _, kind := range []string{"Install", "Upgrade", "Downgrade"} {
		if reported[kind] != counts[kind] {
			return nil, fmt.Errorf("DNF %s action count does not match its summary", kind)
		}
	}
	slices.SortFunc(plan, func(a, b action) int { return strings.Compare(a.nevra(), b.nevra()) })
	return plan, nil
}

func (m *PackageManager) validatePlan(ctx context.Context, plan []action, entries []request, compliant []bool, allowDowngrade bool) error {
	for _, change := range plan {
		requested := false
		for i, entry := range entries {
			if !entry.matches(change.identity) {
				continue
			}
			requested = true
			if compliant[i] {
				return fmt.Errorf("DNF would change already-compliant requested package %q; reconcile its dependent packages separately", entry.input)
			}
			if entry.version != "" {
				cmp, err := m.compare(ctx, change.identity, entry.identity)
				if err != nil {
					return err
				}
				if cmp != 0 {
					return fmt.Errorf("DNF did not resolve exact RPM EVR for %q", entry.input)
				}
			}
		}
		if change.kind == "Downgrade" && (!requested || !allowDowngrade) {
			return fmt.Errorf("DNF proposed an unrequested or unauthorized downgrade of %q", change.key())
		}
	}
	for i, entry := range entries {
		if compliant[i] {
			continue
		}
		matches := 0
		for _, change := range plan {
			if entry.matches(change.identity) {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("DNF did not resolve exactly one requested RPM identity for %q (capability, wrong architecture, or unavailable package)", entry.input)
		}
	}
	return nil
}
