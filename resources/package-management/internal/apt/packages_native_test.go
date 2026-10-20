package apt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only archive building and Get/Test are used here; no package transaction runs.
func TestPackagesNativeReadOnly(t *testing.T) {
	for _, tool := range []string{"dpkg", "dpkg-query", "dpkg-deb"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("optional native integration tool %s unavailable: %v", tool, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manager := NewPackageManager(nil)
	bash, err := manager.Get(ctx, packageDesired("bash"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bash.Packages) != 1 || !bash.Packages[0].Installed {
		t.Skip("optional native integration requires installed bash")
	}
	installed := bash.Packages[0]
	artifact := packageDeb(t, "artifact-name-does-not-match-package.deb")
	directory := filepath.Dir(artifact)
	buildArchive := func(name, version, architecture string) {
		t.Helper()
		root := filepath.Join(directory, "archive")
		controlDirectory := filepath.Join(root, "DEBIAN")
		if err := os.MkdirAll(controlDirectory, 0755); err != nil {
			t.Fatal(err)
		}
		control := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: Test <test@example.invalid>\nDescription: Native read-only package resource fixture\n",
			name, version, architecture)
		if err := os.WriteFile(filepath.Join(controlDirectory, "control"), []byte(control), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.run(ctx, "dpkg-deb", "--build", "--root-owner-group", root, artifact); err != nil {
			t.Fatal(err)
		}
	}
	buildArchive("bash", installed.Version, installed.Architecture)
	actual, differences, err := manager.Test(ctx, packageDesired(artifact))
	if err != nil {
		t.Fatal(err)
	}
	if actual.State != "Present" || len(differences) != 0 || differences == nil ||
		actual.Packages[0].Name != "bash" || actual.Packages[0].RequestedName != artifact ||
		actual.Packages[0].Version != installed.Version || actual.Packages[0].Architecture != installed.Architecture {
		t.Fatalf("archive metadata did not resolve to installed bash: %+v, differences %v", actual, differences)
	}

	// The repository-local random fixture directory makes an installed-name
	// collision extremely unlikely while retaining Debian's package grammar.
	missingName := "dsc-extras-uninstalled-test-" + strings.TrimPrefix(filepath.Base(directory), ".packages-test-")
	buildArchive(missingName, "1", installed.Architecture)
	actual, differences, err = manager.Test(ctx, packageDesired(artifact))
	if err != nil {
		t.Fatal(err)
	}
	if actual.State != "Absent" || len(differences) != 1 || differences[0] != "ensure" ||
		actual.Packages[0].Name != missingName || actual.Packages[0].Installed ||
		actual.Packages[0].Status != "" || actual.Packages[0].Version != "" {
		t.Fatalf("native missing-package diagnostic was not recognized: %+v, differences %v", actual, differences)
	}
}
