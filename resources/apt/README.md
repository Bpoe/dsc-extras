# dscapt: APT repository resource

`dscapt` is a Go-based DSCv3 resource for managing one APT repository per
Deb822 source file. It implements `Get`, `Set`, and `Test` for
`DscExtras.Apt/Repository`. Each managed repository uses exactly two files:

- `/etc/apt/sources.list.d/<name>.sources`
- `/etc/apt/keyrings/<name>.gpg`

`signingKey` continues to accept the complete inline ASCII-armored OpenPGP
public key. The resource validates and decodes it into a binary `.gpg` keyring,
then points the Deb822 `Signed-By` field at that file. `Get` returns the actual
key as ASCII armor, and `Test` compares parsed public-key material rather than
armor formatting. No key downloads or user-specified key paths are supported.

Each repository exclusively owns its `.sources` and `.gpg` files. Other
repositories must not reference a keyring owned by this resource. On
`ensure: Absent`, both managed files are removed without inspecting unrelated
APT sources. Deb822 sources with external binary keyrings target APT 1.4 or
newer as the conservative supported baseline.

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
`<name>.sources` file and the
`/etc/apt/keyrings/<name>.gpg` keyring.
