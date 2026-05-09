#!/bin/sh
# Lint checks for scripts/install.sh
# Verifies POSIX sh compatibility (no bashisms).

set -e

echo "Checking install.sh syntax..."
sh -n scripts/install.sh && echo "OK: Syntax valid"

echo ""
echo "Checking for bashisms..."
fail=0

# Check for [[ ]] conditionals (exclude POSIX [[:class:]] in sed/grep patterns)
if grep -n '\[\[' scripts/install.sh | grep -v '\[\[:' | grep -v '^\s*#'; then
    echo "FAIL: bashism [[ found"
    fail=1
fi

if grep -n '^[^#]*function ' scripts/install.sh; then
    echo "FAIL: bashism 'function' keyword found"
    fail=1
fi

if grep -n '^[^#]*declare ' scripts/install.sh; then
    echo "FAIL: bashism 'declare' found"
    fail=1
fi

# Note: 'local' is technically a bashism but is supported by dash/ash,
# so we don't flag it. The install script avoids it anyway.

if [ "$fail" -ne 0 ]; then
    exit 1
fi

echo "OK: No bashisms detected"

echo ""
echo "Checking channel guard behavior..."

tmp_dir="$(mktemp -d 2>/dev/null || mktemp -d -t lynxdb-install-test)"
trap 'rm -rf "$tmp_dir"' EXIT INT TERM

cat >"$tmp_dir/curl" <<'EOF'
#!/bin/sh
url=""
for arg do
    url="$arg"
done

case "$url" in
    */nightly/manifest.json)
        printf '%s\n' '{"version":"v0.7.0-nightly.20260509.g1a2b3c4"}'
        ;;
    */manifest.json)
        printf '%s\n' '{"version":"v0.6.0"}'
        ;;
    *)
        exit 22
        ;;
esac
EOF
chmod +x "$tmp_dir/curl"

run_resolve() {
    PATH="$tmp_dir:$PATH" \
    LYNXDB_INSTALL_TEST_MODE=resolve \
    LYNXDB_INSTALL_DIR="$tmp_dir/bin" \
    LYNXDB_NO_MODIFY_PATH=1 \
    sh scripts/install.sh --quiet "$@"
}

assert_contains() {
    haystack="$1"
    needle="$2"
    label="$3"
    case "$haystack" in
        *"$needle"*) ;;
        *)
            echo "FAIL: $label"
            echo "Expected to find: $needle"
            echo "$haystack"
            exit 1
            ;;
    esac
}

output="$(run_resolve)"
assert_contains "$output" "VERSION=v0.6.0" "default channel resolves stable manifest"
assert_contains "$output" "CHANNEL=stable" "default channel is stable"
assert_contains "$output" "MANIFEST_URL=https://dl.lynxdb.org/manifest.json" "stable manifest URL"

if output="$(run_resolve --channel nightly 2>&1)"; then
    echo "FAIL: nightly channel without allow-prerelease succeeded"
    echo "$output"
    exit 1
else
    assert_contains "$output" "Nightly installs require explicit prerelease consent" "nightly consent error"
fi

if output="$(
    PATH="$tmp_dir:$PATH" \
    LYNXDB_CHANNEL=nightly \
    LYNXDB_INSTALL_TEST_MODE=resolve \
    LYNXDB_INSTALL_DIR="$tmp_dir/bin" \
    sh scripts/install.sh --quiet 2>&1
)"; then
    echo "FAIL: LYNXDB_CHANNEL=nightly without LYNXDB_ALLOW_PRERELEASE succeeded"
    echo "$output"
    exit 1
else
    assert_contains "$output" "Nightly installs require explicit prerelease consent" "nightly env consent error"
fi

output="$(run_resolve --channel nightly --allow-prerelease)"
assert_contains "$output" "VERSION=v0.7.0-nightly.20260509.g1a2b3c4" "nightly channel resolves nightly manifest"
assert_contains "$output" "CHANNEL=nightly" "nightly channel"
assert_contains "$output" "MANIFEST_URL=https://dl.lynxdb.org/nightly/manifest.json" "nightly manifest URL"

output="$(
    PATH="$tmp_dir:$PATH" \
    LYNXDB_CHANNEL=nightly \
    LYNXDB_ALLOW_PRERELEASE=1 \
    LYNXDB_INSTALL_TEST_MODE=resolve \
    LYNXDB_INSTALL_DIR="$tmp_dir/bin" \
    sh scripts/install.sh --quiet
)"
assert_contains "$output" "VERSION=v0.7.0-nightly.20260509.g1a2b3c4" "nightly env resolves nightly manifest"
assert_contains "$output" "CHANNEL=nightly" "nightly env channel"

if output="$(run_resolve --version v0.7.0-nightly.20260509.g1a2b3c4 2>&1)"; then
    echo "FAIL: explicit prerelease without allow-prerelease succeeded"
    echo "$output"
    exit 1
else
    assert_contains "$output" "Prerelease versions require explicit consent" "explicit prerelease consent error"
fi

output="$(run_resolve --version v0.6.0)"
assert_contains "$output" "VERSION=v0.6.0" "explicit stable version"
assert_contains "$output" "CHANNEL=stable" "explicit stable channel"

echo "OK: Channel guards valid"
