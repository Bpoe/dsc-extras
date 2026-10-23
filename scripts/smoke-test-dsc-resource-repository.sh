#!/usr/bin/env bash
set -euo pipefail

python3 -m http.server 18080 --bind 127.0.0.1 --directory site > "$RUNNER_TEMP/dscpkg-http.log" 2>&1 &
server_pid=$!
trap 'kill "$server_pid" || true' EXIT
repo=http://127.0.0.1:18080
packages="$RUNNER_TEMP/dscpkg-smoke-packages"
for attempt in {1..30}; do
  if curl --fail --silent "$repo/.well-known/dsc.json" > /dev/null; then break; fi
  sleep 1
done
"$DSCPKG" install --repository "$repo" --packages-dir "$packages" --platform linux_amd64 \
  --resource DscExtras.Linux/groups --version 0.0.1
"$DSCPKG" install --repository "$repo" --packages-dir "$packages" --platform linux_amd64 \
  --resource DscExtras.Linux/services --version 0.0.1
test "$(find "$packages" -type f -name dsclinux | wc -l)" -eq 1
"$DSCPKG" install --repository "$repo" --packages-dir "$packages" --platform linux_amd64 \
  --resource DscExtras.PackageManagement/packages --version 0.1.1
"$DSCPKG" install --repository "$repo" --packages-dir "$packages" --platform linux_amd64 \
  --resource DscExtras.PackageManagement/aptRepositories --version 0.1.1
test "$(find "$packages" -type f -name dscpm | wc -l)" -eq 1
