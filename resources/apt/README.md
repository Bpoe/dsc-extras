# dscapt: APT repository resource

`dscapt` implements `Get`, `Test`, and `Set` for the DSCv3 resource
`DscExtras.Apt/Repository`. It manages a logical binary-package (`deb`)
repository, not a particular filename. Existing configurations created by
Ansible, a Debian package, or DSC can already satisfy the desired state.

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

`Test` uses DSC's `stateAndDiff` contract: `dscapt test` emits exactly two
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

There is no package installation, `apt update`, key download, or remote
repository availability check. External binary keyrings require APT 1.4+,
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
Install `dscapt` and `dscapt.dsc.resource.json` together on `PATH`. Changing
system configuration requires sufficient filesystem permissions.

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
    type: DscExtras.Apt/Repository
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

## Build and check

Run from `resources/apt`:

```sh
gofmt -w ./cmd ./internal
go vet ./...
go test ./...
go test -race ./...
GOOS=linux GOARCH=amd64 go build ./cmd/dscapt
GOOS=linux GOARCH=arm64 go build ./cmd/dscapt
```

Tests use `NewStore(t.TempDir())` with a complete temporary filesystem layout;
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

When `dsc` is on `PATH`, `go test ./...` also runs the manifest's `stateAndDiff`
contract through the real DSC executable. The integration test redirects the
manifest's test command to a test-process helper using a temporary APT root;
it does not read or change the host's repositories. It skips when DSC is absent.
