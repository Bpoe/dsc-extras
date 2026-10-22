# dsc-extras

`dsc-extras` is a collection of independently useful resources and extensions
for [DSC v3](https://github.com/PowerShell/DSC). Each component lives in its own
directory and can be built, tested, and released independently.

## Components

- [Package management component (`dscpm`)](resources/package-management/README.md)
  provides `DscExtras.PackageManagement/aptRepositories` to manage APT
  repositories and `DscExtras.PackageManagement/packages` to manage repository
  packages and local archives in batched APT or DNF installation transactions. The
  compatibility manifest preserves
  `DscExtras.Apt/Repository` for existing DSC configurations; direct `dscapt`
  CLI users must migrate to `dscpm aptRepositories`. RHEL/Rocky/AlmaLinux 8, 9,
  and 10 use DNF/RPM; DNF repositories and DNF removal remain future scope.
- [Azure Key Vault secret extension (`dsckvsec`)](extensions/azure-keyvault/README.md)
  retrieves Azure Key Vault secrets for DSC's `secret()` configuration function.
- [Linux component (`dsclinux`)](resources/linux/README.md) provides
  `DscExtras.Linux/services` to manage systemd unit activation, persistent
  enablement, and persistent masking independently, for system and user scopes.

Resource names use a PascalCase namespace and plural camelCase type, as in
`DscExtras.PackageManagement/aptRepositories` and
`DscExtras.PackageManagement/packages`. `dnfRepositories` remains a future type.
