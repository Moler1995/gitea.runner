#!/usr/bin/env bash
# Runs the Go tests on Windows in a dockur/windows QEMU VM on the docker host, see DEVELOPMENT.md.
#
# Env:
#   WINDOWS_TEST_IMAGE    dockur/windows image, pinned in the Makefile
#   WINDOWS_TEST_TIMEOUT  minutes to wait for installing Windows and running the tests (default 165)
set -euo pipefail

image="${WINDOWS_TEST_IMAGE:?}"
timeout="${WINDOWS_TEST_TIMEOUT:-165}"
name="gitea-runner-windows-test-$$"
work="$(mktemp -d)"

cleanup() {
  docker rm -fv "$name" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

echo "==> Building Windows test binaries"
oem="$work/oem"
mkdir -p "$oem/bin" "$oem/src" "$work/shared"
module="$(go list -m)"
packages="$(GOOS=windows go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...)"
GOOS=windows GOARCH=amd64 go test -c -ldflags='-s -w' -o "$oem/bin/" $packages
for package in $packages; do
  echo "${package##*/}.test.exe ${package#"$module"/}"
done > "$oem/tests.txt"
git ls-files -z --cached --others --exclude-standard | while IFS= read -r -d '' file; do
  [ -e "$file" ] && printf '%s\0' "$file"
done | tar --null -T - -cf - | tar -xf - -C "$oem/src"
# a repository like the CI checkout, for the tests that read the github context from it
git -C "$oem/src" init -q -b main
git -C "$oem/src" add -Af .
git -C "$oem/src" -c user.name=test -c user.email=test@localhost -c commit.gpgsign=false -c core.hooksPath=/dev/null commit -qm test
git -C "$oem/src" remote add origin https://gitea.com/gitea/runner
cp scripts/test-windows-setup.ps1 scripts/test-windows.ps1 "$oem/"
printf '%s\r\n' '@powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\OEM\test-windows-setup.ps1 && C:\pwsh\pwsh.exe -NoProfile -ExecutionPolicy Bypass -File C:\OEM\test-windows.ps1' > "$oem/install.bat"

kvm=Y
if ! docker run --rm --privileged --entrypoint sh "$image" -c 'test -w /dev/kvm'; then
  kvm=N
  echo "==> The docker host has no usable /dev/kvm, Windows runs emulated and much slower"
fi

echo "==> Installing Windows"
# the timeout stops the VM even when this script is killed before its cleanup runs
docker create --rm --name "$name" --privileged -e VERSION=2025 -e EDITION=core -e KVM="$kvm" -e CPU_CORES=4 -e RAM_SIZE=4G \
  --entrypoint timeout "$image" "$((timeout + 10))m" /usr/bin/tini -s /run/entry.sh >/dev/null
docker cp "$oem" "$name:/oem"
docker cp "$work/shared" "$name:/shared"
docker start "$name" >/dev/null
docker logs -f "$name" &
docker exec "$name" sh -c 'touch /shared/summary.txt && tail -f /shared/summary.txt' &

if ! timeout "${timeout}m" docker exec "$name" sh -c 'until [ -f /shared/exitcode ]; do sleep 10; done'; then
  echo "Windows did not finish the tests within $timeout minutes" >&2
  exit 1
fi
results="$work/results"
docker cp "$name:/shared" "$results"

tr -d '\r' < "$results/summary.txt" | while read -r result package _; do
  if [ "$result" = FAIL ]; then
    echo "::group::$package"
    cat "$results/${package//\//-}.log"
    echo "::endgroup::"
  fi
done
if [ -f "$results/error.txt" ]; then
  cat "$results/error.txt" >&2
fi
echo "==> Slowest tests"
find "$results" -name '*.log' -exec cat {} + | tr -d '\r' | sed -nE 's/^--- (PASS|FAIL): (\S+) \(([0-9.]+)s\)$/\3 \2/p' | sort -rn | head -20
echo "==> Packages"
tr -d '\r' < "$results/summary.txt"
exit "$(tr -d '\r\n' < "$results/exitcode")"
