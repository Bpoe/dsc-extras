# dscapt: APT repository resource

`dscapt` is a Go-based DSCv3 resource for managing one APT repository per
Deb822 source file. It implements `Get`, `Set`, and `Test` for
`DscExtras.Apt/Repository`. Each managed repository uses exactly two files:

- `/etc/apt/sources.list.d/<name>.sources`
- `/etc/apt/keyrings/<name>.asc`

`signingKey` continues to accept the complete inline ASCII-armored OpenPGP
public key. The resource writes that key to the `.asc` keyring file and points
the Deb822 `Signed-By` field at it. It does not download keys or read any
user-specified key path. On `ensure: Absent`, it removes the repository's
`.sources` file and removes its keyring only when no other `.sources`, `.list`,
or `/etc/apt/sources.list` entry references it.

## Build and check

Run these commands from `resources/apt`:

```sh
gofmt -w ./cmd ./internal
go vet ./...
go test ./...
go test -race ./...
GOOS=linux GOARCH=amd64 go build ./cmd/dscapt
GOOS=linux GOARCH=arm64 go build ./cmd/dscapt
```

The Go module uses `github.com/ProtonMail/go-crypto` to parse and verify
ASCII-armored public keys without invoking an external process. Reconciliation
writes the keyring and source as separate atomic file replacements rather than
as one transaction; an error is returned if either replacement fails, and a
subsequent Set can safely complete the reconciliation.

Install `dscapt` and `dscapt.dsc.resource.json` together in a directory on
`PATH` for DSC to discover the resource. Creating or removing a system source
file requires sufficient filesystem permissions.

## Example

```json
{
  "name": "example",
  "uri": "https://packages.example.org/debian",
  "suite": "trixie",
  "components": ["main"],
  "architectures": ["amd64"],
  "signingKey": "-----BEGIN PGP PUBLIC KEY BLOCK-----\n...\n-----END PGP PUBLIC KEY BLOCK-----"
}
```

Set `"ensure": "Absent"` with only `"name"` to remove the corresponding
`<name>.sources` file and, if it is not shared, the
`/etc/apt/keyrings/<name>.asc` keyring.
