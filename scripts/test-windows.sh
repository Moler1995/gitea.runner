#!/usr/bin/env bash
# Runs the Go tests on Windows in a dockur/windows QEMU VM on the docker host, see DEVELOPMENT.md.
#
# Env:
#   WINDOWS_TEST_IMAGE    dockur/windows image, pinned in the Makefile
#   WINDOWS_TEST_TIMEOUT  minutes to wait for each of installing and testing (default 150)
set -euo pipefail

image="${WINDOWS_TEST_IMAGE:?}"
timeout="${WINDOWS_TEST_TIMEOUT:-150}"
volume=gitea-runner-windows-test
name="gitea-runner-windows-test-$$"
run="run-$(date +%s)-$$"
work="$(mktemp -d)"
vm_env=(-e VERSION=2025 -e EDITION=core -e DISK_FMT=qcow2 -e CPU_CORES=4 -e RAM_SIZE=4G)
# the quarter renews the base before its 180-day evaluation license expires
base="base-$( (echo "$image ${vm_env[*]} $(date +%Y)q$((($(date +%-m) + 2) / 3))" && cat scripts/test-windows-setup.ps1) | sha256sum | cut -c1-16)"

on_volume() {
  docker run --rm --privileged -v "$volume:/cache" --entrypoint sh "$image" -c "$1"
}

cleanup() {
  docker rm -fv "$name" >/dev/null 2>&1 || true
  on_volume "rm -rf /cache/$run" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

read -r kvm installed < <(on_volume "find /cache -mindepth 1 -maxdepth 1 \\( -name 'run-*' -mmin +360 -o -name 'base-*' -mtime +14 \\) -exec rm -rf {} +
  if [ -w /dev/kvm ]; then printf 'Y '; else printf 'N '; fi
  if [ -f /cache/$base/windows.boot ]; then echo Y; else echo N; fi")
if [ "$kvm" = N ]; then
  echo "==> The docker host has no usable /dev/kvm, Windows runs emulated and much slower"
fi

if [ "$installed" = N ]; then
  mkdir "$work/oem"
  cp scripts/test-windows-setup.ps1 "$work/oem/"
  printf '@powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\\OEM\\test-windows-setup.ps1\r\n' > "$work/oem/install.bat"
  # one install per base and docker host, publishing the base itself so it outlives this run
  installer="gitea-runner-windows-test-$base"
  if created="$(docker create --rm --name "$installer" --privileged -v "$volume:/cache" -e STORAGE="/cache/$base-install" -e KVM="$kvm" "${vm_env[@]}" \
    --entrypoint sh "$image" -c "rm -rf \"\$STORAGE\" && mkdir -p \"\$STORAGE/shared\" && timeout ${timeout}m /usr/bin/tini -s /run/entry.sh
      cat \"\$STORAGE/shared/error.txt\" 2>/dev/null
      [ -f \"\$STORAGE/shared/ready\" ] && rm -rf \"\$STORAGE/shared\" && mv -T \"\$STORAGE\" /cache/$base" 2>&1)"; then
    echo "==> Installing Windows into $base, later runs on this docker host boot it directly"
    docker cp "$work/oem" "$installer:/oem"
    docker start -a "$installer" || true
  elif [[ "$created" == *"already in use"* ]]; then
    echo "==> Waiting for another run installing Windows into $base"
    docker wait "$installer" >/dev/null 2>&1 || true
  else
    echo "$created" >&2
    exit 1
  fi
  if ! on_volume "test -f /cache/$base/windows.boot"; then
    echo "Installing Windows failed" >&2
    exit 1
  fi
fi

echo "==> Booting Windows from $base"
on_volume "mkdir -p /cache/$run/shared/results && touch /cache/$base &&
  find /cache/$base -maxdepth 1 -type f ! -name 'data.*' -exec cp {} /cache/$run/ \\; &&
  qemu-img create -q -f qcow2 -F qcow2 -b /cache/$base/data.qcow2 /cache/$run/data.qcow2"
# the timeout stops the VM even when this script is killed before its cleanup runs
docker run -d --rm --name "$name" --privileged -v "$volume:/cache" -e STORAGE="/cache/$run" -e KVM="$kvm" "${vm_env[@]}" \
  --entrypoint timeout "$image" "$((timeout + 10))m" /usr/bin/tini -s /run/entry.sh >/dev/null
docker logs -f "$name" &

echo "==> Building Windows test binaries while it boots"
bundle="$work/bundle"
mkdir -p "$bundle/bin" "$bundle/src"
module="$(go list -m)"
packages="$(GOOS=windows go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...)"
GOOS=windows GOARCH=amd64 go test -c -ldflags='-s -w' -o "$bundle/bin/" $packages
for package in $packages; do
  echo "${package##*/}.test.exe ${package#"$module"/}"
done > "$bundle/tests.txt"
git ls-files -z --cached --others --exclude-standard | while IFS= read -r -d '' file; do
  [ -e "$file" ] && printf '%s\0' "$file"
done | tar --null -T - -cf - | tar -xf - -C "$bundle/src"
# a repository like the CI checkout, for the tests that read the github context from it
git -C "$bundle/src" init -q -b main
git -C "$bundle/src" add -Af .
git -C "$bundle/src" -c user.name=test -c user.email=test@localhost -c commit.gpgsign=false -c core.hooksPath=/dev/null commit -qm test
git -C "$bundle/src" remote add origin https://gitea.com/gitea/runner
docker cp "$bundle" "$name:/cache/$run/shared/bundle"
docker cp scripts/test-windows.ps1 "$name:/cache/$run/shared/" # last, the logon task waits for it
docker exec "$name" sh -c "touch /cache/$run/shared/results/summary.txt && tail -f /cache/$run/shared/results/summary.txt" &

boot=20
[ "$kvm" = Y ] || boot=120
if ! timeout "${boot}m" docker exec "$name" sh -c "until [ -f /cache/$run/shared/results/started ]; do sleep 5; done"; then
  echo "Windows did not start the tests within $boot minutes" >&2
  exit 1
fi
if ! timeout "${timeout}m" docker exec "$name" sh -c "until [ -f /cache/$run/shared/results/exitcode ]; do sleep 5; done"; then
  echo "Windows did not finish the tests within $timeout minutes" >&2
  exit 1
fi
results="$work/results"
docker cp "$name:/cache/$run/shared/results" "$results"

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
