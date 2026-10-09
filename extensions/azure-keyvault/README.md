# dsckvsec: Azure Key Vault secret extension

`dsckvsec` is a cross-platform command-line extension for retrieving a secret
from Azure Key Vault for DSC v3's `secret()` configuration function. It uses
Azure's official Go SDK and `DefaultAzureCredential`.

The extension follows DSC's secret protocol: it writes only the secret value
to stdout, without a newline. A genuine `SecretNotFound` result exits
successfully with empty stdout so DSC can query other providers. Other failures
exit nonzero and write a sanitized JSON-line diagnostic to stderr.

## Build

Run these commands from `extensions/azure-keyvault`.

Build for Linux:

```sh
mkdir -p dist/linux-amd64
GOOS=linux GOARCH=amd64 go build -o dist/linux-amd64/dsckvsec ./cmd/dsckvsec
```

Build for Windows:

```powershell
New-Item -ItemType Directory -Force dist\windows-amd64 | Out-Null
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -o dist/windows-amd64/dsckvsec.exe ./cmd/dsckvsec
```

To run the local checks:

```sh
gofmt -w ./cmd
go vet ./...
go test ./...
```

## Install

DSC discovers extension manifests in directories on `PATH`. Put the executable
and `dsckvsec.dsc.extension.json` together in a directory on `PATH`; the
manifest names `dsckvsec` as its executable.

On Linux, for example:

```sh
install -d "$HOME/.local/bin"
install -m 0755 dist/linux-amd64/dsckvsec "$HOME/.local/bin/dsckvsec"
install -m 0644 dsckvsec.dsc.extension.json "$HOME/.local/bin/"
export PATH="$HOME/.local/bin:$PATH"
dsc extension list
```

For a system service, install both files in a suitable system directory such
as `/usr/local/bin` and ensure that directory is in the service's `PATH`.

On Windows, copy `dist\windows-amd64\dsckvsec.exe` and
`dsckvsec.dsc.extension.json` into a directory such as
`C:\Program Files\dsckvsec`, then add that directory to the system `PATH`:

```powershell
$destination = "C:\Program Files\dsckvsec"
New-Item -ItemType Directory -Force $destination
Copy-Item .\dist\windows-amd64\dsckvsec.exe $destination
Copy-Item .\dsckvsec.dsc.extension.json $destination
$env:Path += ";$destination"
dsc extension list
```

The examples assume the directory is discoverable by DSC; run `dsc extension
list` in the same environment that will invoke DSC to confirm discovery.

## Authentication

The extension uses `azidentity.NewDefaultAzureCredential`; credentials are
never accepted as command-line arguments. Depending on the environment, the
credential chain can use:

- Azure CLI credentials (`az login`) for local development.
- Standard environment-based service principal credentials such as
  `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, and `AZURE_CLIENT_SECRET`.
- Managed identity when running on Azure.
- Workload identity when its standard environment and federated-token settings
  are configured.

`DefaultAzureCredential` runs as the identity of the **current process**. A
service such as `dscd` may run as a different user or service account than your
interactive shell, so it may not have access to your Azure CLI login or
environment. Configure a credential source available to the process and grant
that identity permission to read secrets from the vault. Authentication and
authorization failures are operational errors, not “secret not found.”

## Usage

The vault URI must be supplied as an absolute HTTPS data-plane URI:

```sh
dsckvsec --name DatabasePassword \
  --vault https://production.vault.azure.net/
```

Use this command directly only when it is safe for the terminal to display the
secret. Successful output has no trailing newline or other text.

## DSC integration

The extension manifest provides the `--name` and `--vault` arguments expected
by the DSC secret protocol. Reference the extension using the vault URI:

```yaml
password: "[secret('DatabasePassword', 'https://production.vault.azure.net/')]"
```

A minimal configuration can pass the secret through a `secureString` parameter
to a resource that supports secure values:

```yaml
$schema: https://aka.ms/dsc/schemas/v3/bundled/config/document.json
parameters:
  databasePassword:
    type: secureString
    defaultValue: "[secret('DatabasePassword', 'https://production.vault.azure.net/')]"
resources:
  - name: Database connection
    type: Microsoft.DSC.Debug/Echo
    properties:
      output: "[parameters('databasePassword')]"
```

Confirm that DSC sees the installed extension with:

```sh
dsc extension list
```

When a requested secret does not exist in this vault, `dsckvsec` returns
successfully with no output. DSC interprets that as “not found in this
provider,” allowing it to query other registered secret extensions. Errors
such as an inaccessible vault, invalid credentials, or service failures
remain errors.

`secret()` does not automatically make a value secure. Pass the result into a
`secureString` or `secureObject` parameter when the resource supports it, and
avoid unwrapping or emitting secret values in resource output. DSC documents
that unwrapped values can appear in trace output at `TRACE` level; use secure
wrappers and avoid trace logging of inputs containing secrets.

## Limitations

- Only secret retrieval is supported.
- The absolute HTTPS vault URI is required; a vault name or ARM resource ID is
  not accepted.
- The current/latest version is retrieved; explicit version selection is not
  supported in v0.1.
- Empty and multiline secret values are rejected because they are ambiguous or
  unsupported by DSC's secret-extension protocol.
- Authentication must work in the security context of the process invoking
  DSC.
