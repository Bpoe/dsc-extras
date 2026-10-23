# DSC Extras Resource Repository (GitHub-hosted)

This repository exposes published `dsclinux` and `dscpm` ZIP assets through a static
[dscpkg](https://github.com/Bpoe/dscpkg) Resource Repository. No server, database,
container, hosted registry, or separate archive upload is necessary.

- **Repository metadata:** GitHub Pages, under `https://bpoe.github.io/dsc-extras/`.
- **Archive downloads:** existing immutable URLs under `https://github.com/Bpoe/dsc-extras/releases/download/...`.
- **Publication:** `.github/workflows/dsc-resource-repository.yml` rebuilds the entire static catalog
  from *published, non-prerelease* `dsclinux-vX.Y.Z` and `dscpm-vX.Y.Z` releases.
- **Supported platforms:** `linux_amd64` and `linux_arm64`.

| Package | Resource types |
| --- | --- |
| `dscextras/linux` (`dsclinux`) | `DscExtras.Linux/services`, `DscExtras.Linux/groups` |
| `dscextras/package-management` (`dscpm`) | `DscExtras.PackageManagement/aptRepositories`, `DscExtras.PackageManagement/packages`, `DscExtras.Apt/Repository` (compatibility alias) |

Each resource type resolves to the archive for its **package** and platform, so
installing services and groups doesn't duplicate the `dsclinux` executable. Resource
versions are taken directly from the published manifests, not from the main branch.
The publisher validates release manifest types and versions before adding metadata.

## First-time setup

1. Merge the workflow, scripts and this document into `main`.
2. Open **Settings → Pages** in `Bpoe/dsc-extras`; select **GitHub Actions** as the source.
   A repository owner may need to enable Pages first.
3. In **Actions → DSC Resource Repository**, run **Run workflow** on `main`.
4. Verify `https://bpoe.github.io/dsc-extras/.well-known/dsc.json` and
   `https://bpoe.github.io/dsc-extras/v1/catalog.json` return JSON.

Published future releases automatically retrigger the workflow. `workflow_dispatch`
rebuilds the site manually, and pushes to `main` affecting the publishing files also
rebuild it. The publisher keeps all previously published stable versions discoverable
as long as their releases remain present (including `dscpm` 0.1.0 and 0.1.1).

## Install

```sh
repo=https://bpoe.github.io/dsc-extras/
dscpkg install --repository "$repo" --resource DscExtras.Linux/groups --version 0.0.1
dscpkg install --repository "$repo" --resource DscExtras.Linux/services --version 0.0.1
dscpkg install --repository "$repo" --resource DscExtras.PackageManagement/packages --version 0.1.1
export DSC_RESOURCE_PATH="$(dscpkg env)"
dsc resource list
```

`dscpkg` verifies downloaded archive hashes recorded in package descriptors and
installs each package version once. DSC sees the bundled resource manifests via
`DSC_RESOURCE_PATH`. Installing a resource registers it locally for later `dscpkg
update` and `dscpkg cleanup` operations.

## Maintenance

The GitHub Actions workflow pins the dscpkg publisher to a reviewed commit rather
than running an unreviewed latest version. Update the pinned commit deliberately
when new publisher behavior is needed. For each release, the workflow checks both
platform archives, validates all expected resource manifests, and generates metadata
using `dscpkg publish --archive-url`. The ZIPs themselves are *not* stored in Pages.

The workflow runs `scripts/smoke-test-dsc-resource-repository.sh` to smoke-test
installation from a local HTTP mirror of the generated Pages site, and verifies that
the two `dsclinux` resources share one installed executable (and likewise two `dscpm`
resources).

The repository is suitable for public demonstration. As in the current dscpkg model,
archive hashes provide integrity checks but do not by themselves establish publisher
identity; first-party namespace signing or stronger provenance can be added separately.
