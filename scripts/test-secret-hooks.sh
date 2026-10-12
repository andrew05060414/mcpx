#!/usr/bin/env bash
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
if [ ! -f "$repo_root/.gitleaks.toml" ]; then
    repo_root=$script_dir
fi
tmp_root=$(mktemp -d "$repo_root/.secret-hook-test.XXXXXX")
trap 'rm -rf "$tmp_root"' EXIT

git -C "$tmp_root" init -q
git -C "$tmp_root" config user.name "MCPX hook test"
git -C "$tmp_root" config user.email "mcpx-hook-test@example.invalid"
mkdir -p "$tmp_root/.githooks"
cp "$repo_root/.githooks/pre-commit" "$repo_root/.githooks/pre-push" "$tmp_root/.githooks/"
cp "$repo_root/.gitleaks.toml" "$tmp_root/.gitleaks.toml"
git -C "$tmp_root" config core.hooksPath .githooks

printf 'safe=true\n' > "$tmp_root/safe.txt"
git -C "$tmp_root" add safe.txt
git -C "$tmp_root" commit -qm "safe baseline"

prefix_secret="gho_$(printf '%016d' 0 | tr '0' 'x')"
printf 'MCPX_TEST_TOKEN=%s\n' "$prefix_secret" > "$tmp_root/prefix-secret.txt"
git -C "$tmp_root" add prefix-secret.txt
if git -C "$tmp_root" commit -qm "must be blocked"; then
    echo "FAIL: pre-commit accepted a synthetic provider token" >&2
    exit 1
fi
echo "PASS: pre-commit blocked synthetic provider token"
git -C "$tmp_root" reset -q
rm -f "$tmp_root/prefix-secret.txt"

hex_secret=$(printf '%s%s%s%s' '0123456789abcdef' '0123456789abcdef' '0123456789abcdef' '0123456789abcdef')
printf 'MCPX_TEST_SECRET=%s\n' "$hex_secret" > "$tmp_root/hex-secret.txt"
git -C "$tmp_root" add hex-secret.txt
if git -C "$tmp_root" commit -qm "must be blocked"; then
    echo "FAIL: pre-commit accepted a synthetic 64-character hex secret" >&2
    exit 1
fi
echo "PASS: pre-commit blocked synthetic 64-character hex secret"
git -C "$tmp_root" reset -q
rm -f "$tmp_root/hex-secret.txt"

printf 'seed=true\n' > "$tmp_root/seed.txt"
git -C "$tmp_root" add seed.txt
git -C "$tmp_root" config core.hooksPath /dev/null
git -C "$tmp_root" commit -qm "seed bypass candidate"
git -C "$tmp_root" config core.hooksPath .githooks
git -C "$tmp_root" init --bare -q "$tmp_root/remote.git"
git -C "$tmp_root" remote add origin "$tmp_root/remote.git"
git -C "$tmp_root" push -q origin HEAD:main

printf 'MCPX_PUSH_TOKEN=%s\n' "$prefix_secret" > "$tmp_root/push-secret.txt"
git -C "$tmp_root" add push-secret.txt
git -C "$tmp_root" config core.hooksPath /dev/null
git -C "$tmp_root" commit -qm "pre-push candidate"
git -C "$tmp_root" config core.hooksPath .githooks
if git -C "$tmp_root" push -q origin HEAD:main; then
    echo "FAIL: pre-push accepted a synthetic provider token" >&2
    exit 1
fi
echo "PASS: pre-push blocked synthetic provider token"

# Simulate a credential already in this destination's history. Hook bypasses
# below apply only to synthetic fixture setup inside the disposable test repo.
git -C "$tmp_root" -c core.hooksPath=/dev/null push -q origin HEAD:legacy-fixture
printf 'new_branch_safe=true\n' > "$tmp_root/new-branch-safe.txt"
git -C "$tmp_root" add new-branch-safe.txt
git -C "$tmp_root" commit -qm "safe change above remote historical fixture"
git -C "$tmp_root" push -q origin HEAD:new-safe-branch
echo "PASS: new branch excludes history already in its destination"

printf 'MCPX_NEW_BRANCH_TOKEN=%s\n' "$prefix_secret" > "$tmp_root/new-branch-secret.txt"
git -C "$tmp_root" add new-branch-secret.txt
git -C "$tmp_root" -c core.hooksPath=/dev/null commit -qm "new branch secret fixture"
if git -C "$tmp_root" push -q origin HEAD:new-secret-branch; then
    echo "FAIL: new branch accepted a newly introduced secret" >&2
    exit 1
fi
echo "PASS: new branch still blocks new secrets"

git -C "$tmp_root" init --bare -q "$tmp_root/other-remote.git"
git -C "$tmp_root" remote add other "$tmp_root/other-remote.git"
if git -C "$tmp_root" push -q other HEAD:main; then
    echo "FAIL: other remote incorrectly inherited the first remote's baseline" >&2
    exit 1
fi
echo "PASS: baseline is specific to the push destination"
