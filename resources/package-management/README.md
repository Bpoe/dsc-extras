# dscpm: Package management component

`dscpm` is the package management component in `resources/package-management`,
with its executable entry point in `cmd/dscpm`. It currently implements `Get`,
`Test`, and `Set` for the DSCv3 resources
`DscExtras.PackageManagement/aptRepositories` and
`DscExtras.PackageManagement/packages`. The repository resource manages a logical binary-package (`deb`)
repository, not a particular filename. Existing configurations created by
Ansible, a Debian package, or DSC can already satisfy the desired state.

Resource names use a PascalCase namespace (`DscExtras.PackageManagement`) and
plural camelCase type (`aptRepositories`, `packages`). Package management currently
supports APT/dpkg on Debian-family Linux only. DNF repositories and packages
(`dnfRepositories` and a future DNF package backend) are deferred, not implemented.

The repository implementation retains the structural rename from `dscapt` in
`resources/apt`; existing repository behavior and safety rules are unchanged.
The manifest
`manifests/aptRepositories.dsc.resource.json` exposes
`DscExtras.PackageManagement/aptRepositories`. The compatibility manifest
`manifests/dscapt.dsc.resource.json` preserves `DscExtras.Apt/Repository` for
existing DSC configurations. Both invoke `dscpm aptRepositories get`, `test`,
and `set`. There is no `dscapt` binary shim: old direct CLI invocations and
build paths must migrate to the new command and directory.

## Command line and installation

`dscpm --help` displays usage; `dscpm --version` displays the executable version.
Operations read a single JSON properties object from standard
input:

```sh
dscpm aptRepositories get < repository.json
dscpm aptRepositories test < repository.json
dscpm aptRepositories set < repository.json
dscpm packages get < packages.json
dscpm packages test < packages.json
dscpm packages set < packages.json
```

Install `dscpm`, `aptRepositories.dsc.resource.json`,
`dscapt.dsc.resource.json`, and `packages.dsc.resource.json` together in a directory on `PATH`, with all manifests
beside the executable. Changing system configuration requires sufficient
filesystem permissions.

## Repository identity and state

Every operation requires `name`, `uri`, and `suite`. URI and suite identify the
repository; `name` is a management label used to choose filenames **only when
creating a missing repository**. A trailing URI slash is equivalent, except in
URIs containing query strings. Changing a suite selects a different repository;
remove the old URI/suite explicitly if migrating it.

Discovery reads `/etc/apt/sources.list`, `/etc/apt/sources.list.d/*.list`, and
`/etc/apt/sources.list.d/*.sources`. Multiple lines/stanzas and Deb822
multi-value Types, URIs, and Suites are supported. `deb-src` definitions are
retained, not managed as `deb`. Enabled `deb-src` entries with the selected URI
and suite must pass the same security-option validation as `deb` entries, as
well as the existing `Signed-By` consistency check. Unsupported or unsafe
options cause `Get`/`Test` errors for an existing binary repository and prevent
`Set` from writing, including when creating a missing binary repository.

`components` and `architectures` are compared as unordered sets. Omitted
architectures mean APT's configured host defaults, **not** an arbitrary explicit
architecture list. Explicit architecture additions/removals are evaluated when
the base restriction is explicit; host-dependent modifiers require manual
clarification. `enabled` defaults to `true`; `false` is represented by
`Enabled: no` or a commented-out `deb` line. `ensure: Absent` removes the matching
definition, not unrelated entries or signing keyrings.

`signingKey` is the complete inline ASCII-armored OpenPGP public key, required
for `Present`. Existing `.asc` and binary OpenPGP `.gpg` references and Deb822
embedded keys are compared by parsed public-key material. Multiple keyrings
and primary-key fingerprint selectors are supported. Subkey-only selectors,
`!` restrictions, and selectors relying on global trust are recognized but
require explicit reconciliation rather than guessing equivalent trust.
An unrestricted/global-trust source is never compliant with explicit signing
material. Trust overrides such as `Trusted: yes` or `Trusted: no`, disabled
signature/freshness checks, and unsupported behavioral options produce
actionable errors. Unknown Deb822 `X-` metadata is retained.

`Test` uses DSC's `stateAndDiff` contract: `dscpm aptRepositories test` emits exactly two
newline-delimited JSON values, the actual state including `_inDesiredState`,
then an array of semantically differing property names (`[]` when compliant).
The observed `signingKey` remains in the actual state even when its armor
differs from the desired input. Compliance and differences use the same
comparison; a missing repository desired `Present` reports only `ensure`.

## Preservation and safety

`AptRepository` holds logical properties; `AptDocument` retains a source's path,
format, original bytes, and entries; `AptKeyring` retains its reference, format,
original bytes, and parsed public-key material. An unchanged `WriteToDisk()`
does not open files for writing, change modification times or permissions,
replace files, normalize formatting, or create keyrings.

`Test` and `Get` are read-only. A compliant `Set` returns before any write or
directory creation, including for package-owned sources and shared keyrings.
Safe changes retain an existing source's format and location, changing only
affected lines or fields. A changed member of a multi-value Deb822 stanza is
split into individual stanzas with the other repositories' properties retained.
Unchanged documents and keyrings are never serialized.

A missing repository creates:

- `/etc/apt/sources.list.d/<name>.sources`, marked as managed by DSC.
- `/etc/apt/keyrings/<name>.gpg`, containing the supplied public key.

The managed source marker remains `# Managed by dscapt: <name>` for compatibility
with existing managed files. The rename does not rewrite this marker or change
the ownership checks that allow safe managed-key rotation.

An equivalent existing keyring at the creation path is reused unchanged.
Unrelated files occupying either creation path are not overwritten. Existing
external signing material is not rotated automatically. DSC-created keys can
be rotated only when the marked source is editable and no other source
(including disabled sources, `deb-src`, and filesystem aliases) references
the key. Removing a repository intentionally retains its keyrings.

Before any change, the resource checks the dpkg file database under
`/var/lib/dpkg/info/*.list`. It refuses changes to package-owned configuration
or keys, including trust changes that would leave a package source's bytes
unchanged. It also refuses writes through symlinks, stale discovery snapshots,
malformed source files, duplicate/conflicting definitions, and ambiguous edits
to repositories distributed across multiple entries. Consistent entries with
disjoint components can collectively satisfy a desired repository without edits.
If ownership cannot be established, no change is made. Errors identify the
source, owner, or ambiguity that must be resolved explicitly.

Key and source writes use separate atomic file replacements, not a filesystem
transaction. All safety checks run before either write. An I/O failure between
replacements can leave an unreferenced managed key; a retry reuses it unchanged.
Concurrent external configuration writers must be serialized operationally;
snapshot checks detect intervening changes but are not a system-wide APT lock.

The repository resource does not install packages, run `apt update`, download
keys, or check remote repository availability. Use the separate `packages`
resource for package operations. External binary keyrings require APT 1.4+,
ASCII-armored keyrings APT 1.4+, and embedded Deb822 keys APT 2.4+.

## Example

```json
{
  "name": "docker",
  "uri": "https://download.docker.com/linux/debian",
  "suite": "trixie",
  "components": ["stable"],
  "architectures": ["amd64"],
  "enabled": true,
  "signingKey": "-----BEGIN PGP PUBLIC KEY BLOCK-----\n...\n-----END PGP PUBLIC KEY BLOCK-----"
}
```

For `Get` or removal, signing material and component/architecture properties
are not needed:

```json
{
  "name": "docker",
  "uri": "https://download.docker.com/linux/debian",
  "suite": "trixie",
  "ensure": "Absent"
}
```

This intentionally replaces the earlier filename-only `Get`/`Absent` contract.
Use these properties as the JSON input to `dscpm aptRepositories get`, `test`,
or `set`, as appropriate.

### Parameterized DSC configuration

Use parameters to supply the Ubuntu codename (for example, `noble`), architecture
(for example, `amd64`), and complete Docker public signing key:

```yaml
$schema: https://aka.ms/dsc/schemas/v3/bundled/config/document.json

parameters:
  ubuntuCodename:
    type: string
  architecture:
    type: string
  dockerSigningKey:
    type: string

resources:
  - name: Docker APT Repository
    type: DscExtras.PackageManagement/aptRepositories
    properties:
      name: docker
      uri: https://download.docker.com/linux/ubuntu
      suite: "[parameters('ubuntuCodename')]"
      components:
        - stable
      architectures:
        - "[parameters('architecture')]"
      signingKey: "[parameters('dockerSigningKey')]"
      enabled: true
```

### Changing suites explicitly

Changing `suite` from `bookworm` to `trixie` selects a different repository; it
does not migrate or remove the `bookworm` definition. If the old repository is
no longer wanted, explicitly apply:

```json
{
  "name": "docker",
  "uri": "https://download.docker.com/linux/debian",
  "suite": "bookworm",
  "ensure": "Absent"
}
```

Then apply the `Present` example above with `"suite": "trixie"` and, for example,
`"name": "docker-trixie"`. A distinct name avoids colliding with the retained
old source document/keyring when new files are needed. Existing trixie
definitions are still discovered by URI and suite regardless of their names.
This removes only the old `deb` definition; related `deb-src` entries must be
managed separately.

## APT packages

`DscExtras.PackageManagement/packages` manages installed packages, not repository
definitions. It requires APT 2.2+ (Debian 11+/Ubuntu 22.04+) and `apt-get`, `dpkg`,
`dpkg-query`, and `dpkg-deb` on `PATH`. Its fail-closed cache refresh
(`APT::Update::Error-Mode=any`) and disabled legacy regex fallback
(`APT::Cmd::Pattern-Only=true`) rely on current APT behavior.

Use `name` as one package name or a nonempty array. An entry can
also be a local `.deb` path; arrays can mix repository names and local files.
Local paths may be absolute or relative to the resource process's working
directory and must identify existing regular files. Arrays are unordered sets
without duplicate entries. `ensure` defaults to `Present` and accepts only
`Present` or `Absent`. Repository names may include an architecture qualifier,
such as `libc6:amd64`. Unqualified names select the host's native architecture
(or an architecture-independent `all` package). Use `:arch` for a configured
foreign architecture; wildcard and `native`/`any` architecture aliases are not
supported.

For an unversioned repository package, **any installed version is compliant**.
This resource does not upgrade an already installed package to the newest
candidate. A scalar repository package with `ensure: Present` may specify
`version` to require an exact Debian version. `version` is not supported with an
array, a local file, or `ensure: Absent`. A local `.deb` supplies its own exact
package name, version, and architecture from its metadata; its file must remain
available on every Get/Test/Set reconciliation, even after installation.
Replacing or removing that file can therefore change or invalidate the desired
state. Changing only a local archive's bytes, while its package name, version,
and architecture remain unchanged, does not trigger a reinstall.

Exact-version requests can upgrade packages, but a downgrade requires explicit
`allowDowngrade: true`; otherwise reconciliation fails rather than silently
downgrading. Version equality and ordering use `dpkg --compare-versions`, including
native Debian epoch, revision, and tilde semantics, not semantic-version or
lexical ordering. `Get` and `Test` are read-only. `Set` computes the minimal set of
noncompliant packages and applies them in one APT install/remove transaction for
the entire batch, not one invocation per entry. Only noncompliant local artifacts
are installed. In a necessary mixed install, an already-compliant exact local
package may also appear as a `name:architecture=installedVersion` solver
constraint, not an archive path, to prevent dependency resolution from changing
its version; it does not reinstall the compliant archive. Compliant unversioned
repository packages are excluded; an entirely compliant group remains a strict
no-op. APT may install dependencies needed by that transaction.
Package mutations and cache updates require root/equivalent privileges and the
host's APT/dpkg tools; failures such as unavailable versions, dependency
conflicts, or package-manager locks are errors, not successful compliance.
Package commands execute directly without a shell. Repository names are
validated and passed as exact `name:architecture` targets with APT's legacy
regular-expression fallback disabled, rather than interpreted as package
patterns or install/remove suffixes.

Version wildcards/ranges, package holds, automatic upgrades, remote URL inputs,
and removal by `.deb` path are not supported. To remove a package originally
installed from a local archive, request `ensure: Absent` with its package name.
Cancellation can interrupt APT while a child dpkg process may still finish;
inspect the host's package-manager state and reconcile again before assuming
the operation rolled back or retrying concurrently.
One APT transaction does not guarantee rollback: an APT failure or failed
post-operation verification may leave partial changes. The next reconciliation
queries installed state again before planning further work.

### Observed package state

The read-only `state` property summarizes installation across the request:
`Present` means all requested packages are fully installed, `Absent` means none
have active installation state (residual configuration files are acceptable),
and `Mixed` covers a partially installed batch or an incomplete dpkg installation.
This is observed state, not a third allowed value for desired `ensure`. A package
can be installed but still fail an exact-version comparison. All top-level input
properties (`name`, `ensure`, and any `version` or options) retain normalized
desired intent in the output, not observed installation. In particular, a
top-level `version` remains the requested target even when the installed version
differs. Read actual installed versions and architectures only from
`packages[].version` and `packages[].architecture`, and aggregate installation
from `state`.

The read-only `packages` array reports one entry per requested package:

| Property | Meaning |
| --- | --- |
| `requestedName` | Original requested repository name or local `.deb` path. |
| `name` | Resolved base package name, including the name read from local metadata; architecture is separate. |
| `installed` | Whether the requested package is installed. |
| `status` | Full observed dpkg status string, such as `install ok installed`; empty when no record exists. |
| `version` | Observed installed version. |
| `architecture` | Observed package architecture. |

The entries are sorted by `requestedName`; `version` and `architecture` can be
omitted when unavailable. Do not supply `state` or `packages` as desired
properties. `Test` uses the same `stateAndDiff` contract as the repository
resource: actual state with `_inDesiredState`, followed by the array of differing
property names.
Real DSC `stateAndDiff` explicitly reports `version` differences independently
of the retained top-level intent fields.

### Package examples

Pass these properties to `dscpm packages get`, `test`, or `set`, or use them in a
DSC resource of type `DscExtras.PackageManagement/packages`.

One package, accepting any installed version:

```json
{"name": "curl", "ensure": "Present"}
```

Multiple repository packages:

```json
{"name": ["curl", "jq", "git"], "ensure": "Present"}
```

Remove a package (use an array to remove several). Removal accepts repository
package names, not local paths, and does not support `version`,
`allowDowngrade: true`, or `updateCache: true`:

```json
{"name": "telnet", "ensure": "Absent"}
```

Install a local file, requiring the version and architecture in its metadata:

```json
{"name": "/srv/packages/example-agent_1.2.3_amd64.deb", "ensure": "Present"}
```

A batch containing only local archives:

```json
{"name": ["/srv/packages/example-agent_1.2.3_amd64.deb", "/srv/packages/example-tools_2.0.0_amd64.deb"], "ensure": "Present"}
```

A mixed repository/local batch, reconciled in one APT transaction:

```json
{"name": ["curl", "/srv/packages/example-agent_1.2.3_amd64.deb"], "ensure": "Present"}
```

Require an exact repository version (the version must be available from APT):

```json
{"name": "curl", "version": "8.5.0-2ubuntu10.6", "ensure": "Present"}
```

Explicitly permit a downgrade to that exact version when needed:

```json
{"name": "curl", "version": "8.5.0-2ubuntu10.6", "ensure": "Present", "allowDowngrade": true}
```

### Cache refresh and repository ordering

`updateCache: true` requests an APT cache refresh during `Set`, before a needed
package installation transaction. It does not force a cache-only update when
the packages are already compliant. `Get` and `Test` never refresh the cache;
neither does the repository resource. Declare `dependsOn` so repository
configuration completes before the package resource refreshes and installs:

```yaml
$schema: https://aka.ms/dsc/schemas/v3/bundled/config/document.json

parameters:
  ubuntuCodename:
    type: string
  dockerSigningKey:
    type: string

resources:
  - name: Docker APT Repository
    type: DscExtras.PackageManagement/aptRepositories
    properties:
      name: docker
      uri: https://download.docker.com/linux/ubuntu
      suite: "[parameters('ubuntuCodename')]"
      components: [stable]
      signingKey: "[parameters('dockerSigningKey')]"
  - name: Docker packages
    type: DscExtras.PackageManagement/packages
    dependsOn:
      - "[resourceId('DscExtras.PackageManagement/aptRepositories', 'Docker APT Repository')]"
    properties:
      name: [docker-ce, docker-ce-cli, containerd.io]
      ensure: Present
      updateCache: true
```

## Releases

Release tags must reference commits already merged into `main`. Fetch the latest
`main`, tag the intended merged commit, and push that tag. For example, to release
the current `origin/main`:

```sh
git fetch origin main
git tag -a dscpm-v1.2.3 origin/main -m "dscpm 1.2.3"
git push origin dscpm-v1.2.3
```

The `dscpm` GitHub Actions workflow accepts stable `dscpm-vX.Y.Z` tags only;
prerelease suffixes and leading zeroes in version components are not supported.
Both lightweight and annotated tags are supported. Before the architecture
builds start, the workflow resolves the tag to its commit, fetches the full
history needed for `origin/main`, and checks that `main` contains that commit.
Tagging an unmerged branch or an unrelated commit fails the release. The
workflow does not move or modify tags; release tags are immutable. Historical
`dscapt` releases and tags remain untouched.

After the checks and both builds succeed, it creates an unpublished
[draft GitHub Release](https://github.com/Bpoe/dsc-extras/releases) with these assets:

- `dscpm-1.2.3-linux-amd64.zip` for x86-64 Linux.
- `dscpm-1.2.3-linux-arm64.zip` for ARM64/AArch64 Linux.

Each ZIP contains exactly five files at its root: `dscpm`,
`aptRepositories.dsc.resource.json`, `dscapt.dsc.resource.json`,
`packages.dsc.resource.json`, and `LICENSE`.
The executable is built without CGO and retains its Linux executable permission
in the archive. The tag supplies the version for `dscpm --version` and all three
packaged manifests; the checked-in source versions are not changed.

The maintainer must inspect both ZIP assets in the draft and manually publish
the release through GitHub. The workflow never automatically publishes a draft.
Existing releases and drafts are not overwritten; rerunning the workflow for a
tag that already has either will fail. Use a new version for subsequent changes.

Once the maintainer publishes the release, download the ZIP for your architecture
and extract it into a directory on `PATH`, keeping all three manifests beside the
executable. For example:

```sh
install_dir="$HOME/.local/share/dscpm/1.2.3"
mkdir -p "$install_dir"
unzip dscpm-1.2.3-linux-amd64.zip -d "$install_dir"
export PATH="$install_dir:$PATH"
dscpm --version
```

Persist the `PATH` addition in your shell configuration as needed. Changing APT
configuration still requires sufficient filesystem permissions.

Branch, pull-request, and manually dispatched workflow runs upload ZIP workflow
artifacts using the source version, but never publish GitHub releases. Ordinary
CI runs do not require their commits to be on `main` and do not create drafts.
Local builds also retain the source version.

## Build and check

Run from `resources/package-management`:

```sh
gofmt -w ./cmd ./internal
go vet ./...
go test ./...
go test -race ./...
GOOS=linux GOARCH=amd64 go build -o dscpm ./cmd/dscpm
GOOS=linux GOARCH=arm64 go build -o dscpm ./cmd/dscpm
```

Repository tests use `NewStore(t.TempDir())` with a complete temporary filesystem layout;
they never access the host's APT configuration or use the network. Acceptance
fixtures reproduce
[`Bpoe/homelab`'s Docker playbook](https://github.com/Bpoe/homelab/blob/fd8bae42e117df78abcb5d6a76781b477e2622e4/ansible/playbooks/docker.yaml)
for amd64/trixie and the Debian 12 `packages-microsoft-prod.deb` configuration.
`testdata/docker.asc` is Docker's public repository key from
`https://download.docker.com/linux/debian/gpg`; `testdata/microsoft-prod.gpg`
is the public keyring extracted from
`https://packages.microsoft.com/config/debian/12/packages-microsoft-prod.deb`.
Both were captured on October 12, 2026. Other cases use synthetic public test
keys. The suite checks file bytes, identity, permissions, modification times,
and directory contents to detect unwanted writes after round trips or Set.

When `dsc` is on `PATH`, `go test ./...` also runs all three manifests'
`stateAndDiff` contracts and discovery/Get/Test/Set through the real DSC
executable. Integration tests redirect each manifest's commands to test-process
helpers: repository tests use a temporary APT root, while package tests use an
injected executor and fake installed-state files. They do not read or change the
host's repositories or installed packages. Local runs skip when DSC is absent
unless `DSCPM_REQUIRE_DSC=1` is set. CI installs checksum-verified DSC 3.3.0 and
sets this variable for both normal and race tests, so missing DSC fails rather
than silently skipping integration coverage.

Optional native package tests build minimal local archives with `dpkg-deb` and
exercise read-only Get/Test through real dpkg tools, including local metadata
resolution and missing-package detection. They skip when the required tools or
the installed `bash` fixture are unavailable; they never run native Set or APT
transactions and do not modify host packages.
