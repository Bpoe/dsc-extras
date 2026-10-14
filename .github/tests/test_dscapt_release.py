import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml


WORKFLOW = yaml.load(
    (Path(__file__).resolve().parents[1] / "workflows" / "dscapt.yml").read_text(),
    Loader=yaml.BaseLoader,
)
RELEASE_CONDITION = "github.event_name == 'push' && startsWith(github.ref, 'refs/tags/dscapt-v')"


def step(job, name):
    return next(item for item in WORKFLOW["jobs"][job]["steps"] if item.get("name") == name)


class ReleaseSafeguards(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="dscapt-release-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.env = dict(
            os.environ,
            GIT_CONFIG_NOSYSTEM="1",
            GIT_CONFIG_GLOBAL=os.devnull,
            GIT_AUTHOR_NAME="Release test",
            GIT_AUTHOR_EMAIL="release-test@example.invalid",
            GIT_COMMITTER_NAME="Release test",
            GIT_COMMITTER_EMAIL="release-test@example.invalid",
            GITHUB_EVENT_NAME="push",
            GITHUB_REF="refs/tags/dscapt-v1.2.3",
            GITHUB_REF_NAME="dscapt-v1.2.3",
            GITHUB_OUTPUT=str(self.root / "output"),
            GH_REPO="example/dsc-extras",
            VERSION="1.2.3",
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
                        tag = f"dscapt-v1.2.{len(self.git(origin, 'tag').splitlines())}"
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
        (self.root / "dscapt.dsc.resource.json").write_text(json.dumps({"version": "0.1.0"}))
        resolve = step("test", "Resolve version")["run"]
        output = self.root / "output"
        for version in ("0.0.0", "1.2.3", "12.34.567"):
            with self.subTest(version=version):
                output.unlink(missing_ok=True)
                self.assert_success(self.run_script(resolve, GITHUB_REF=f"refs/tags/dscapt-v{version}"))
                self.assertEqual(output.read_text(), f"version={version}\n")
        for version in ("", "1.2", "01.2.3", "1.02.3", "1.2.03", "1.2.3-rc.1", "1.2.3+build"):
            with self.subTest(invalid_version=version):
                output.unlink(missing_ok=True)
                result = self.run_script(resolve, GITHUB_REF=f"refs/tags/dscapt-v{version}")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Expected a stable X.Y.Z version", result.stdout)
                self.assertFalse(output.exists())
        for event, ref in [
            ("push", "refs/heads/feature"),
            ("pull_request", "refs/pull/4/merge"),
            ("workflow_dispatch", "refs/heads/feature"),
            ("workflow_dispatch", "refs/tags/dscapt-v1.2.3"),
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
            "group": "dscapt-release-${{ github.ref }}",
            "cancel-in-progress": "false",
        })
        names = [item.get("name") for item in jobs["test"]["steps"]]
        self.assertLess(names.index("Resolve version"), names.index("Verify release commit is on main"))

    def test_draft_creation_and_failures(self):
        assets = self.root / "dist"
        assets.mkdir()
        paths = [assets / f"dscapt-1.2.3-linux-{arch}.zip" for arch in ("amd64", "arm64")]
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
            "release", "create", "dscapt-v1.2.3",
            "dist/dscapt-1.2.3-linux-amd64.zip", "dist/dscapt-1.2.3-linux-arm64.zip",
        ])
        self.assertIn("--draft", calls.read_text().splitlines())
        self.assertIn("--verify-tag", calls.read_text().splitlines())
        self.assertNotIn("--clobber", calls.read_text().splitlines())
        for existing in ("dscapt-v1.2.3", "older-tag\ndscapt-v1.2.3\nnewer-tag"):
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
