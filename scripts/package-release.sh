#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf 'usage: %s VERSION OUTPUT_DIR\n' "${0##*/}" >&2
}

fail() {
  printf 'package-release: %s\n' "$1" >&2
  exit 1
}

if [[ $# -ne 2 ]]; then
  usage
  exit 64
fi

version=$1
output_dir=$2
semver_pattern='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|([0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))(\.((0|[1-9][0-9]*)|([0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
if [[ ! $version =~ $semver_pattern ]]; then
  fail "version must be semantic version text without a leading v: $version"
fi

mkdir -p -- "$output_dir"
output_dir=$(cd -- "$output_dir" && pwd -P)

shopt -s nullglob
for library in "$output_dir"/codex-header-rewrite-linux-*.so; do
  case ${library##*/} in
    codex-header-rewrite-linux-amd64.so|codex-header-rewrite-linux-arm64.so)
      ;;
    *)
      fail "unsupported architecture library: ${library##*/}"
      ;;
  esac
done
shopt -u nullglob

for arch in amd64 arm64; do
  library="$output_dir/codex-header-rewrite-linux-${arch}.so"
  [[ -f $library ]] || fail "missing architecture library: ${library##*/}"
done

source_date_epoch=${SOURCE_DATE_EPOCH:-0}
[[ $source_date_epoch =~ ^[0-9]+$ ]] || fail "SOURCE_DATE_EPOCH must be a non-negative integer"

stage_dir=$(mktemp -d "${TMPDIR:-/tmp}/codex-header-rewrite-package.XXXXXX")
trap 'rm -rf -- "$stage_dir"' EXIT

for arch in amd64 arm64; do
  library_name="codex-header-rewrite-linux-${arch}.so"
  zip_name="codex-header-rewrite_${version}_linux_${arch}.zip"
  checksum_name="${library_name}.sha256"

  rm -f -- "$output_dir/$zip_name" "$output_dir/$checksum_name"
  cp -- "$output_dir/$library_name" "$stage_dir/codex-header-rewrite.so"
  chmod 0644 "$stage_dir/codex-header-rewrite.so"
  touch -d "@${source_date_epoch}" "$stage_dir/codex-header-rewrite.so"
  (
    cd -- "$stage_dir"
    zip -X -q "$output_dir/$zip_name" codex-header-rewrite.so
  )
  (
    cd -- "$output_dir"
    sha256sum "$library_name" > "$checksum_name"
  )
done

(
  cd -- "$output_dir"
  sha256sum \
    codex-header-rewrite-linux-amd64.so \
    codex-header-rewrite-linux-arm64.so \
    "codex-header-rewrite_${version}_linux_amd64.zip" \
    "codex-header-rewrite_${version}_linux_arm64.zip" \
    | LC_ALL=C sort -k2,2 > checksums.txt
)
