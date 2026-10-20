import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
import zipfile

import yaml


REPOSITORY = Path(__file__).resolve().parents[2]
MODULE = REPOSITORY / "resources" / "package-management"
MANIFESTS = {
    "aptRepositories.dsc.resource.json": "DscExtras.PackageManagement/aptRepositories",
    "dscapt.dsc.resource.json": "DscExtras.Apt/Repository",
}
WORKFLOW = yaml.load(
    (REPOSITORY / ".github" / "workflows" / "dscpm.yml").read_text(),
    Loader=yaml.BaseLoader,
)
RELEASE_CONDITION = "github.event_name == 'push' && startsWith(github.ref, 'refs/tags/dscpm-v')"


def step(job, name):
    return next(item for item in WORKFLOW["jobs"][job]["steps"] if item.get("name") == name)


class ReleaseSafeguards(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix=".dscpm-release-test-", dir=REPOSITORY)
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        scratch = self.root / "scratch"
        scratch.mkdir()
        self.env = dict(
            os.environ,
            GIT_CONFIG_NOSYSTEM="1",
            GIT_CONFIG_GLOBAL=os.devnull,
            GIT_AUTHOR_NAME="Release test",
            GIT_AUTHOR_EMAIL="release-test@example.invalid",
            GIT_COMMITTER_NAME="Release test",
            GIT_COMMITTER_EMAIL="release-test@example.invalid",
            GITHUB_EVENT_NAME="push",
            GITHUB_REF="refs/tags/dscpm-v1.2.3",
            GITHUB_REF_NAME="dscpm-v1.2.3",
            GITHUB_OUTPUT=str(self.root / "output"),
            GH_REPO="example/dsc-extras",
            VERSION="1.2.3",
            TMPDIR=str(scratch),
        )

    def git(self, directory, *args):
        result = subprocess.run(
            ["git", "-C", str(directory), *args], env=self.env, text=True,
            capture_output=True,
        )
        self.assert_success(result)
        return result.stdout.strip()

    def run_script(self, script, directory=None, **environment):
        return subprocess.run(
            ["bash", "--noprofile", "--norc", "-e", "-o", "pipefail"],
            input=script, text=True, cwd=directory or self.root,
            env=dict(self.env, **environment), capture_output=True,
        )

    def assert_success(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_tag_ancestry_and_unchanged_refs(self):
        origin = self.root / "origin"
        origin.mkdir()
        self.git(origin, "init", "-q", "--initial-branch=main")
        self.git(origin, "commit", "-q", "--allow-empty", "-m", "merged commit")
        merged = self.git(origin, "rev-parse", "HEAD")
        self.git(origin, "commit", "-q", "--allow-empty", "-m", "new main tip")
        self.git(origin, "checkout", "-q", "-b", "unmerged", merged)
        self.git(origin, "commit", "-q", "--allow-empty", "-m", "unmerged commit")
        unmerged = self.git(origin, "rev-parse", "HEAD")
        self.git(origin, "checkout", "-q", "--orphan", "unrelated")
        self.git(origin, "commit", "-q", "--allow-empty", "-m", "unrelated root")
        unrelated = self.git(origin, "rev-parse", "HEAD")
        self.git(origin, "checkout", "-q", "main")
        preflight = step("test", "Verify release commit is on main")["run"]

        for target, accepted in [(merged, True), (unmerged, False), (unrelated, False)]:
            for annotated in (False, True):
                for shallow in (False, True):
                    with self.subTest(target=target, annotated=annotated, shallow=shallow):
                        suffix = f"{target[:8]}-{annotated}-{shallow}"
                        tag = f"dscpm-v1.2.{len(self.git(origin, 'tag').splitlines())}"
                        args = ["tag", tag, target]
                        if annotated:
                            args += ["-a", "-m", "annotated release"]
                        self.git(origin, *args)
                        clone = self.root / suffix
                        clone.mkdir()
                        self.git(clone, "init", "-q")
                        self.git(clone, "remote", "add", "origin", origin.as_uri())
                        ref = f"refs/tags/{tag}"
                        args = ["fetch", "-q", "--no-tags"]
                        if shallow:
                            args += ["--depth=1"]
                        self.git(clone, *args, "origin", f"{ref}:{ref}")
                        self.git(clone, "checkout", "-q", "--detach", tag)
                        before = self.git(clone, "rev-parse", ref)
                        origin_refs = self.git(origin, "show-ref", "--tags")
                        self.assertEqual(
                            self.git(clone, "rev-parse", "--is-shallow-repository"),
                            str(shallow).lower(),
                        )
                        result = self.run_script(
                            preflight, clone, GITHUB_REF=ref, GITHUB_REF_NAME=tag,
                        )
                        if accepted:
                            self.assert_success(result)
                            self.assertEqual(self.git(clone, "rev-parse", f"{ref}^{{commit}}"), merged)
                        else:
                            self.assertNotEqual(result.returncode, 0)
                            self.assertIn("not reachable from origin/main", result.stdout)
                        self.assertEqual(self.git(clone, "rev-parse", ref), before)
                        self.assertEqual(self.git(origin, "show-ref", "--tags"), origin_refs)

    def test_versions_and_ordinary_ci(self):
        (self.root / "manifests").mkdir()
        for manifest in MANIFESTS:
            (self.root / "manifests" / manifest).write_text(json.dumps({"version": "0.1.0"}))
        resolve = step("test", "Resolve version")["run"]
        output = self.root / "output"
        for version in ("0.0.0", "1.2.3", "12.34.567"):
            with self.subTest(version=version):
                output.unlink(missing_ok=True)
                self.assert_success(self.run_script(resolve, GITHUB_REF=f"refs/tags/dscpm-v{version}"))
                self.assertEqual(output.read_text(), f"version={version}\n")
        for version in ("", "1.2", "01.2.3", "1.02.3", "1.2.03", "1.2.3-rc.1", "1.2.3+build"):
            with self.subTest(invalid_version=version):
                output.unlink(missing_ok=True)
                result = self.run_script(resolve, GITHUB_REF=f"refs/tags/dscpm-v{version}")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Expected a stable X.Y.Z version", result.stdout)
                self.assertFalse(output.exists())
        for event, ref in [
            ("push", "refs/heads/feature"),
            ("pull_request", "refs/pull/4/merge"),
            ("workflow_dispatch", "refs/heads/feature"),
            ("workflow_dispatch", "refs/tags/dscpm-v1.2.3"),
            ("push", "refs/tags/dscapt-v1.2.3"),
        ]:
            with self.subTest(event=event, ref=ref):
                output.unlink(missing_ok=True)
                self.assert_success(self.run_script(resolve, GITHUB_EVENT_NAME=event, GITHUB_REF=ref))
                self.assertEqual(output.read_text(), "version=0.1.0\n")

    def test_job_gates_and_permissions(self):
        jobs = WORKFLOW["jobs"]
        self.assertEqual(step("test", "Verify release commit is on main")["if"], RELEASE_CONDITION)
        self.assertEqual(jobs["publish"]["if"], RELEASE_CONDITION)
        self.assertEqual(jobs["build"]["needs"], "test")
        self.assertEqual(set(jobs["publish"]["needs"]), {"test", "build"})
        self.assertEqual(jobs["build"]["strategy"]["matrix"]["goarch"], ["amd64", "arm64"])
        self.assertEqual(WORKFLOW["permissions"], {"contents": "read"})
        for name in ("test", "build"):
            self.assertNotIn("permissions", jobs[name])
        self.assertEqual(jobs["publish"]["permissions"], {"contents": "write"})
        self.assertEqual(jobs["publish"]["concurrency"], {
            "group": "dscpm-release-${{ github.ref }}",
            "cancel-in-progress": "false",
        })
        names = [item.get("name") for item in jobs["test"]["steps"]]
        self.assertLess(names.index("Resolve version"), names.index("Verify release commit is on main"))

    def test_refactored_paths_and_commands(self):
        self.assertEqual(WORKFLOW["name"], "dscpm")
        for event in ("pull_request", "push"):
            self.assertEqual(WORKFLOW["on"][event]["paths"], [
                ".github/workflows/dscpm.yml",
                ".github/tests/test_dscpm_release.py",
                "resources/package-management/**",
            ])
        self.assertEqual(WORKFLOW["on"]["push"]["tags"], ["dscpm-v*"])
        for job in ("test", "build"):
            self.assertEqual(
                WORKFLOW["jobs"][job]["defaults"]["run"]["working-directory"],
                "resources/package-management",
            )
            setup = next(item for item in WORKFLOW["jobs"][job]["steps"]
                         if item.get("uses") == "actions/setup-go@v5")
            self.assertEqual(setup["with"], {
                "go-version-file": "resources/package-management/go.mod",
                "cache-dependency-path": "resources/package-management/go.mod",
            })
        test_steps = WORKFLOW["jobs"]["test"]["steps"]
        setup_index = next(index for index, item in enumerate(test_steps)
                           if item.get("uses") == "actions/setup-go@v5")
        self.assertLess(setup_index, test_steps.index(step("test", "Test release safeguards")))
        self.assertIn("../../.github/tests/test_dscpm_release.py",
                      step("test", "Test release safeguards")["run"])
        self.assertEqual(step("test", "Race test")["run"], "go test -race ./...")
        self.assertEqual(WORKFLOW["jobs"]["test"]["env"]["DSCPM_REQUIRE_DSC"], "1")
        install = step("test", "Install DSC")
        self.assertNotIn("if", install)
        self.assertNotIn("continue-on-error", install)
        self.assertLess(test_steps.index(install), test_steps.index(step("test", "Test")))
        self.assertLess(test_steps.index(install), test_steps.index(step("test", "Race test")))
        self.assertIn("releases/download/v3.3.0/DSC-3.3.0-x86_64-linux.tar.gz", install["run"])
        self.assertIn("sha256sum --check", install["run"])
        self.assertIn('= "dsc 3.3.0"', install["run"])
        self.assertIn('>> "$GITHUB_PATH"', install["run"])
        upload = next(item for item in WORKFLOW["jobs"]["build"]["steps"]
                      if item.get("uses") == "actions/upload-artifact@v4")
        self.assertEqual(upload["with"], {
            "name": "dscpm-linux-${{ matrix.goarch }}",
            "path": "resources/package-management/dist/dscpm-${{ needs.test.outputs.version }}-linux-${{ matrix.goarch }}.zip",
            "if-no-files-found": "error",
        })
        download = WORKFLOW["jobs"]["publish"]["steps"][0]
        self.assertEqual(download["uses"], "actions/download-artifact@v4")
        self.assertEqual(download["with"], {
            "pattern": "dscpm-linux-*", "path": "dist", "merge-multiple": "true",
        })
        for filename, resource_type in MANIFESTS.items():
            with self.subTest(manifest=filename):
                manifest = json.loads((MODULE / "manifests" / filename).read_text())
                self.assertEqual(manifest["type"], resource_type)
                for operation in ("get", "test", "set"):
                    self.assertEqual(manifest[operation]["executable"], "dscpm")
                    self.assertEqual(manifest[operation]["args"], ["aptRepositories", operation])

    def test_build_and_package_both_architectures(self):
        module = self.root / "resources" / "package-management"
        shutil.copytree(MODULE, module, ignore=shutil.ignore_patterns("dist"))
        shutil.copyfile(REPOSITORY / "LICENSE", self.root / "LICENSE")
        build = step("build", "Build and package")
        self.assertEqual(build["env"]["CGO_ENABLED"], "0")
        self.assertEqual(build["env"]["GOOS"], "linux")
        verify = step("build", "Verify package")["run"]
        expected_members = {"dscpm", *MANIFESTS, "LICENSE"}
        for architecture in ("amd64", "arm64"):
            with self.subTest(architecture=architecture):
                environment = dict(GOARCH=architecture, GOOS="linux", CGO_ENABLED="0")
                self.assert_success(self.run_script(build["run"], module, **environment))
                self.assert_success(self.run_script(verify, module, **environment))
                archive = module / "dist" / f"dscpm-1.2.3-linux-{architecture}.zip"
                with zipfile.ZipFile(archive) as package:
                    self.assertEqual(set(package.namelist()), expected_members)
                    self.assertEqual(len(package.namelist()), len(expected_members))
                    self.assertTrue((package.getinfo("dscpm").external_attr >> 16) & 0o111)
                    self.assertEqual(package.read("LICENSE"), (REPOSITORY / "LICENSE").read_bytes())
                    for filename, resource_type in MANIFESTS.items():
                        manifest = json.loads(package.read(filename))
                        self.assertEqual(manifest["version"], self.env["VERSION"])
                        self.assertEqual(manifest["type"], resource_type)
                        for operation in ("get", "test", "set"):
                            self.assertEqual(manifest[operation]["executable"], "dscpm")
                            self.assertEqual(manifest[operation]["args"], ["aptRepositories", operation])

        archive = module / "dist" / "dscpm-1.2.3-linux-amd64.zip"
        original = archive.read_bytes()
        with zipfile.ZipFile(archive) as package:
            entries = [(info, package.read(info)) for info in package.infolist()]
        mutations = [("extra-member", None)]
        for filename in MANIFESTS:
            mutations.extend((filename, field) for field in ("version", "type", "executable", "args"))
        for filename, field in mutations:
            with self.subTest(rejected_manifest=filename, rejected_field=field):
                shutil.rmtree(module / "dist" / "check-amd64", ignore_errors=True)
                with zipfile.ZipFile(archive, "w") as package:
                    for info, content in entries:
                        if info.filename == filename:
                            manifest = json.loads(content)
                            if field in ("version", "type"):
                                manifest[field] = "invalid"
                            elif field == "executable":
                                manifest["get"]["executable"] = "dscapt"
                            elif field == "args":
                                manifest["set"]["args"] = ["aptRepositories", "get"]
                            content = json.dumps(manifest).encode()
                        package.writestr(info, content)
                    if filename == "extra-member":
                        package.writestr("unexpected", "must not ship")
                result = self.run_script(verify, module, GOARCH="amd64")
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        archive.write_bytes(original)

    def test_draft_creation_and_failures(self):
        assets = self.root / "dist"
        assets.mkdir()
        paths = [assets / f"dscpm-1.2.3-linux-{arch}.zip" for arch in ("amd64", "arm64")]
        for path in paths:
            path.write_bytes(b"test asset")
        calls = self.root / "calls"
        script = """
gh() {
  if [[ "$1" == api ]]; then
    [[ "$*" == "api --paginate repos/example/dsc-extras/releases --jq .[].tag_name" ]] || return 99
    printf '%s\\n' "$EXISTING_TAGS"
    return "$API_EXIT"
  fi
  printf '%s\\n' "$@" > "$CALLS"
  return "$CREATE_EXIT"
}
""" + step("publish", "Create draft release")["run"]
        env = dict(EXISTING_TAGS="", API_EXIT="0", CREATE_EXIT="0", CALLS=str(calls))
        self.assert_success(self.run_script(script, **env))
        self.assertEqual(calls.read_text().splitlines()[:5], [
            "release", "create", "dscpm-v1.2.3",
            "dist/dscpm-1.2.3-linux-amd64.zip", "dist/dscpm-1.2.3-linux-arm64.zip",
        ])
        self.assertIn("--draft", calls.read_text().splitlines())
        self.assertIn("--verify-tag", calls.read_text().splitlines())
        self.assertNotIn("--clobber", calls.read_text().splitlines())
        for existing in ("dscpm-v1.2.3", "older-tag\ndscpm-v1.2.3\nnewer-tag"):
            calls.unlink(missing_ok=True)
            result = self.run_script(script, **dict(env, EXISTING_TAGS=existing))
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("release or draft", result.stdout)
            self.assertFalse(calls.exists())
        self.assertNotEqual(self.run_script(script, **dict(env, API_EXIT="1")).returncode, 0)
        self.assertFalse(calls.exists())
        self.assertNotEqual(self.run_script(script, **dict(env, CREATE_EXIT="1")).returncode, 0)
        for path in paths:
            calls.unlink(missing_ok=True)
            for empty in (False, True):
                path.unlink(missing_ok=True)
                if empty:
                    path.touch()
                result = self.run_script(script, **env)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Missing release asset", result.stdout)
                self.assertFalse(calls.exists())
            path.write_bytes(b"test asset")


if __name__ == "__main__":
    unittest.main()
