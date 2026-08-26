+++
title = "Building a Release Pipeline for a Cross-Platform CLI: Makefile → CI → Package Registry → One-Line Install"
date = 2026-08-25
description = "Using tool-terminal as an example, this post walks the full path of a Rust CLI from 'push a git tag' to 'users install with curl | sh': a Makefile for auto-versioning and local builds, GitLab CI for multi-platform binary builds and uploads, and an install.sh that detects the platform and pulls the latest build from a package registry."
[taxonomies]
tags = ["rust", "ci"]
[extra]
toc = true
+++

You finished a command-line tool — now how do people install it? "Clone it and `cargo build` yourself" clearly won't do. The mature approach: **every time you push a git tag, CI compiles binaries for every platform, packages and uploads them to a registry, and users install with a single `curl … | sh`**.

This post uses [tool-terminal](https://github.com/spursy/cli-terminal) (a demo Rust CLI whose binary is named `tool`) to break that pipeline into three parts: local release (Makefile), CI build & upload (GitLab CI), and user install (install.sh). Each part has a few easy traps, noted along the way.

<!-- more -->

## Big picture: what happens in one release

```
Developer (local)            GitLab CI                     User machine
─────────────────            ─────────                     ────────────
make release
  ├ compute version vX.Y.Z
  ├ build macOS binaries
  ├ commit + git tag
  └ git push tag  ──triggers──▶  release pipeline
                                 ├ linux-x64  (native)
                                 ├ linux-arm64 (cross)
                                 ├ darwin-*   (upload prebuilt)
                                 └ version.txt (latest version)
                                       │
                                       ▼
                                 package registry (generic)
                                  <pkg>/<ver>/tool-<os>-<arch>.tar.gz
                                  <pkg>/latest/version.txt
                                       ▲
                                       │
                     curl … /install.sh | sh  ◀── user runs
                       ├ detect os-arch
                       ├ read latest/version.txt
                       ├ download the matching tarball
                       └ extract + set up PATH
```

The artifacts are fixed: four platform tarballs plus one text file recording the latest version:

```
<pkg>/<version>/tool-linux-x64.tar.gz
<pkg>/<version>/tool-linux-arm64.tar.gz
<pkg>/<version>/tool-darwin-arm64.tar.gz
<pkg>/<version>/tool-darwin-x64.tar.gz
<pkg>/latest/version.txt          # contents are just the version string
```

The `<os>-<arch>` naming (`linux-x64` / `darwin-arm64` …) is what install.sh builds after detection, so **all three places (CI output, install.sh download, and the binary name inside the tarball) must match exactly** — this is the single easiest thing to get out of sync in the whole chain.

---

## Part 1: Makefile — collapse a release into one command

A release has quite a few manual steps: build two mac arches, copy into `dist/`, commit, tag, push the branch, then push the tag. Do it by hand and you'll eventually miss one. A `Makefile` collapses it into `make release`.

### Why mac binaries are built locally

First, a design premise: **you can't build a macOS binary on Linux by default**. Linux's compiler doesn't know the `apple-darwin` target, and the link step lacks Apple's system libraries. Truly cross-compiling mac from Linux requires osxcross plus an Apple SDK — a high configuration cost.

So this uses a pragmatic scheme: **the two mac arches are built natively on the developer's own Mac and committed into the repo (via Git LFS, to avoid bloating git history); CI only packages and uploads them**. Only the two Linux arches are built by CI in a container.

> On an Apple Silicon Mac you can natively build both `aarch64-apple-darwin` and `x86_64-apple-darwin` (the latter without Rosetta), so one Mac is enough.

### Auto-versioning

You don't want to type a version number every release, so the rule is: **an explicit value wins; otherwise take the highest `vX.Y.Z` tag and bump the patch by 1**.

```makefile
# An explicit command-line VERSION overrides this block; the shell runs only when
# empty, and `:=` computes it at most once.
ifeq ($(strip $(VERSION)),)
VERSION := $(shell \
  latest=$$(git tag --list 'v*' | sed 's/^v//' \
    | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$$' \
    | sort -t. -k1,1n -k2,2n -k3,3n | tail -n1); \
  if [ -z "$$latest" ]; then echo v0.0.1; \
  else echo "$$latest" | awk -F. '{printf "v%d.%d.%d\n", $$1, $$2, $$3 + 1}'; fi)
endif
```

**Trap 1: sort versions numerically, not lexically.** `sort` defaults to lexical order, which puts `v0.0.9` *after* `v0.0.10`. `sort -t. -k1,1n -k2,2n -k3,3n` splits on `.` and compares each field numerically, giving the true maximum.

### The release target

```makefile
release: check-version stage-macos
	git add .gitattributes $(DIST_DIR)/tool-darwin-arm64 $(DIST_DIR)/tool-darwin-x64
	@# When the rebuilt binaries are byte-identical to what is committed, there is
	@# nothing to stage; skip the commit instead of aborting so we can still tag
	@# the current HEAD and trigger a release.
	@if git diff --cached --quiet; then \
	  echo "No binary changes to commit; tagging current HEAD."; \
	else \
	  git commit -m "release: macOS binaries for $(VERSION)"; \
	fi
	git tag $(VERSION)
	git push origin $(MAIN_BRANCH)   # push code + LFS objects first
	git push origin $(VERSION)       # then the tag, which triggers CI
```

Two battle-tested details here:

**Trap 2: reproducible release builds cause "nothing to commit" on a re-run.** An LTO release build is usually byte-identical, so if you cut another release without a binary change, `git commit` fails with "nothing to commit" and aborts the whole `make release`. Use `git diff --cached --quiet` to check for staged changes; if none, skip the commit and tag anyway so the flow continues.

**Trap 3: check the version *before* building.** `release` depends on `check-version` and `stage-macos`; put `check-version` **first** so a missing `VERSION` (or an existing tag) fails immediately instead of after a multi-minute `cargo build`.

```makefile
check-version:
	@test -n "$(VERSION)" || { echo "ERROR: set VERSION, e.g. make release VERSION=v1.2.3"; exit 1; }
	@# Refuse to reuse an existing tag (guards manual mode; auto mode already bumps).
	@if git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null; then \
	  echo "ERROR: tag $(VERSION) already exists"; exit 1; fi
```

So a release is one line:

```bash
make release                  # auto version (previous tag, patch +1)
make release VERSION=v1.0.0   # explicit
```

### About Git LFS

The binaries under `dist/` go through Git LFS, declared in `.gitattributes`:

```
dist/tool-darwin-arm64 filter=lfs diff=lfs merge=lfs -text
dist/tool-darwin-x64   filter=lfs diff=lfs merge=lfs -text
```

The essence of LFS: **large files never enter git history — git stores only a tiny pointer, while the real file lives on the LFS server**. Cut a hundred releases and `.git` still stays lean. The cost: CI runners must have LFS enabled, or they'll fetch the pointer text instead of the real binary — which CI explicitly checks for later.

---

## Part 2: GitLab CI — multi-platform build & upload

The reusable core in CI is the "build → package → upload" logic, carried by a hidden job template `.release-binary`. Each platform reuses it via `extends` and expands through `parallel:matrix`.

### The shared template

```yaml
.release-binary:
  stage: release
  only: [tags]
  variables:
    PACKAGE: tool-terminal
    PACKAGES_API: "${CI_API_V4_URL}/projects/${CI_PROJECT_ID}/packages/generic"
  script:
    - |
      if [ -n "${RUST_TARGET:-}" ]; then          # cross-compile when set
        rustup target add "$RUST_TARGET"
        cargo build --release --bin tool --target "$RUST_TARGET"
        BIN_DIR="target/${RUST_TARGET}/release"
      else                                          # native otherwise
        cargo build --release --bin tool
        BIN_DIR="target/release"
      fi
    - TARBALL="tool-${PLATFORM}.tar.gz"
    - tar -czf "$TARBALL" -C "$BIN_DIR" tool
    - VERSION="${CI_COMMIT_TAG:-$CI_COMMIT_SHORT_SHA}"   # never empty
    - |
      curl -fsSL --header "JOB-TOKEN: ${CI_JOB_TOKEN}" \
        --upload-file "$TARBALL" \
        "${PACKAGES_API}/${PACKAGE}/${VERSION}/${TARBALL}"
```

Key points:
- **`only: [tags]`** — runs only on tag pushes; regular branch pushes don't trigger a release.
- **`JOB-TOKEN`** — CI's built-in ephemeral token; it already has permission to upload to this project's registry, so no extra secret is needed.
- **`VERSION` fallback** — `${CI_COMMIT_TAG:-$CI_COMMIT_SHORT_SHA}` keeps the version segment of the upload URL non-empty (empty → HTTP 400).

### Linux: two arches from one job

```yaml
release-binary:linux:
  extends: .release-binary
  parallel:
    matrix:
      - PLATFORM: linux-x64                              # native
      - PLATFORM: linux-arm64                            # cross
        RUST_TARGET: aarch64-unknown-linux-gnu
        CARGO_TARGET_AARCH64_UNKNOWN_LINUX_GNU_LINKER: aarch64-linux-gnu-gcc
```

`parallel:matrix` expands this job into two parallel instances — x64 takes the native branch, arm64 the cross branch.

**Trap 4: cross-compiling Rust crates that contain C code needs the *target arch's libc dev headers*, not just a cross gcc.** Many crypto/TLS crates compile C code and need the `aarch64` libc headers at link time. Installing only `gcc-aarch64-linux-gnu` is not enough — it ships no headers. Install `crossbuild-essential-arm64` (it bundles the cross gcc plus `libc6-dev-arm64-cross`), and **do not add `--no-install-recommends`**, or the headers get skipped.

**Trap 5: pick a pure-Rust TLS backend to avoid cross-compiling OpenSSL.** If your HTTP client uses the system OpenSSL, cross-compiling to aarch64 makes it hunt for a target-arch OpenSSL install everywhere — a nightmare. Switch the TLS backend to a pure-Rust implementation (e.g. `rustls`), and the binary no longer depends on `libssl`, making cross-compiles painless.

### macOS: don't build, just upload the prebuilt artifacts

As noted, mac binaries are built locally and committed via LFS, so the CI mac job doesn't `cargo build` — it just **packages and uploads the files from `dist/`**:

```yaml
release-binary:macos:
  stage: release
  only: [tags]
  script:
    - SRC="dist/tool-${PLATFORM}"
    - |
      if [ ! -f "$SRC" ]; then
        echo "ERROR: $SRC missing from the tagged commit." >&2; exit 1
      fi
      if head -c 64 "$SRC" | grep -q 'git-lfs'; then
        echo "ERROR: $SRC is a Git LFS pointer, not the real binary." >&2; exit 1
      fi
    - STAGE="$CI_PROJECT_DIR/.macos-stage"; mkdir -p "$STAGE"
    - cp "$SRC" "$STAGE/tool"; chmod +x "$STAGE/tool"   # tarball must contain `tool`
    - tar -czf "tool-${PLATFORM}.tar.gz" -C "$STAGE" tool
    # ...(upload as above)
  parallel:
    matrix:
      - PLATFORM: darwin-arm64
      - PLATFORM: darwin-x64
```

**Trap 6: always check "not an LFS pointer".** If the runner didn't enable LFS, the checked-out `dist/tool-darwin-arm64` is a text pointer (starting with `version https://git-lfs...`), not the real binary. Package and upload that, and users' installs crash on first run. Catch it early with `head -c 64 … | grep git-lfs` — an error beats letting users hit the landmine.

### version.txt: telling install.sh what "latest" is

A separate job uploads `latest/version.txt`, whose contents are just this tag. install.sh reads it to resolve the latest version:

```yaml
release-version:
  only: [tags]
  script:
    - VERSION="${CI_COMMIT_TAG:-$CI_COMMIT_SHORT_SHA}"
    - echo "$VERSION" > version.txt
    - curl -fsSL --header "JOB-TOKEN: ${CI_JOB_TOKEN}" \
        --upload-file version.txt \
        "${PACKAGES_API}/${PACKAGE}/latest/version.txt"
```

---

## Part 3: install.sh — one-line install

The user-facing goal is a single command:

```bash
curl -fsSL https://raw.githubusercontent.com/spursy/cli-terminal/refs/heads/master/scripts/install.sh | sh
```

None of the four `-fsSL` flags are optional: `-f` makes an HTTP error (e.g. 404) fail rather than feeding the error page to sh; `-s` is silent; `-S` still prints the reason on error; `-L` follows redirects. This is the standard safe idiom for "download a script and pipe it to a shell".

The script's core is three steps.

### 1. Detect the platform

```sh
detect_platform() {
  OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  ARCH=$(uname -m)
  case "$OS" in
    darwin) OS="darwin" ;;
    linux)  OS="linux" ;;
    mingw*|msys*|cygwin*) echo "Windows not supported" >&2; exit 1 ;;
  esac
  case "$ARCH" in
    arm64|aarch64) ARCH="arm64" ;;
    x86_64|amd64)  ARCH="x64" ;;
  esac
  echo "${OS}-${ARCH}"        # builds linux-x64 / darwin-arm64 ...
}
```

Note the normalization of `aarch64`/`arm64` and `x86_64`/`amd64` — different systems report `uname -m` differently, and without normalizing you'd build a tarball name that doesn't exist in the registry.

### 2. Read the latest version + download (private registries need a token)

```sh
VERSION_URL="${PACKAGES_URL}/latest/version.txt"
if [ -n "$AUTH_HEADER" ]; then
  HTTP_CODE=$(curl -sSL -H "$AUTH_HEADER" -o "$VERSION_FILE" -w '%{http_code}' "$VERSION_URL")
else
  HTTP_CODE=$(curl -sSL -o "$VERSION_FILE" -w '%{http_code}' "$VERSION_URL")
fi
case "$HTTP_CODE" in
  2*) : ;;
  401|403)
    echo "Access denied (HTTP ${HTTP_CODE}). Set TOOL_REPO_TOKEN and retry:" >&2
    echo "  TOOL_REPO_TOKEN=<token> sh install.sh" >&2
    exit 1 ;;
  000) echo "Could not reach ${VERSION_URL}. Check your network." >&2; exit 1 ;;
esac
```

**Trap 7: don't swallow curl's error with `2>/dev/null`.** I first wrote the fetch as `curl -fsSL … 2>/dev/null`, and when a private registry returned 401 the script printed only a misleading "Check your network" — I spent ages before realizing it was an *auth* problem, not a network one. Capturing the HTTP status code and giving a clear "set a token" hint on 401/403 is what makes it usable.

If the registry inherits from a private project, anonymous requests get 401. The script reads a `TOOL_REPO_TOKEN` env var and, if present, sends it as an auth header:

```sh
TOOL_REPO_TOKEN="${TOOL_REPO_TOKEN:-}"
if [ -n "$TOOL_REPO_TOKEN" ]; then
  AUTH_HEADER="PRIVATE-TOKEN: ${TOOL_REPO_TOKEN}"
fi
```

Users just need a token with read-only package-registry scope:

```bash
curl -fsSL https://<host>/install.sh | TOOL_REPO_TOKEN=<token> sh
```

> Note: in `VAR=x curl … | sh` the variable is attached only to curl, **not to sh**; write `curl … | VAR=x sh` or `export` it beforehand so the script can read it. This is because the two sides of a pipe are separate processes.

### 3. Extract + set up PATH

The downloaded tarball extracts a `tool` binary into `~/.local/bin` (default), then that directory is written into the login shell's rc file. One more small trap here:

**Trap 8: decide which rc file to use from `$SHELL`, not `$0`.** `curl | sh` forces `$0` to be `sh`, so it's useless for detection. Dispatch on the basename of `$SHELL` (`zsh`→`.zshrc`, `bash`→`.bashrc`; on macOS a bash *login* shell also needs `.bash_profile`).

### Letting the service serve install.sh

install.sh needs somewhere to be hosted. If the CLI ships a backend service (using something like axum), the simplest option is to have the service **embed the script at compile time and expose it on a route**:

```rust
async fn install_script() -> impl IntoResponse {
    const SCRIPT: &str = include_str!("../../scripts/install.sh");
    (
        [(header::CONTENT_TYPE, "text/x-shellscript; charset=utf-8")],
        SCRIPT,
    )
}
// .route("/install.sh", get(install_script))
```

`include_str!` bakes the script into the binary at compile time — zero runtime IO, and no dependency on the script existing on the filesystem, so the image has one fewer file dependency and can't 404 on a wrong path.

---

## Summary: what each part solves

| Part | Tool | Problem it solves | Key traps |
|------|------|-------------------|-----------|
| Local release | Makefile | Collapse multi-step release into one command; auto-version; build mac locally | numeric sort, nothing-to-commit on reproducible builds, check first |
| CI build & upload | GitLab CI | Parallel multi-platform builds; upload to registry | cross-compile libc headers, pure-Rust TLS, LFS pointer check |
| User install | install.sh | Detect platform, fetch latest, install in one line | don't swallow curl errors, arch normalization, PATH via `$SHELL` |

The contract between the three parts is one sentence: **the `<os>-<arch>` naming and the binary name inside the tarball must be identical across output, download, and extraction**. Get that right, and a `git tag` push plus a `curl | sh` makes the whole chain flow.

See the full example at [github.com/spursy/tool-terminal](https://github.com/spursy/tool-terminal).
