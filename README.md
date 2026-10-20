# dsc-extras

`dsc-extras` is a collection of independently useful resources and extensions
for [DSC v3](https://github.com/PowerShell/DSC). Each component lives in its own
directory and can be built, tested, and released independently.

## Components

- [Package management component (`dscpm`)](resources/package-management/README.md)
  provides `DscExtras.PackageManagement/aptRepositories` to manage APT
  repositories using Deb822 source files. The compatibility manifest preserves
  `DscExtras.Apt/Repository` for existing DSC configurations; direct `dscapt`
  CLI users must migrate to `dscpm aptRepositories`. APT packages, DNF
  repositories, and DNF packages are future scope, not implemented resources.
- [Azure Key Vault secret extension (`dsckvsec`)](extensions/azure-keyvault/README.md)
  retrieves Azure Key Vault secrets for DSC's `secret()` configuration function.

Resource names use a PascalCase namespace and plural camelCase type, as in
`DscExtras.PackageManagement/aptRepositories`. Future package-management types
include `packages` and `dnfRepositories`.
