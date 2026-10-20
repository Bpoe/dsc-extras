package apt

import (
	"fmt"
	"slices"
	"strings"
)

func (d *AptDocument) parseList() error {
	for number, line := range documentLines(d.OriginalContents) {
		text := strings.TrimSpace(line.text)
		enabled := true
		if strings.HasPrefix(text, "#") {
			text = strings.TrimSpace(strings.TrimPrefix(text, "#"))
			enabled = false
			if !strings.HasPrefix(text, "deb ") && !strings.HasPrefix(text, "deb-src ") &&
				!strings.HasPrefix(text, "deb\t") && !strings.HasPrefix(text, "deb-src\t") {
				continue
			}
		}
		text, _, _ = strings.Cut(text, "#")
		tokens := strings.Fields(text)
		if len(tokens) == 0 {
			continue
		}
		typ := tokens[0]
		text = strings.TrimSpace(strings.TrimPrefix(text, typ))
		fields := map[string]string{}
		if strings.HasPrefix(text, "[") {
			options, rest, ok := strings.Cut(text[1:], "]")
			if !ok {
				return fmt.Errorf("unclosed options at line %d", number+1)
			}
			for _, option := range strings.Fields(options) {
				name, value, ok := strings.Cut(option, "=")
				if !ok || name == "" || value == "" {
					return fmt.Errorf("invalid option %q at line %d", option, number+1)
				}
				name = optionField(strings.ToLower(name))
				if _, exists := fields[name]; exists {
					return fmt.Errorf("duplicate option %q at line %d", name, number+1)
				}
				fields[name] = strings.ReplaceAll(value, ",", " ")
			}
			text = rest
		}
		tokens = strings.Fields(text)
		if len(tokens) < 2 {
			return fmt.Errorf("source at line %d requires URI and suite", number+1)
		}
		fields["components"] = strings.Join(tokens[2:], " ")
		repo, err := repositoryFromFields(typ, tokens[0], tokens[1], fields)
		if err != nil {
			return fmt.Errorf("line %d: %w", number+1, err)
		}
		repo.Enabled = enabled
		first := len(d.Entries)
		d.Entries = append(d.Entries, repo)
		d.blocks = append(d.blocks, sourceBlock{start: line.start, end: line.end, first: first, endEntry: first + 1})
	}
	return nil
}

func optionField(name string) string {
	switch name {
	case "arch":
		return "architectures"
	case "arch+":
		return "architectures-add"
	case "arch-":
		return "architectures-remove"
	default:
		return name
	}
}

func fieldOption(name string) string {
	switch name {
	case "architectures":
		return "arch"
	case "architectures-add":
		return "arch+"
	case "architectures-remove":
		return "arch-"
	default:
		return name
	}
}

func serializeList(repo AptRepository, original, newline string) (string, error) {
	if repo.Signing.InlineKey != "" {
		return "", fmt.Errorf("inline signing keys cannot be stored in a .list file")
	}
	fields := repositoryFields(repo)
	names := []string{}
	for name, value := range fields {
		switch name {
		case "types", "uris", "suites", "components", "enabled":
			continue
		}
		if value != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	options := []string{}
	for _, name := range names {
		options = append(options, fieldOption(name)+"="+strings.Join(strings.Fields(fields[name]), ","))
	}
	var out strings.Builder
	out.WriteString(original[:len(original)-len(strings.TrimLeft(original, " \t"))])
	if !repo.Enabled {
		out.WriteString("# ")
	}
	out.WriteString(repo.Type + " ")
	if len(options) > 0 {
		out.WriteString("[" + strings.Join(options, " ") + "] ")
	}
	out.WriteString(repo.URI + " " + repo.Suite)
	if len(repo.Components) > 0 {
		out.WriteString(" " + strings.Join(repo.Components, " "))
	}
	commentSource := strings.TrimSpace(original)
	if strings.HasPrefix(commentSource, "#") {
		commentSource = strings.TrimPrefix(commentSource, "#")
	}
	if _, comment, found := strings.Cut(commentSource, "#"); found {
		out.WriteString(" #" + comment)
	}
	out.WriteString(newline)
	return out.String(), nil
}
