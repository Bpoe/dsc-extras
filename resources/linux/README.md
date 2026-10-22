# Linux component (`dsclinux`)

`dsclinux` provides the DSC v3 resources `DscExtras.Linux/services` for managing
Linux systemd units and `DscExtras.Linux/groups` for managing local groups.
Services, timers, sockets, mounts, targets, and other systemd
unit types are supported. Names without a recognized unit-type suffix resolve to
`.service`.

## Installation

Download the appropriate Linux AMD64 or ARM64 ZIP from a `dsclinux-vX.Y.Z`
release. Extract `dsclinux`, `services.dsc.resource.json`,
`groups.dsc.resource.json`, and `LICENSE` into
the same directory. Make the executable runnable and add that directory to
`PATH` and, if necessary, `DSC_RESOURCE_PATH`:

```bash
chmod +x "$HOME/.local/bin/dsclinux"
export PATH="$HOME/.local/bin:$PATH"
export DSC_RESOURCE_PATH="$HOME/.local/bin"
dsc resource list DscExtras.Linux/services
dsc resource list DscExtras.Linux/groups
dsclinux --version
```

Services requires Linux with a running systemd manager and `systemctl` on
`PATH`. Groups requires Linux account-management tools (`groupadd`, `groupmod`,
`groupdel`, `gpasswd`) and `getent` on `PATH`, but does not require systemd.
Group mutations normally require root; system-scope service mutations require
root or noninteractive authorization.
`dsclinux` does not invoke sudo, prompt for authentication, or switch users.

Build independently with Go 1.25 or later:

```bash
cd resources/linux
go build -o dsclinux ./cmd/dsclinux
```

Install both resource manifests alongside the built executable.

## Services properties

| Property | Type | Default / behavior |
| --- | --- | --- |
| `name` | string, required key | Literal unit name, for example `docker`, `backup.timer`, `docker.socket`, or `worker@one.service`; paths and wildcards are not accepted |
| `state` | `active` or `inactive` | Unmanaged when omitted; matches systemd's `ActiveState` exactly |
| `enabled` | boolean | Unmanaged when omitted; persistent enablement, independent of runtime state and masking |
| `masked` | boolean | Unmanaged when omitted; persistent activation mask, independent of runtime state and enablement |
| `scope` | `system` or `user` | `system`; `user` selects the current user's manager |
| `activeState` | read-only string | Raw systemd activation state, including `failed`, `activating`, and `deactivating` |
| `subState` | read-only string | Raw unit-type-specific runtime state, such as `running`, `exited`, or `listening` |
| `loadState` | read-only string | Raw load state, including `loaded`, `masked`, and `not-found` |
| `unitFileState` | read-only string | Raw combined installation status, such as `enabled`, `disabled`, `static`, or `masked` |

Unknown JSON properties are ignored, including through the manifest schema.
Known desired properties are validated. Read-only diagnostics are not compared
by Test or used as desired state by Set.

For actual activation states other than `active` and `inactive`, Get/Test omit
`state` and return the exact value in `activeState`. This preserves diagnostics
without returning a value outside the desired-state schema. A failed service is
**not** considered compliant with `state: inactive`. Set stops it and, only if
systemd still reports `failed`, clears the failure with `reset-failed` to reach
the explicitly requested `inactive` state. A oneshot service with
`RemainAfterExit=yes`, an active timer, and a listening socket can all be active
without having a running service process.

## Runtime state, enablement, and masking

These are three independent properties:

- `state` starts or stops the unit; it does not enable, disable, or persistently
  change masking.
- `enabled` manages persistent boot/dependency enablement; it does not start or
  stop the unit.
- `masked` manages a persistent activation prohibition; it does not disable or
  stop the unit.

**A unit can be both enabled and masked.** Enablement is represented by
dependency or alias symlinks; a mask is a separate unit-file symlink to
`/dev/null` (an empty overriding file is also a systemd mask). `systemctl
is-enabled` and `UnitFileState` hide enablement behind the combined `masked`
status. This resource instead inspects persistent `.wants`, `.requires`,
`.upholds`, and alias symlinks without resolving enablement links through the
mask.

System configuration is inspected under `/etc/systemd/system`. User scope
includes `$XDG_CONFIG_HOME/systemd/user` (default `~/.config/systemd/user`),
`$XDG_CONFIG_DIRS/systemd/user` (default `/etc/xdg/systemd/user`), and
`/etc/systemd/user`, respecting local unit-definition precedence over global
masks. Runtime enablement and masks under `/run` are not persistent state.

Get and Test never unmask or otherwise modify configuration. Set skips
compliant mutations, enables before applying a requested mask, and verifies
the resulting state. A masked unit may need temporary unmasking to change
enablement or become active. Its mask is restored if masking is requested
or unmanaged, including after a failed transition; restoration failures are
reported. Enablement links pruned by `systemctl unmask` are restored exactly
when enablement is unmanaged or desired enabled, without overwriting existing
configuration. A daemon reload occurs only for a necessary transition with a
stale/new definition, or when restoring pruned links requires it; systemctl's
own implicit reloads are retained.

Static units can be active without being enabled. Requesting enablement of a
unit without suitable install rules fails verification rather than claiming
success. Missing units are reported as `loadState: not-found`; Set cannot create
unit definitions. A missing inactive unit can already satisfy requested
inactive/disabled/unmasked state. Conflicting local definitions, permission
failures, and global user configuration that cannot be changed by the current
user are reported, not overwritten with `--force`. Runtime masks are left
alone; remove a conflicting transient mask explicitly before requesting a
transition that it prevents. Restart and reload are actions, not supported
declarative states.

A template mask inherited by an instance is observed as masked. Removing that
shared mask must be done explicitly using the defining template name, rather
than implicitly changing every instance while reconciling just one.

Templates such as `worker@.service` support persistent enablement and masking.
Their enablement includes persistent links for concrete instances such as
`worker@default.service`; enabling a disabled template lets systemd choose the
instance through `DefaultInstance=`. A non-default enabled instance also counts
as persistent enablement, even when the raw `unitFileState` is `indirect`.
Templates themselves have no runtime unit object, so their observations omit
`state`, `activeState`, and `subState`. Specify a concrete instance name when
managing runtime activation.

## Services examples

Manage all three independent properties:

```yaml
resources:
  - name: Docker
    type: DscExtras.Linux/services
    properties:
      name: docker
      state: active
      enabled: true
      masked: false
```

Enable but prohibit activation, without changing the current runtime state:

```yaml
resources:
  - name: Hold backup timer
    type: DscExtras.Linux/services
    properties:
      name: backup.timer
      enabled: true
      masked: true
```

Disable persistent enablement without stopping a socket:

```yaml
resources:
  - name: Disable socket at boot
    type: DscExtras.Linux/services
    properties:
      name: docker.socket
      enabled: false
```

User scope operates only on the calling user's systemd manager:

```yaml
resources:
  - name: User backup timer
    type: DscExtras.Linux/services
    properties:
      name: backup.timer
      scope: user
      state: active
```

Run user-scope DSC from the intended user's login session, with a running user
manager and valid `XDG_RUNTIME_DIR` / `DBUS_SESSION_BUS_ADDRESS`. There is no
implicit user switching or automatic creation of a user manager.

The executable accepts one JSON object on stdin:

```bash
printf '%s\n' '{"name":"docker"}' | dsclinux services get
printf '%s\n' '{"name":"docker","state":"active"}' | dsclinux services test
printf '%s\n' '{"name":"docker","enabled":false}' | dsclinux services set
dsclinux --help
```

Get and Set emit one observed-state JSON object. Test emits the observed state
with `_inDesiredState`, followed by a second JSON value containing the array of
differing desired properties (`state`, `enabled`, `masked`). An empty difference
array is `[]`, never `null`. Errors are JSON diagnostics on stderr with a
nonzero exit status. The manifest declares `stateAndDiff` and
`implementsPretest`.

## Groups properties

| Property | Type | Default / behavior |
| --- | --- | --- |
| `name` | string, required key | Literal local Linux group name |
| `ensure` | `Present` or `Absent` | `Present`; creates/adopts a local group or deletes it |
| `gid` | integer, 0 through 4294967294 | Unmanaged when omitted; Linux allocates the GID on creation |
| `members` | string array | Exact supplementary membership; unmanaged when omitted |
| `includedMembers` | string array | Required supplementary members; preserves other members |
| `excludedMembers` | string array | Users required not to belong, including primary membership |

Membership arrays are unordered sets; duplicate desired names are invalid.
`members` cannot be combined with either selective property, even if the
arrays are empty. `includedMembers` and `excludedMembers` can be combined but
cannot overlap. Unknown JSON properties are ignored. Omit unmanaged optional
properties rather than passing `null`.

### Primary and supplementary membership

A user's **primary group** is identified by the GID in the user's account
record. **Supplementary membership** is the additional list recorded in
`/etc/group`. Exact `members` manages only that supplementary list, not primary
group assignments. A primary-group user need not appear in an exact
supplementary list. Selective `excludedMembers` means the user must not belong
in either way: if the target is the user's primary group, Test reports
`excludedMembers` as differing and Set fails with an actionable error. Reassign
that primary group explicitly outside this resource.

The resource never explicitly reassigns primary groups. However, native
`groupmod -g` may update the numeric primary GIDs of local users assigned to the
group. This native behavior is preserved, not reversed or supplemented with
separate user management. Changing the group's number is not a workaround for
primary-group exclusion. **Filesystem ownership is not migrated** when the GID
changes. Membership changes apply to newly established process credentials and
login sessions; already-running sessions are not updated.

### Exact, selective, and unmanaged membership

Supplying `members` owns the complete supplementary set: add missing users and
remove extras. `members: []` removes every supplementary member. Omitting all
membership properties leaves membership untouched.

Selective constraints add missing `includedMembers` and remove present
`excludedMembers`, leaving unrelated members unchanged. Empty selective arrays
impose no membership constraints; they do not clear the group. Requested exact
or included users must already exist; no users are created. A nonexistent
excluded user already satisfies exclusion unless a stale supplementary entry
still needs removal.

### Existence, GID, and local-only management

Existing local groups are adopted without rewriting compliant state. Omitted
GID leaves the existing number unchanged; new groups let Linux allocate one.
Explicit GIDs are changed only when different. Duplicate GIDs are not enabled
through non-unique native options. There is no `system` property: system-group
allocation is not a separate persisted group attribute.

`ensure: Absent` deletes only the local group, never user accounts. It cannot
be combined with GID or membership constraints. Already-absent groups are
compliant. Native deletion is allowed to fail if the group is someone's primary
group; the resource does not force deletion or reassign users.

`/etc/group` is authoritative for local existence and supplementary membership.
Split entries created with `MAX_MEMBERS_PER_GROUP` are combined when their name
and GID agree, with sorted, deduplicated observed membership. Same-name entries
with different GIDs are errors. Desired arrays still reject duplicates.

`getent` performs NSS-aware identity lookups, including LDAP/NIS-backed users.
An external-only group is locally Absent: Get reports Absent, Test compares
local state, Set Present fails as externally managed, and Set Absent does
nothing. The resource does not manage LDAP/NIS groups or modify their backends.
`getent` exit 2 means the requested key was not found; exits 1 and 3 and other
execution failures are errors, not absence. Malformed successful responses also
fail. Some NSS backend outages can appear as missing identities; there is no
promise of detecting every backend outage or automatic retry/health mechanism.

All modifications use native account-management tools with direct process
execution, not shell interpolation or direct rewrites of `/etc/group`,
`/etc/gshadow`, or `/etc/passwd`. Get and Test are read-only. Set makes only
necessary changes, orders membership differences deterministically, and
verifies the result. Failures return nonzero with JSON diagnostics on stderr.
Completed changes are not rolled back; a subsequent Set can continue
reconciliation.

### Groups examples

Create or adopt a group (`ensure: Present` can be omitted):

```yaml
$schema: https://aka.ms/dsc/schemas/v3/bundled/config/document.json
resources:
  - name: Developers group
    type: DscExtras.Linux/groups
    properties:
      name: developers
      ensure: Present
```

Create or update a specific GID, leaving supplementary membership unmanaged:

```yaml
$schema: https://aka.ms/dsc/schemas/v3/bundled/config/document.json
resources:
  - name: Developers GID
    type: DscExtras.Linux/groups
    properties:
      name: developers
      gid: 2000
```

Add existing users to Docker without removing any existing members:

```yaml
$schema: https://aka.ms/dsc/schemas/v3/bundled/config/document.json
resources:
  - name: Docker access
    type: DscExtras.Linux/groups
    properties:
      name: docker
      includedMembers:
        - brandon
        - deploy
```

Require and exclude users while preserving unrelated members:

```yaml
$schema: https://aka.ms/dsc/schemas/v3/bundled/config/document.json
resources:
  - name: Selective Docker access
    type: DscExtras.Linux/groups
    properties:
      name: docker
      includedMembers: [brandon, deploy]
      excludedMembers: [olduser]
```

Own the exact supplementary set:

```yaml
$schema: https://aka.ms/dsc/schemas/v3/bundled/config/document.json
resources:
  - name: Exact developers membership
    type: DscExtras.Linux/groups
    properties:
      name: developers
      members: [alice, bob]
```

Remove all supplementary members, without reassigning any primary groups:

```yaml
$schema: https://aka.ms/dsc/schemas/v3/bundled/config/document.json
resources:
  - name: Empty developers membership
    type: DscExtras.Linux/groups
    properties:
      name: developers
      members: []
```

Delete a group without deleting its users:

```yaml
$schema: https://aka.ms/dsc/schemas/v3/bundled/config/document.json
resources:
  - name: Remove developers group
    type: DscExtras.Linux/groups
    properties:
      name: developers
      ensure: Absent
```

Direct CLI usage:

```bash
printf '%s\n' '{"name":"docker"}' | dsclinux groups get
printf '%s\n' '{"name":"docker","includedMembers":["brandon"]}' | dsclinux groups test
printf '%s\n' '{"name":"docker","includedMembers":["brandon"]}' | dsclinux groups set
```

Get/Set emit the actual name, `ensure`, numeric `gid`, and complete sorted
`members` for an existing group. Absent groups omit GID and membership. Get does
not echo selective constraints. Test uses the same two-value `stateAndDiff`
contract as services, with `_inDesiredState` on actual state and a second array
of differing managed property names: `ensure`, `gid`, `members`,
`includedMembers`, or `excludedMembers`.

## Tests and releases

```bash
gofmt -l ./cmd ./internal
go vet ./...
go test ./...
go test -race ./...
DSCLINUX_REQUIRE_DSC=1 go test ./cmd/dsclinux
DSCLINUX_SYSTEMD_CONTAINER=1 go test -count=1 -timeout=6m ./internal/services -run '^TestSystemdContainer$'
DSCLINUX_GROUPS_CONTAINER=1 go test -count=1 -timeout=6m ./internal/groups -run '^TestGroupsContainer$'
```

Linux unit tests use isolated temporary configuration directories and a fake
systemctl executor; group tests use isolated account fixtures and fake native
commands/NSS lookups. They require neither root nor a running systemd instance.
DSC CLI integration tests cover both resources using fake managers in child
test processes and skip
when DSC is unavailable unless `DSCLINUX_REQUIRE_DSC=1`.

The opt-in real-systemd test needs Docker supporting privileged containers and
private cgroup namespaces. It builds a disposable Debian systemd image and
boots a separate manager with disposable system/user units. It never mounts
host unit directories or connects to the host bus, and removes its container
and image afterward. CI runs it separately from unit/race tests.

The opt-in groups test requires Docker but not privileged mode or systemd.
It creates temporary accounts only inside a disposable Debian container, checks
native GID/primary-GID and membership behavior, confirms ownership is not
migrated, and removes the container and image afterward. It never mounts host
account databases. Both container suites are mandatory CI packaging gates.

`.github/workflows/dsclinux.yml` validates this component independently, builds
Linux AMD64/ARM64 ZIP archives containing the executable, both manifests, and license,
and creates **draft** releases for stable `dsclinux-vX.Y.Z` tags reachable from
`main`. The tag supplies the executable/archive/manifest version; untagged
builds use the manifests' shared initial version `0.1.0`. The `dscpm` pipeline is
unchanged.

Systemd semantics: [systemctl](https://www.freedesktop.org/software/systemd/man/latest/systemctl.html)
and [systemd.unit](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html).
Account semantics: [groupmod](https://man7.org/linux/man-pages/man8/groupmod.8.html),
[group](https://man7.org/linux/man-pages/man5/group.5.html), and
[getent](https://man7.org/linux/man-pages/man1/getent.1.html).
