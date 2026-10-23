#!/usr/bin/env bash
set -euo pipefail

# Build a fully-static DSC Resource Repository for all published, stable
# dsclinux and dscpm releases. Archives remain in GitHub Releases.
# Requires: gh, jq, unzip, dscpkg, and a GitHub token with contents:read.

: "${GH_REPO:?Set GH_REPO, e.g. Bpoe/dsc-extras}"
: "${DSCPKG:?Set DSCPKG to the compiled dscpkg binary}"
site_dir="${1:-site}"
mkdir -p "$site_dir"

releases="$(
  gh api --paginate "repos/${GH_REPO}/releases" \
    --jq '.[] | select(.draft == false and .prerelease == false) | .tag_name' \
  | grep -E '^(dsclinux|dscpm)-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' \
  | LC_ALL=C sort -V
)"
if [[ -z "$releases" ]]; then
  echo 'No published stable dsclinux or dscpm releases found' >&2
  exit 1
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

while IFS= read -r tag; do
  [[ -n "$tag" ]] || continue
  component="${tag%%-v*}"
  version="${tag#"${component}-v"}"

  case "$component" in
    dsclinux)
      package='dscextras/linux'
      manifests=(services.dsc.resource.json groups.dsc.resource.json)
      types=(DscExtras.Linux/services DscExtras.Linux/groups)
      ;;
    dscpm)
      package='dscextras/package-management'
      manifests=(aptRepositories.dsc.resource.json packages.dsc.resource.json dscapt.dsc.resource.json)
      types=(DscExtras.PackageManagement/aptRepositories DscExtras.PackageManagement/packages DscExtras.Apt/Repository)
      ;;
    *)
      echo "Unsupported release component: $component" >&2
      exit 1
      ;;
  esac

  for arch in amd64 arm64; do
    platform="linux_${arch}"
    asset="${component}-${version}-linux-${arch}.zip"
    archive_dir="$work_dir/${component}-${version}-${arch}"
    mkdir -p "$archive_dir"
    gh release download "$tag" --repo "$GH_REPO" --pattern "$asset" --dir "$archive_dir"
    archive="$archive_dir/$asset"
    test -s "$archive"
    unzip -tqq "$archive"

    args=(publish --repository "$site_dir" --package "$package" \
      --package-version "$version" --platform "$platform" \
      --archive-url "https://github.com/${GH_REPO}/releases/download/${tag}/${asset}")
    for i in "${!manifests[@]}"; do
      actual_type="$(unzip -p "$archive" "${manifests[$i]}" | jq -er '.type')"
      actual_version="$(unzip -p "$archive" "${manifests[$i]}" | jq -er '.version')"
      if [[ "$actual_type" != "${types[$i]}" || "$actual_version" != "$version" ]]; then
        echo "Unexpected manifest in $asset: ${manifests[$i]}: type=$actual_type, version=$actual_version" >&2
        exit 1
      fi
      args+=(--resource "${actual_type}@${actual_version}")
    done

    # dscpkg downloads the URL and hashes the exact published ZIP, while
    # storing only its metadata in site_dir (no duplicate ZIPs in Pages).
    "$DSCPKG" "${args[@]}"
  done
  echo "Indexed ${component} ${version} for Linux AMD64 and ARM64"
done <<< "$releases"

# GitHub Actions' Pages artifact upload preserves .well-known. This file also
# permits branch-based Pages deployment without Jekyll stripping hidden paths.
touch "$site_dir/.nojekyll"
cat > "$site_dir/index.html" <<'HTML'
<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>DSC Extras Resource Repository</title></head>
<body>
<h1>DSC Extras Resource Repository</h1>
<p>Static DSC Resource Repository for <code>dsclinux</code> and <code>dscpm</code>.</p>
<p><a href=".well-known/dsc.json">Discovery document</a> · <a href="v1/catalog.json">Catalog</a></p>
<p>Archives are served from <a href="https://github.com/Bpoe/dsc-extras/releases">GitHub Releases</a>.</p>
</body>
</html>
HTML

test -s "$site_dir/.well-known/dsc.json"
test -s "$site_dir/v1/catalog.json"
echo "Built resource repository in $site_dir"
