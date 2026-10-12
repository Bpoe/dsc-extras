# dsc-extras

`dsc-extras` is a collection of independently useful resources and extensions
for [DSC v3](https://github.com/PowerShell/DSC). Each component lives in its own
directory and can be built, tested, and released independently.

## Components

- [APT repository resource (`dscapt`)](resources/apt/README.md)
  manages APT repositories using Deb822 source files.
- [Azure Key Vault secret extension (`dsckvsec`)](extensions/azure-keyvault/README.md)
  retrieves Azure Key Vault secrets for DSC's `secret()` configuration function.
