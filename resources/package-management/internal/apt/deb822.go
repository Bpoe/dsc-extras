package apt

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var deb822FieldName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

type sourceField struct {
	name       string
	value      string
	start, end int
}

type sourceBlock struct {
	start, end      int
	first, endEntry int
	fields          []sourceField
}

func (d *AptDocument) parseDeb822() error {
	lines := documentLines(d.OriginalContents)
	for i := 0; i < len(lines); {
		if strings.TrimSpace(lines[i].text) == "" {
			i++
			continue
		}
		block := sourceBlock{start: lines[i].start, first: len(d.Entries)}
		values := map[string]string{}
		for ; i < len(lines) && strings.TrimSpace(lines[i].text) != ""; i++ {
			line := lines[i]
			block.end = line.end
			if strings.HasPrefix(strings.TrimSpace(line.text), "#") {
				continue
			}
			if strings.HasPrefix(line.text, " ") || strings.HasPrefix(line.text, "\t") {
				if len(block.fields) == 0 {
					return fmt.Errorf("orphan continuation at line %d", i+1)
				}
				field := &block.fields[len(block.fields)-1]
				field.value += "\n" + strings.TrimSpace(line.text)
				field.end = line.end
				values[field.name] = field.value
				continue
			}
			name, value, ok := strings.Cut(line.text, ":")
			if !ok || !deb822FieldName.MatchString(name) {
				return fmt.Errorf("invalid Deb822 field at line %d", i+1)
			}
			name = strings.ToLower(name)
			if _, exists := values[name]; exists {
				return fmt.Errorf("duplicate Deb822 field %q at line %d", name, i+1)
			}
			value = strings.TrimSpace(value)
			values[name] = value
			block.fields = append(block.fields, sourceField{name, value, line.start, line.end})
		}
		if len(block.fields) == 0 {
			continue
		}
		types, uris, suites := strings.Fields(values["types"]), strings.Fields(values["uris"]), strings.Fields(values["suites"])
		if len(types) == 0 || len(uris) == 0 || len(suites) == 0 {
			return fmt.Errorf("Deb822 stanza at byte %d requires Types, URIs and Suites", block.start)
		}
		if len(types)*len(uris)*len(suites) > 4096 {
			return fmt.Errorf("Deb822 stanza at byte %d expands to too many repositories", block.start)
		}
		for _, typ := range types {
			for _, uri := range uris {
				for _, suite := range suites {
					repo, err := repositoryFromFields(typ, uri, suite, values)
					if err != nil {
						return err
					}
					d.Entries = append(d.Entries, repo)
				}
			}
		}
		block.endEntry = len(d.Entries)
		d.blocks = append(d.blocks, block)
	}
	return nil
}

func repositoryFromFields(typ, uri, suite string, fields map[string]string) (AptRepository, error) {
	repo := AptRepository{
		Type: typ, URI: uri, Suite: suite, Enabled: true,
		Components: strings.Fields(fields["components"]), Architectures: strings.Fields(fields["architectures"]),
		Options: map[string]string{},
	}
	if typ != "deb" && typ != "deb-src" {
		return repo, fmt.Errorf("unsupported APT source type %q", typ)
	}
	if value, exists := fields["enabled"]; exists {
		switch strings.ToLower(value) {
		case "yes":
		case "no":
			repo.Enabled = false
		default:
			return repo, fmt.Errorf("invalid Enabled value %q", value)
		}
	}
	signedBy := strings.TrimSpace(fields["signed-by"])
	if strings.HasPrefix(signedBy, "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		repo.Signing.InlineKey = strings.ReplaceAll(signedBy, "\n.\n", "\n\n") + "\n"
	} else {
		for _, reference := range strings.Fields(strings.ReplaceAll(signedBy, ",", " ")) {
			if strings.HasPrefix(reference, "/") {
				repo.Signing.Keyrings = append(repo.Signing.Keyrings, reference)
			} else if fingerprintPattern.MatchString(reference) {
				repo.Signing.Fingerprints = append(repo.Signing.Fingerprints, strings.ToUpper(reference))
			} else {
				return repo, fmt.Errorf("invalid Signed-By reference %q", reference)
			}
		}
	}
	for name, value := range fields {
		switch name {
		case "types", "uris", "suites", "components", "architectures", "enabled", "signed-by":
		default:
			repo.Options[name] = value
		}
	}
	if strings.HasSuffix(suite, "/") != (len(repo.Components) == 0) {
		return repo, fmt.Errorf("invalid components for suite %q", suite)
	}
	return repo, nil
}

var fingerprintPattern = regexp.MustCompile(`(?i)^[0-9a-f]{40}!?$`)

func signingValue(signing SigningConfiguration) string {
	if signing.InlineKey != "" {
		return "\n" + strings.ReplaceAll(strings.TrimSpace(signing.InlineKey), "\n\n", "\n.\n")
	}
	return strings.Join(append(slices.Clone(signing.Keyrings), signing.Fingerprints...), " ")
}

func repositoryFields(r AptRepository) map[string]string {
	fields := map[string]string{
		"types": r.Type, "uris": r.URI, "suites": r.Suite,
		"components": strings.Join(r.Components, " "), "architectures": strings.Join(r.Architectures, " "),
		"signed-by": signingValue(r.Signing),
	}
	if !r.Enabled {
		fields["enabled"] = "no"
	} else {
		fields["enabled"] = "yes"
	}
	for name, value := range r.Options {
		fields[name] = value
	}
	return fields
}

func deb822Field(name, value, newline string) string {
	var lines strings.Builder
	fmt.Fprintf(&lines, "%s: %s%s", name, strings.Split(value, "\n")[0], newline)
	for _, continuation := range strings.Split(value, "\n")[1:] {
		fmt.Fprintf(&lines, " %s%s", continuation, newline)
	}
	return lines.String()
}

func serializeDeb822(repository Repository, keyPath string) []byte {
	repo := AptRepository{Type: "deb", URI: repository.URI, Suite: repository.Suite,
		Components: repository.Components, Architectures: repository.Architectures,
		Enabled: repository.enabled(), Signing: SigningConfiguration{Keyrings: []string{keyPath}}}
	fields := repositoryFields(repo)
	var out strings.Builder
	for _, name := range []string{"types", "uris", "suites", "components", "architectures", "enabled", "signed-by"} {
		if fields[name] == "" || name == "enabled" && repo.Enabled {
			continue
		}
		out.WriteString(deb822Field(fieldTitle(name), fields[name], "\n"))
	}
	return []byte(out.String())
}

func fieldTitle(name string) string {
	switch name {
	case "uris":
		return "URIs"
	case "signed-by":
		return "Signed-By"
	default:
		return strings.ToUpper(name[:1]) + name[1:]
	}
}
