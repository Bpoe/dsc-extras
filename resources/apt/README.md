# dscapt: APT repository resource

`dscapt` is a Go-based DSCv3 resource for managing one APT repository per
Deb822 source file. It implements `Get`, `Set`, and `Test` for
`DscExtras.Apt/Repository`. Each managed repository uses only
`/etc/apt/sources.list.d/<name>.sources`; the resource does not inspect or
modify other source files.

The resource accepts an inline ASCII-armored OpenPGP public key and writes it
as Deb822 `Signed-By` data. It does not download keys or read external key
files.

## Build and check

Run these commands from `resources/apt`:

```sh
mkdir -p dist/linux-amd64
GOOS=linux GOARCH=amd64 go build -o dist/linux-amd64/dscapt ./cmd/dscapt
cp dscapt.dsc.resource.json dist/linux-amd64/
gofmt -w ./cmd
go vet ./...
go test ./...
```

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
`<name>.sources` file.
