# dscpm: Package management component

`dscpm` is the package management component in `resources/package-management`,
with its executable entry point in `cmd/dscpm`. It implements `Get`, `Test`,
`Set`, and native DSC v3 Export for the resources
`DscExtras.PackageManagement/aptRepositories` and
`DscExtras.PackageManagement/packages`. The repository resource manages a logical binary-package (`deb`)
repository. A declaration adopts and reconciles its named source file,
including configurations created by Ansible, a Debian package, or DSC.

Resource names use a PascalCase namespace (`DscExtras.PackageManagement`) and
plural camelCase type (`aptRepositories`, `packages`). Package management selects
APT/dpkg on Debian/Ubuntu and DNF/RPM on RHEL, Rocky Linux, and AlmaLinux 8, 9,
and 10 using `/etc/os-release`, not the presence of commands on `PATH`.
Unknown distributions and unvalidated derivatives return an actionable error;
`ID_LIKE` alone does not opt a distribution into support.
DNF repository management and DNF package removal are deferred.

The repository implementation retains the structural rename from `dscapt` in
`resources/apt`.
The manifest
`manifests/aptRepositories.dsc.resource.json` exposes
`DscExtras.PackageManagement/aptRepositories`. The compatibility manifest
`manifests/dscapt.dsc.resource.json` preserves `DscExtras.Apt/Repository` for
existing DSC configurations. Both invoke the same `dscpm aptRepositories` handlers, including Export. There is no `dscapt` binary shim: old direct CLI invocations and
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
dscpm aptRepositories export
dscpm packages export
```

### Export

DSC assembles resource instances emitted as JSON Lines by the read-only Export
commands:

```bash
dsc resource export --resource DscExtras.PackageManagement/aptRepositories
dsc resource export --resource DscExtras.PackageManagement/packages
```

Each exported instance includes `_name` as an RFC 3986 percent-encoded resource label.
Repositories and APT packages use their existing `name` property (including any
architecture qualifier). RPM packages use the compound `name,version`, so
multiple installed versions of an install-only package have distinct top-level
resource names.
Encoding leaves only ASCII letters, digits, and `-._~` unchanged; spaces become `%20`,
literal `+` becomes `%2B`, and other bytes use `%HH` sequences.
The original `name` and `version` properties are unchanged.
For example, `libstdc++6:arm64` becomes `libstdc%2B%2B6%3Aarm64` in `_name`.
RPM instances of `kernel:x86_64` at versions `1:1.0-1` and `1:2.0-1` become
`kernel%3Ax86_64%2C1%3A1.0-1` and `kernel%3Ax86_64%2C1%3A2.0-1`, respectively.

The legacy `DscExtras.Apt/Repository` manifest uses the same repository
enumerator. DSC's `--input`/`--file` filters use resource properties:
repository filters supply `name`, `uri`, and `suite`, matching the normalized URI
and exact suite. `name` is a management label, not an identity constraint, so a
normal label such as `docker` works regardless of the source filename or
generated Export label. Package filters
match one or more package names, optionally architecture-qualified.

APT repositories are discovered from `sources.list`, `.list`, and `.sources`
files, then combined by normalized URI and suite. Export labels are stable
`repo-<hash>` names because filenames are not repository identity. Deb822
multi-value fields are expanded according to their Types/URIs/Suites
combinations. Ambiguous definitions, unsupported options, or malformed source
files fail Export rather than producing guessed state. Because this resource
requires an explicit `signingKey` to reconcile a present repository, definitions
using global trust or an unreadable/missing keyring are reported as
unrepresentable rather than exported without their trust semantics. Readable
explicit signing keys are exported.

Packages are enumerated from the local dpkg or RPM database without refreshing
metadata or contacting repositories. Each installed package is a separate
resource instance with its exact observed version; architecture is included
where needed to preserve package identity. Like Get, each exported instance
includes `ensure: Present`, the observed aggregate `state: Present`, and
`packages` containing its installed status, version, architecture, and requested
identity. These observations come from the same batched local inventory, without
per-package manager invocations. APT Get and Export share dpkg record parsing:
installation requires an `ok installed` status regardless of the requested
selection (`install`, `hold`, `deinstall`, or `purge`). Pending-removal packages
are still exported while installed; residual configuration and partial
installations are not. The complete original dpkg status is retained, and
malformed records fail rather than being silently skipped.
Export uses the same resource instance schema
as Get, Test, and Set, and requires no input. Exported resources can be placed
directly in a DSC configuration or piped through DSC's `resource export`
output options.

Install `dscpm`, `aptRepositories.dsc.resource.json`,
`dscapt.dsc.resource.json`, and `packages.dsc.resource.json` together in a directory on `PATH`, with all manifests
beside the executable. Changing system configuration requires sufficient
filesystem permissions.

## Repository identity and state

Every operation requires `name`, `uri`, and `suite`. The resource first selects
`/etc/apt/sources.list.d/<name>.sources`, or `<name>.list` if the Deb822 file
does not exist. A single binary repository in that file is reconciled in place,
even when its URL or suite differs. For files containing multiple repositories,
URI and suite select the entry to manage; ambiguous edits are rejected.
If the named file does not identify the repository, discovery by URI and suite
preserves existing configurations in other files instead of creating duplicates.
HTTP, HTTPS, local `file` and `copy`, legacy `mirror`, and `mirror+http`,
`mirror+https`, `mirror+file`, and `mirror+copy` URIs are supported. Credentials
and fragments are rejected. Original URIs are preserved without resolving mirror
lists or cleaning paths. A trailing URI slash is equivalent for directory
repositories, except in URIs containing query strings. Mirror-list URIs identify
files and are compared exactly, including any trailing slash.

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

`Test` and `Get` are read-only and never refresh APT metadata. A compliant `Set`
returns before any write or directory creation, including for package-owned
sources and shared keyrings.
Safe changes retain an existing source's format and location, changing only
affected lines or fields. A changed member of a multi-value Deb822 stanza is
split into individual stanzas with the other repositories' properties retained.
Unchanged documents and keyrings are never serialized. A malformed named source
is noncompliant and can be replaced by `Set`; `Get` reports a parsing error when
its actual properties cannot be read. Malformed unrelated sources still produce
errors and are not replaced. A malformed named source that overlaps a matching
repository elsewhere is rejected before any writes rather than creating a
duplicate definition.

A missing repository creates:

- `/etc/apt/sources.list.d/<name>.sources`, marked as managed by DSC.
- `/etc/apt/keyrings/<name>.gpg`, containing the supplied public key.

New source files retain the legacy `# Managed by dscapt: <name>` comment.
Neither this marker nor any other ownership metadata is required for adoption
or reconciliation.

An equivalent existing keyring at the declared key path is reused unchanged.
Incorrect or malformed key material at `/etc/apt/keyrings/<name>.gpg` is replaced,
regardless of who created it. A different external signing-key reference is
redirected to that declared key path rather than overwriting an unrelated key.
Embedded signing material is reconciled in place. Keys can be rotated only
when the source is editable and no other source
(including disabled sources, `deb-src`, and filesystem aliases) references
the key. Removing a repository intentionally retains its keyrings.

The declaration establishes ownership of the selected source and declared key
path; no dpkg ownership lookup, force flag, or adoption step is needed.
The resource rejects unsafe names and paths and refuses writes through symlinks,
stale discovery snapshots, duplicate/conflicting definitions, and ambiguous edits
to repositories distributed across multiple entries. Consistent entries with
disjoint components can collectively satisfy a desired repository without edits.
Errors identify genuine filesystem problems or the source ambiguity that must
be resolved explicitly. Atomic replacement preserves existing file permissions.

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

### Correcting or migrating an existing repository

Keep `name` unchanged when correcting a URL, suite, components, architecture
restriction, or signing key. For example, changing Docker's URL from
`https://download.docker.com/linux/debian` to
`https://download.docker.com/linux/ubuntu` repairs the existing `docker.sources`
on the next `set`; no manual deletion is required. A suite change likewise
updates that file, and subsequent runs are no-ops.

Changing `name` may select a different filename. Previously named or unrelated
files are not automatically deleted; use `ensure: Absent` to remove an old
definition explicitly when needed. Related `deb-src` entries and signing
keyrings are retained.

## DNF/RPM package installation

The same `DscExtras.PackageManagement/packages` resource and Linux AMD64/ARM64
binary support `ensure: Present` on RHEL/Rocky/AlmaLinux 8, 9, and 10.
`rpm` and the platform's `dnf` must be on `PATH`. Necessary installations require
the DNF 4 CLI; unrecognized versions (including DNF 5) fail explicitly rather
than guessing transaction formats or exact-version behavior. Get and Test only
read RPM metadata and never invoke DNF or refresh repository metadata.

Use a scalar name or an unordered, nonempty array. Repository names are
case-sensitive and may contain periods, underscores, hyphens and plus signs.
Use `name:architecture`, such as `glibc:x86_64` or `tzdata:noarch`, to select
exactly that architecture. Periods in names remain literal; use the colon
qualifier rather than DNF's ambiguous `name.arch` notation.
Unqualified names accept only the native RPM architecture or `noarch`, never
an arbitrary installed multilib variant. Explicit architectures and local RPMs
must appear in RPM's native compatible-architecture list. Availability still
depends on the distribution's enabled repositories (for example, newer releases
may not publish a multilib package even when RPM recognizes its architecture).

```yaml
resources:
  - name: Development tools
    type: DscExtras.PackageManagement/packages
    properties:
      name: [git, curl, vim]
      ensure: Present
  - name: Web server
    type: DscExtras.PackageManagement/packages
    properties:
      name: nginx
  - name: Application and prerequisites
    type: DscExtras.PackageManagement/packages
    properties:
      name:
        - jq
        - /opt/packages/myapp-1.2.3-1.x86_64.rpm
      updateCache: true
```

Local RPM identity comes from `rpm -qp`, **not its filename**: name, epoch,
version, release and architecture must match installed metadata. Files must be
regular, valid RPMs and remain accessible on every reconciliation, even after
installation. Relative paths resolve against the resource process's working
directory. Changing only archive bytes, without changing identity or EVR, does
not trigger a reinstall. Overlapping or conflicting identities in a batch are
rejected before installation.

The observed `packages` array retains `requestedName`, `name`, `version`,
`architecture`, `status`, and `installed`. RPM versions are reported as
**`epoch:version-release`**, always including epoch (`0` when absent).
Status is `installed` or `not-installed`; aggregate `state` is `Present`,
`Absent`, or `Mixed`, independent of the requested `ensure`.
Multiple installed versions (such as install-only kernels) are supported: any
acceptable installed instance satisfies a request. RPM database presence is
the installed-state authority; this does not verify every installed payload
file or infer scriptlet success beyond native transaction exit status.

Without `version`, any installed version is compliant and Set does not upgrade
it. A scalar repository name may specify exact **`[epoch:]version-release`**;
release is required and omitted epoch means zero. For example:

```json
{"name":"example:x86_64","version":"1:2.3.4-5.el9","ensure":"Present"}
```

This illustrative version must actually exist in the configured repositories.
Version is not valid with an array or a local artifact. Exact requests may
upgrade; downgrades require `allowDowngrade: true`. Ordering compares epoch,
version and release separately using RPM's built-in Lua `rpm.vercmp`, with no
Python bindings, RPM SDK or rpmdevtools dependency. Native RPM semantics, not
lexical or Debian comparisons, determine equality and ordering.

Set sends only noncompliant requests to one native DNF install transaction,
including mixed repository/local batches. DNF resolves and installs dependencies.
An entirely compliant batch is a strict no-op, including when `updateCache` is
true. For a necessary transaction, `updateCache: true` adds `--refresh` to its
first preflight; repository failures are propagated, not silently skipped.
There are no timestamps, background refreshes or separate periodic refreshes.
DNF may still perform its normal configured metadata refresh.

### DNF transaction safety and limitations

Before mutation, DNF's C-locale `--assumeno` transaction table must describe
exactly the requested identities/versions, plus dependencies. Unknown output,
ambiguous identities, erasures, replacements, reinstalls and unauthorized
downgrades fail closed. Repository targets are pinned to the resolved full
NEVRAs and the preflight is repeated. The plan, installed inventory and local
RPM metadata must remain unchanged before execution. This also prevents a
capability/provides match or name prefix from being accepted as the request.
If dependencies would change an already-compliant member of the requested
batch, Set refuses that transaction rather than silently upgrading it.

The executing DNF command additionally requests native `protected_packages`
protection for **every initially installed package name**. This is defense in
depth, not a substitute for preflight: some libdnf versions can still propose an
obsoleting provider despite that protection. Both plans must therefore explicitly
reject every `replacing` row as well as removal actions. The temporary protection
list is removed after the operation. Commands also set `obsoletes=False` and
disable install-only pruning; old kernels are not automatically deleted to
enforce an install-only limit. The obsoletes option alone does not prevent an
explicitly requested RPM from replacing another installed package.
No `--allowerasing`, `--skip-broken`, `--nogpgcheck`, direct RPM installs, deleted
lock files, or dependency/signature bypasses are used. Host repository and local
package signature policy is retained, including enabled subscription plugins.
Post-transaction checks require the requested state and retention of every
previously installed name/architecture.

Commands use direct argument arrays, a stable locale and bounded contexts
(30 seconds for RPM, 15 minutes per DNF command). Set requires root/equivalent
permissions, writable DNF/RPM state and a writable temporary directory.
Unavailable packages/versions, disabled or unreachable repositories, signature
failures, dependency conflicts and lock failures produce errors with native
diagnostics and exit status. Repository configuration is not created or repaired.

**Concurrency and rollback:** preflights are not an atomic reservation of a
transaction. Native DNF/RPM locking remains in use, but native protection alone
does not guarantee that a newly resolved transaction cannot replace a package.
External inventory, metadata, configuration or artifact changes after the final
checks can invalidate the plan. Serialize other package/repository maintenance
with `dscd`. Package scripts, cancellation and failed transactions
can leave changes behind; this resource does not promise rollback. Inspect
native package-manager state before retrying concurrently.

`ensure: Absent` is explicitly unsupported for all DNF Get/Test/Set operations,
even if the package is missing; no removal is attempted. Safe removal and
`dnfRepositories` will be implemented separately. APT installation, removal,
automatic-dependency cleanup, and APT repository management remain unchanged.

### Native DNF integration tests

Run `DSCPM_DNF_CONTAINER=1 go test -v -count=1 -timeout=20m ./internal/dnf -run
'^TestDNFContainer$'` from this module (set the variable using your shell's
syntax). Override `DSCPM_DNF_IMAGE` for a specific Rocky image. CI gates release
builds on Rocky 8, 9 and 10 containers from
`quay.io/rockylinux/rockylinux`, alongside the existing APT and Go/DSC tests.
All fixture builds and package mutations occur in disposable, unmounted
containers. Native tests cover configured repositories, exact versions, mixed
batches, local RPM metadata, upgrades/downgrades, idempotence, Obsoletes rejection,
unsupported removal and the shipped manifest using SHA-256-pinned DSC 3.3.0.
The default Docker Library `rockylinux:10` tag is not available; use the official
Quay namespace for that release. Native runs exercised DNF 4.7/RPM 4.14 on
Rocky 8, DNF 4.14/RPM 4.16 on Rocky 9, and DNF 4.20/RPM 4.19 on Rocky 10.
All three expose the required embedded RPM comparison and DNF 4 transaction
format; Rocky 10 still ships DNF 4, not DNF 5.
Rocky 10's native solver proposed a `replacing` row for a fixture with both
`Obsoletes` and `Provides` despite native protection; the resource's preflight
rejected it before mutation. The integration test accepts either a native
protected-package rejection or this explicit preflight rejection, and requires
unchanged RPM inventory and DNF history in both cases.
RHEL and AlmaLinux compatibility follows their
DNF 4/RPM interfaces; subscription-enabled RHEL images are not exercised in CI.

## APT packages

`DscExtras.PackageManagement/packages` manages installed packages, not repository
definitions. It requires APT 2.2+ (Debian 11+/Ubuntu 22.04+) and `apt-get`, `dpkg`,
`dpkg-query`, and `dpkg-deb` on `PATH`. Its fail-closed cache refresh
(`APT::Update::Error-Mode=any`) and disabled legacy regex fallback
(`APT::Cmd::Pattern-Only=true`) rely on current APT behavior.

Use `name` as one package name or a nonempty array. An entry can
also be a local `.deb` path; arrays can mix repository names and local files.
Local paths may be absolute or relative to the resource process's working
directory and must identify existing regular files for `Get` and `Set` (see
[Local archives that do not exist yet](#local-archives-that-do-not-exist-yet)
for `Test`). Arrays are unordered sets
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
available on every Get/Set reconciliation, even after installation (a missing
file makes `Test` report noncompliance).
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

### Local archives that do not exist yet

A local `.deb` can be produced by another DSC resource (for example, a download)
that this resource `dependsOn`. On a fresh machine, `dsc config test` runs before
that archive exists, so `Test` treats a referenced archive that does not exist
as **not in the desired state** rather than as an error: it exits successfully
with `_inDesiredState: false` and an `ensure` difference. This applies to a
single local `.deb` and to arrays mixing repository packages, existing archives,
and missing archives; other entries are evaluated normally. `Test` never
downloads, creates, or installs the archive.

Because the archive's package identity cannot be determined, its `packages[]`
entry contains only `requestedName` (with empty `name` and `status` and
`installed: false`, meaning not confirmed installed), and no version or
architecture. The aggregate `state` is omitted unless the other observed
packages are already `Mixed`. Duplicate-identity checks cannot include the
missing archive.

Only a file-not-found condition is treated this way. Permission errors, a
directory or other non-regular file, unreadable or invalid `.deb` metadata, and
other failures remain errors. `Get` and `Set` still fail for a missing archive,
because its identity cannot be reported and installation cannot proceed; `Set`
never silently skips it.

### Safe removal and automatic dependency cleanup

`ensure: Absent` removes only the noncompliant, explicitly requested package
identities, preserving architecture qualifiers. Removal is safe by default:
before any mutation, a plain `apt-get --simulate remove` must propose **only**
those identities. If removing `libexample` would also remove `nginx` because
`nginx` depends on it, reconciliation fails with the unexpected package names
and asks you to remove the dependent packages explicitly or change the
configuration. This protects reverse-dependent packages whether they are marked
manually **or automatically** installed. There is no force flag, dependency-removal
override, or automatic expansion of the explicit request.

This differs from cleaning up orphaned dependencies: removing `nginx` may leave
its automatically installed `libfoo` and `libbar` unused. A second simulation
uses APT's native `--auto-remove` dependency solver. Additional removals are
allowed only when `apt-mark showauto` identifies them as automatically installed;
the plain removal simulation must not have required them as reverse-dependent
removals. A dependency still needed by another remaining package is retained by
APT. Manually installed packages are never automatically cleaned up; they must
be explicitly requested for removal.

Once validated, the resource executes one `apt-get --yes --auto-remove remove`
transaction, not a separate `autoremove` operation, and verifies that all
explicitly requested packages are absent afterward. This can also clean up
**unrelated, previously orphaned automatically installed packages**, not just
dependencies orphaned by this removal. An entirely absent request is a strict
no-op: no APT simulation, removal, or unrelated cleanup runs.
Removal never requests purge; residual configuration files are retained.

Planning parses actual `Remv` operations, checks identities against the dpkg
inventory and removal counts, and rejects unknown/ambiguous identities,
unexpected installs/upgrades/downgrades/configuration/purge operations,
malformed action records, failed commands, or unavailable automatic-package status.
Other informational output, including `Use 'sudo apt autoremove' to remove them.`,
is ignored rather than matched against a message allowlist. Notices do not
authorize removals; only action records and the existing cleanup safeguards do.
The removal summary must still agree with the parsed operations.
Both removal plans are repeated, and the complete dpkg inventory, automatic
marks, and effective APT configuration are rechecked before execution.
Detectable changes abort reconciliation without executing the removal.

**Concurrency limitation:** APT simulations do not acquire an exclusive APT
lock. These rechecks are not an atomic reservation of a plan: another package
manager, repository metadata change, or configuration writer can still change
state after a check and before the real APT solver acquires its locks.
Serialize external package/repository maintenance with dscd reconciliation;
do not rely on simulations for atomic protection against concurrent writers.
Package removal and dependency cleanup are not guaranteed transactional or
reversible: maintainer scripts or a failed APT transaction may leave partial
changes, and there is no rollback or configuration-file purge.

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
| `name` | Resolved base package name, including the name read from local metadata; architecture is separate. Empty for a missing local archive during `Test`. |
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

Opt in to real APT removal tests from `resources/package-management`:

```sh
DSCPM_APT_CONTAINER=1 go test -v -count=1 ./internal/apt -run '^TestPackagesContainerRemoval$'
```

`TestPackagesContainerRemoval` requires a working Docker or Podman runtime and
fails if explicitly enabled without one. Otherwise it skips by default. It
creates a disposable `debian:bookworm-slim` container with `--rm`, no host mounts,
and networking disabled (the runtime may first download the image). Synthetic
packages and all real `PackageManager.Set` transactions stay inside that
container, which is forcibly removed during test cleanup. Coverage includes
manual and automatic reverse-dependent rejection, orphan dependency cleanup,
shared/manual dependency retention, preserved conffiles, and idempotent absence.
The `apt-container` CI job enables this test; it never mutates host packages.
