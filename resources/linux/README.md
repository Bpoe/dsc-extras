# Linux component (`dsclinux`)

`dsclinux` provides the DSC v3 resource `DscExtras.Linux/services` for managing
Linux systemd units. Services, timers, sockets, mounts, targets, and other systemd
unit types are supported. Names without a recognized unit-type suffix resolve to
`.service`. Only `services` is implemented.

## Installation

Download the appropriate Linux AMD64 or ARM64 ZIP from a `dsclinux-vX.Y.Z`
release. Extract `dsclinux`, `services.dsc.resource.json`, and `LICENSE` into
the same directory. Make the executable runnable and add that directory to
`PATH` and, if necessary, `DSC_RESOURCE_PATH`:

```bash
chmod +x "$HOME/.local/bin/dsclinux"
export PATH="$HOME/.local/bin:$PATH"
export DSC_RESOURCE_PATH="$HOME/.local/bin"
dsc resource list DscExtras.Linux/services
dsclinux --version
```

Linux with a running systemd manager and `systemctl` on `PATH` is required.
System-scope mutations normally require root or noninteractive authorization.
`dsclinux` does not invoke sudo, prompt for authentication, or switch users.

Build independently with Go 1.25 or later:

```bash
cd resources/linux
go build -o dsclinux ./cmd/dsclinux
```

Install the resource manifest alongside the built executable.

## Properties

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

## Examples

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

## Tests and releases

```bash
gofmt -l ./cmd ./internal
go vet ./...
go test ./...
go test -race ./...
DSCLINUX_REQUIRE_DSC=1 go test ./cmd/dsclinux
DSCLINUX_SYSTEMD_CONTAINER=1 go test -count=1 -timeout=6m ./internal/services -run '^TestSystemdContainer$'
```

Linux unit tests use isolated temporary configuration directories and a fake
systemctl executor; they require neither root nor a running systemd instance.
DSC CLI integration tests use a fake manager in a child test process and skip
when DSC is unavailable unless `DSCLINUX_REQUIRE_DSC=1`.

The opt-in real-systemd test needs Docker supporting privileged containers and
private cgroup namespaces. It builds a disposable Debian systemd image and
boots a separate manager with disposable system/user units. It never mounts
host unit directories or connects to the host bus, and removes its container
and image afterward. CI runs it separately from unit/race tests.

`.github/workflows/dsclinux.yml` validates this component independently, builds
Linux AMD64/ARM64 ZIP archives containing the executable, manifest, and license,
and creates **draft** releases for stable `dsclinux-vX.Y.Z` tags reachable from
`main`. The tag supplies the executable/archive/manifest version; untagged
builds use the manifest's initial version `0.1.0`. The `dscpm` pipeline is
unchanged.

Systemd semantics: [systemctl](https://www.freedesktop.org/software/systemd/man/latest/systemctl.html)
and [systemd.unit](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html).
