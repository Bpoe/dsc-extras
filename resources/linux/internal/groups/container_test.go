package groups

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Account databases and every native mutation remain inside this container.
func TestGroupsContainer(t *testing.T) {
	if os.Getenv("DSCLINUX_GROUPS_CONTAINER") != "1" {
		t.Skip("set DSCLINUX_GROUPS_CONTAINER=1 to test native group tools in a disposable container")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal("DSCLINUX_GROUPS_CONTAINER=1 requires Docker")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	command := func(input string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, docker, args...)
		cmd.Stdin = strings.NewReader(input)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, output)
		}
		return string(output)
	}
	command("", "info")
	name := fmt.Sprintf("dsclinux-groups-%d-%d", os.Getpid(), time.Now().UnixNano())
	image := name + ":test"
	command(`FROM debian:bookworm-slim
RUN apt-get update -qq && apt-get install -y --no-install-recommends passwd libc-bin >/dev/null && apt-get clean
CMD ["sleep", "600"]
`, "build", "--quiet", "--tag", image, "-")
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if output, err := exec.CommandContext(cleanupCtx, docker, "image", "rm", image).CombinedOutput(); err != nil {
			t.Errorf("remove test image: %v\n%s", err, output)
		}
	})
	command("", "run", "--detach", "--name", name, "--network", "none", image)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if output, err := exec.CommandContext(cleanupCtx, docker, "rm", "--force", name).CombinedOutput(); err != nil {
			t.Errorf("remove test container: %v\n%s", err, output)
		}
	})
	command("", "exec", name, "getent", "group", "root")
	binary := filepath.Join(t.TempDir(), "dsclinux")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, filepath.Join("..", "..", "cmd", "dsclinux"))
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build container executable: %v\n%s", err, output)
	}
	command("", "cp", binary, name+":/usr/local/bin/dsclinux")
	command("", "exec", name, "chmod", "755", "/usr/local/bin/dsclinux")
	command(`set -eu
for tool in groupadd groupmod groupdel gpasswd; do
    native=$(command -v "$tool")
    printf '#!/bin/sh\nprintf "%%s\\n" "%s $*" >> /tmp/dsclinux-mutations\nexec %s "$@"\n' "$tool" "$native" > "/usr/local/bin/$tool"
    chmod 755 "/usr/local/bin/$tool"
done
groupadd -g 31000 dsclinux-base
groupadd -g 31001 dsclinux-unrelated
useradd -M -g dsclinux-base alice
useradd -M -g dsclinux-base bob
useradd -M -g dsclinux-base carol
useradd -M -g dsclinux-base old
useradd -M -g dsclinux-base unrelated
gpasswd -a unrelated dsclinux-unrelated
: > /tmp/dsclinux-mutations
`, "exec", "-i", name, "sh", "-s")
	invokeRaw := func(operation, input string) (string, error) {
		cmd := exec.CommandContext(ctx, docker, "exec", "-i", name, "dsclinux", "groups", operation)
		cmd.Stdin = strings.NewReader(input)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if err != nil {
			return stderr.String(), err
		}
		return stdout.String(), nil
	}
	invoke := func(operation, input string) Group {
		t.Helper()
		output, err := invokeRaw(operation, input)
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", operation, input, err, output)
		}
		var actual Group
		if err := json.NewDecoder(strings.NewReader(output)).Decode(&actual); err != nil {
			t.Fatalf("invalid state: %s (%v)", output, err)
		}
		return actual
	}
	assertMembers := func(input string, want []string) {
		t.Helper()
		actual := invoke("set", input)
		if actual.Members == nil || !slices.Equal(*actual.Members, want) {
			t.Fatalf("members=%v; want %v", actual.Members, want)
		}
		native := strings.TrimSpace(command("", "exec", name, "getent", "group", actual.Name))
		fields := strings.Split(native, ":")
		members := []string{}
		if len(fields) != 4 {
			t.Fatalf("invalid native group: %s", native)
		}
		if fields[3] != "" {
			members = strings.Split(fields[3], ",")
			slices.Sort(members)
		}
		if !slices.Equal(members, want) {
			t.Fatalf("native members=%v; want %v", members, want)
		}
	}
	mutations := func() string {
		return command("", "exec", name, "cat", "/tmp/dsclinux-mutations")
	}
	assertNoop := func(input string) {
		t.Helper()
		before := mutations()
		invoke("get", input)
		output, err := invokeRaw("test", input)
		if err != nil {
			t.Fatalf("Test: %v\n%s", err, output)
		}
		var state struct {
			InDesiredState bool `json:"_inDesiredState"`
		}
		var diff []string
		decoder := json.NewDecoder(strings.NewReader(output))
		if err := decoder.Decode(&state); err != nil || !state.InDesiredState {
			t.Fatalf("compliant Test state: %s %v", output, err)
		}
		if err := decoder.Decode(&diff); err != nil || diff == nil || len(diff) != 0 {
			t.Fatalf("compliant Test diff: %s %v", output, err)
		}
		invoke("set", input)
		invoke("set", input)
		if after := mutations(); before != after {
			t.Fatalf("Get/Test/compliant Set mutated accounts:\n%s\n%s", before, after)
		}
	}
	expectFailure := func(input, message string) {
		t.Helper()
		output, err := invokeRaw("set", input)
		if err == nil || !strings.Contains(output, message) {
			t.Fatalf("expected %q: %v\n%s", message, err, output)
		}
	}

	actual := invoke("set", `{"name":"dsclinux-auto"}`)
	if actual.Ensure != "Present" || actual.GID == nil {
		t.Fatalf("automatic allocation failed: %+v", actual)
	}
	assertNoop(`{"name":"dsclinux-auto"}`)
	invoke("set", `{"name":"dsclinux-auto","ensure":"Absent"}`)
	assertNoop(`{"name":"dsclinux-auto","ensure":"Absent"}`)
	actual = invoke("set", `{"name":"dsclinux-dev","gid":32000}`)
	if actual.GID == nil || *actual.GID != 32000 {
		t.Fatalf("explicit allocation failed: %+v", actual)
	}
	assertNoop(`{"name":"dsclinux-dev"}`)
	command("", "exec", name, "groupadd", "-g", "32002", "dsclinux-conflict")
	expectFailure(`{"name":"dsclinux-dev","gid":32002}`, "groupmod")
	expectFailure(`{"name":"dsclinux-collision","gid":32002}`, "groupadd")
	assertMembers(`{"name":"dsclinux-dev","members":["old","bob"]}`, []string{"bob", "old"})
	assertMembers(`{"name":"dsclinux-dev","includedMembers":["alice"]}`, []string{"alice", "bob", "old"})
	assertMembers(`{"name":"dsclinux-dev","excludedMembers":["old"]}`, []string{"alice", "bob"})
	assertMembers(`{"name":"dsclinux-dev","includedMembers":["carol"],"excludedMembers":["alice"]}`, []string{"bob", "carol"})
	assertMembers(`{"name":"dsclinux-dev","members":["bob","alice"]}`, []string{"alice", "bob"})
	assertNoop(`{"name":"dsclinux-dev","members":["bob","alice"]}`)
	assertMembers(`{"name":"dsclinux-dev","members":[]}`, []string{})
	assertNoop(`{"name":"dsclinux-dev","members":[]}`)
	expectFailure(`{"name":"dsclinux-dev","includedMembers":["does-not-exist"]}`, "does not exist")
	for _, user := range []string{"alice", "bob", "carol", "old"} {
		account := strings.Split(strings.TrimSpace(command("", "exec", name, "getent", "passwd", user)), ":")
		if len(account) != 7 || account[3] != "31000" {
			t.Fatalf("supplementary management changed primary GID: %v", account)
		}
	}
	command("", "exec", name, "useradd", "-M", "-g", "dsclinux-dev", "primaryuser")
	command("", "exec", name, "touch", "/tmp/dsclinux-owned")
	command("", "exec", name, "chown", ":32000", "/tmp/dsclinux-owned")
	before := mutations()
	expectFailure(`{"name":"dsclinux-dev","excludedMembers":["primaryuser"]}`, "primary group")
	expectFailure(`{"name":"dsclinux-dev","gid":32001,"excludedMembers":["primaryuser"]}`, "primary group")
	if before != mutations() {
		t.Fatal("known primary-group exclusion performed an avoidable mutation")
	}
	invoke("set", `{"name":"dsclinux-dev","gid":32001}`)
	account := strings.Split(strings.TrimSpace(command("", "exec", name, "getent", "passwd", "primaryuser")), ":")
	if len(account) != 7 || account[3] != "32001" {
		t.Fatalf("native groupmod primary-GID behavior was not retained: %v", account)
	}
	if gid := strings.TrimSpace(command("", "exec", name, "stat", "-c", "%g", "/tmp/dsclinux-owned")); gid != "32000" {
		t.Fatalf("filesystem ownership was migrated: %s", gid)
	}
	assertNoop(`{"name":"dsclinux-dev","gid":32001,"members":[]}`)
	expectFailure(`{"name":"dsclinux-dev","excludedMembers":["primaryuser"]}`, "primary group")
	expectFailure(`{"name":"dsclinux-dev","ensure":"Absent"}`, "groupdel")
	command("", "exec", name, "userdel", "primaryuser")
	invoke("set", `{"name":"dsclinux-dev","ensure":"Absent"}`)
	assertNoop(`{"name":"dsclinux-dev","ensure":"Absent"}`)

	command("", "exec", name, "groupadd", "-g", "31010", "dsclinux-split")
	command(`set -eu
sed -i '/^dsclinux-split:/d' /etc/group
printf 'dsclinux-split:x:31010:alice\ndsclinux-split:x:31010:bob\n' >> /etc/group
`, "exec", "-i", name, "sh", "-s")
	splitEntries := func() []string {
		t.Helper()
		entries := []string{}
		for _, line := range strings.Split(command("", "exec", name, "cat", "/etc/group"), "\n") {
			if strings.HasPrefix(line, "dsclinux-split:") {
				entries = append(entries, line)
			}
		}
		return entries
	}
	otherGroupEntries := func() []string {
		t.Helper()
		entries := []string{}
		for _, line := range strings.Split(command("", "exec", name, "cat", "/etc/group"), "\n") {
			if line != "" && !strings.HasPrefix(line, "dsclinux-split:") {
				entries = append(entries, line)
			}
		}
		return entries
	}
	if got := invoke("get", `{"name":"dsclinux-split"}`).Members; got == nil ||
		!slices.Equal(*got, []string{"alice", "bob"}) {
		t.Fatalf("split Get members=%v; want [alice bob]", got)
	}
	assertNoop(`{"name":"dsclinux-split","includedMembers":["bob"]}`)
	output, err := invokeRaw("test", `{"name":"dsclinux-split","excludedMembers":["alice"]}`)
	if err != nil {
		t.Fatalf("Test split membership: %v\n%s", err, output)
	}
	var testState struct {
		InDesiredState bool `json:"_inDesiredState"`
	}
	var testDiff []string
	decoder := json.NewDecoder(strings.NewReader(output))
	if err := decoder.Decode(&testState); err != nil {
		t.Fatalf("decode split Test state: %v", err)
	}
	if err := decoder.Decode(&testDiff); err != nil ||
		testState.InDesiredState || !slices.Equal(testDiff, []string{"excludedMembers"}) {
		t.Fatalf("split Test state=%+v diff=%v err=%v", testState, testDiff, err)
	}

	beforeEntries := splitEntries()
	probe := command("", "exec", name, "sh", "-c", `before=$(grep '^dsclinux-split:' /etc/group)
if gpasswd -a carol dsclinux-split >/tmp/gpasswd-split.log 2>&1; then
    echo "gpasswd unexpectedly succeeded on split group"
    exit 1
fi
cat /tmp/gpasswd-split.log
after=$(grep '^dsclinux-split:' /etc/group)
[ "$before" = "$after" ]`)
	if !strings.Contains(probe, "Multiple entries named") || !slices.Equal(splitEntries(), beforeEntries) {
		t.Fatalf("native gpasswd split-group behavior changed: %s; entries=%v", probe, splitEntries())
	}
	mutationBefore := mutations()
	unrelatedGroupsBefore := otherGroupEntries()
	passwdBefore := command("", "exec", name, "cat", "/etc/passwd")
	for _, input := range []string{
		`{"name":"dsclinux-split","includedMembers":["carol"]}`,
		`{"name":"dsclinux-split","excludedMembers":["alice"]}`,
		`{"name":"dsclinux-split","members":["alice","carol"]}`,
		`{"name":"dsclinux-split","members":[]}`,
	} {
		expectFailure(input, "split-group membership modification is unsupported")
		if !slices.Equal(splitEntries(), beforeEntries) ||
			!slices.Equal(otherGroupEntries(), unrelatedGroupsBefore) ||
			command("", "exec", name, "cat", "/etc/passwd") != passwdBefore ||
			mutations() != mutationBefore {
			t.Fatalf("rejected split-group mutation changed account state: %s", input)
		}
	}
	for _, user := range []string{"alice", "bob", "carol", "old"} {
		command("", "exec", name, "getent", "passwd", user)
	}
}
