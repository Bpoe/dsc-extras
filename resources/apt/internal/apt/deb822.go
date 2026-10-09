package apt

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

var deb822FieldName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

func parseDeb822(name, expectedKeyPath string, data []byte) (Repository, bool, bool) {
	repository := Repository{Name: name, Ensure: "Present"}
	stanzas, syntaxOK := parseDeb822Stanzas(data)
	if len(stanzas) != 1 {
		return repository, false, false
	}
	fields := stanzas[0]
	if !syntaxOK {
		populateRepository(&repository, fields)
		return repository, false, false
	}
	for field := range fields {
		switch field {
		case "types", "uris", "suites", "components", "architectures", "signed-by":
		default:
			syntaxOK = false
		}
	}
	values := func(key string) []string {
		return strings.Fields(fields[key])
	}
	types := values("types")
	uris := values("uris")
	suites := values("suites")
	components := values("components")
	architectures := values("architectures")
	keyringMatches := len(values("signed-by")) == 1 && fields["signed-by"] == expectedKeyPath
	if len(types) != 1 || types[0] != "deb" ||
		len(uris) != 1 || len(suites) != 1 || len(components) == 0 ||
		!keyringMatches {
		syntaxOK = false
	}
	populateRepository(&repository, fields)
	if len(uris) == 1 {
		repository.URI = uris[0]
	}
	if len(suites) == 1 {
		repository.Suite = suites[0]
	}
	repository.Components = components
	repository.Architectures = architectures
	if validateProperties(repository) != nil {
		syntaxOK = false
	}
	return repository, syntaxOK, keyringMatches
}

func populateRepository(repository *Repository, fields map[string]string) {
	if values := strings.Fields(fields["uris"]); len(values) == 1 {
		repository.URI = values[0]
	}
	if values := strings.Fields(fields["suites"]); len(values) == 1 {
		repository.Suite = values[0]
	}
	repository.Components = strings.Fields(fields["components"])
	repository.Architectures = strings.Fields(fields["architectures"])
}

func parseDeb822Stanzas(data []byte) ([]map[string]string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var stanzas []map[string]string
	fields := make(map[string]string)
	field := ""
	open := false
	syntaxOK := true
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if open {
				stanzas = append(stanzas, fields)
				fields = make(map[string]string)
				field = ""
				open = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if field == "" {
				syntaxOK = false
				continue
			}
			fields[field] += " " + trimmed
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !deb822FieldName.MatchString(name) {
			syntaxOK = false
			field = ""
			continue
		}
		field = strings.ToLower(name)
		if _, duplicate := fields[field]; duplicate {
			syntaxOK = false
			continue
		}
		fields[field] = strings.TrimSpace(value)
		open = true
	}
	if scanner.Err() != nil {
		syntaxOK = false
	}
	if open {
		stanzas = append(stanzas, fields)
	}
	if len(stanzas) == 0 {
		return []map[string]string{{}}, false
	}
	return stanzas, syntaxOK
}

func serializeDeb822(repository Repository, keyPath string) []byte {
	var source strings.Builder
	fmt.Fprintf(&source, "Types: deb\nURIs: %s\nSuites: %s\nComponents: %s\n",
		repository.URI, repository.Suite, strings.Join(repository.Components, " "))
	if len(repository.Architectures) != 0 {
		fmt.Fprintf(&source, "Architectures: %s\n", strings.Join(repository.Architectures, " "))
	}
	fmt.Fprintf(&source, "Signed-By: %s\n", keyPath)
	return []byte(source.String())
}
