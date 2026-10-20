package platform

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

func Detect() (string, error) {
	file, err := os.Open("/etc/os-release")
	if err != nil {
		return "", fmt.Errorf("detect package backend: read /etc/os-release: %w", err)
	}
	defer file.Close()
	return Parse(file)
}

// Parse reads os-release as data, never as a shell script. Unknown derivatives
// are deliberately not enabled merely because ID_LIKE mentions a supported OS.
func Parse(input io.Reader) (string, error) {
	values := map[string]string{}
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "ID" && key != "VERSION_ID" && key != "ID_LIKE") {
			continue
		}
		if _, exists := values[key]; exists {
			return "", fmt.Errorf("duplicate %s in /etc/os-release", key)
		}
		if strings.HasPrefix(value, `"`) || strings.HasPrefix(value, "'") {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				return "", fmt.Errorf("invalid %s in /etc/os-release", key)
			}
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read /etc/os-release: %w", err)
	}
	switch values["ID"] {
	case "debian", "ubuntu":
		return "apt", nil
	case "rhel", "rocky", "almalinux":
		major, _, _ := strings.Cut(values["VERSION_ID"], ".")
		if major == "8" || major == "9" || major == "10" {
			return "dnf", nil
		}
	}
	return "", fmt.Errorf("unsupported package platform ID=%q VERSION_ID=%q: supported platforms are Debian/Ubuntu and RHEL/Rocky/AlmaLinux 8, 9, 10",
		values["ID"], values["VERSION_ID"])
}
