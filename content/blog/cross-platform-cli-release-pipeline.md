+++
title = "给一个跨平台 CLI 搭一条发版流水线：Makefile → CI → 包仓库 → 一键安装"
date = 2026-08-25
description = "以 tool-terminal 为例，串起一个 Rust CLI 从「打 tag 发版」到「用户 curl | sh 一键安装」的全套流程：Makefile 自动版本号与本地构建、GitLab CI 多平台二进制构建与上传、install.sh 探测平台并从包仓库拉取最新构建。"
[taxonomies]
tags = ["rust", "ci"]
[extra]
toc = true
+++

一个命令行工具写完了，怎么让别人装上？「clone 下来自己 `cargo build`」显然不行。成熟的做法是：**每次打一个 git tag，CI 自动为所有平台编译二进制、打包上传到包仓库，用户一条 `curl … | sh` 就装好**。

这篇以 [tool-terminal](https://github.com/spursy/cli-terminal)（一个演示用的 Rust CLI，二进制名 `tool`）为例，把这条流水线拆成三段讲清楚：本地发版（Makefile）、CI 构建上传（GitLab CI）、用户安装（install.sh）。每一段都有几个容易踩的坑，一并记下来。

<!-- more -->

## 全景：一次发版发生了什么

```
开发者本地                    GitLab CI                      用户机器
──────────                    ─────────                      ────────
make release
  ├ 算出版本号 vX.Y.Z
  ├ 构建 macOS 二进制
  ├ commit + git tag
  └ git push tag  ───触发───▶  release 流水线
                               ├ linux-x64  (原生编)
                               ├ linux-arm64 (交叉编)
                               ├ darwin-*   (上传预构建)
                               └ version.txt (记录最新版)
                                     │
                                     ▼
                               包仓库(generic packages)
                                <pkg>/<ver>/tool-<os>-<arch>.tar.gz
                                <pkg>/latest/version.txt
                                     ▲
                                     │
                   curl … /install.sh | sh  ◀── 用户执行
                     ├ 探测 os-arch
                     ├ 读 latest/version.txt
                     ├ 下载对应 tarball
                     └ 解压 + 配 PATH
```

要产出的产物固定为四个平台的 tarball，加一个记录最新版本号的文本：

```
<pkg>/<version>/tool-linux-x64.tar.gz
<pkg>/<version>/tool-linux-arm64.tar.gz
<pkg>/<version>/tool-darwin-arm64.tar.gz
<pkg>/<version>/tool-darwin-x64.tar.gz
<pkg>/latest/version.txt          # 内容就是版本号字符串
```

`<os>-<arch>` 这套命名(`linux-x64` / `darwin-arm64` …)是 install.sh 探测后直接拼出来的，所以**三处（CI 产出、install.sh 下载、tarball 内的二进制名）必须严格一致**——这是整条链最容易对不上的地方。

---

## 第一段：Makefile —— 把发版收敛成一条命令

发版的手工步骤其实不少：编两个 mac 架构、拷进 `dist/`、commit、打 tag、推代码再推 tag。人工做迟早会漏。用一个 `Makefile` 收敛成 `make release`。

### 为什么 mac 二进制要在本地编

先解释一个设计前提：**Linux 上默认编不出 macOS 二进制**。Linux 的编译器不认 `apple-darwin` 目标，链接阶段也缺苹果的系统库。真要在 Linux 上交叉编 mac，得引入 osxcross + 一份苹果 SDK，配置成本高。

所以这里采用一个务实方案：**mac 的两个架构在开发者自己的 Mac 上原生编好，提交进仓库（用 Git LFS 存，避免撑大 git 历史），CI 只负责打包上传**。Linux 的两个架构才由 CI 在容器里编。

> Apple Silicon 的 Mac 上可以同时原生编出 `aarch64-apple-darwin` 和 `x86_64-apple-darwin` 两个目标（后者不走 Rosetta），所以一台 Mac 就够。

### 自动版本号

发版时不想每次手敲版本号，规则设为：**手动传优先，不传就取最大的 `vX.Y.Z` tag、把 patch 位 +1**。

```makefile
# VERSION 命令行显式传值时覆盖本块；为空才计算,`:=` 保证最多算一次。
ifeq ($(strip $(VERSION)),)
VERSION := $(shell \
  latest=$$(git tag --list 'v*' | sed 's/^v//' \
    | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$$' \
    | sort -t. -k1,1n -k2,2n -k3,3n | tail -n1); \
  if [ -z "$$latest" ]; then echo v0.0.1; \
  else echo "$$latest" | awk -F. '{printf "v%d.%d.%d\n", $$1, $$2, $$3 + 1}'; fi)
endif
```

**坑 1：版本号要按数字排序，不能按字典序。** `sort` 默认是字典序，`v0.0.9` 会被排到 `v0.0.10` 后面。`sort -t. -k1,1n -k2,2n -k3,3n` 按 `.` 分段、每段按数字比，才能得到真正的最大版本。

### release 目标

```makefile
release: check-version stage-macos
	git add .gitattributes $(DIST_DIR)/tool-darwin-arm64 $(DIST_DIR)/tool-darwin-x64
	@# 重新编出的二进制与已提交的字节一致时无东西可提交,跳过 commit 而不是报错退出,
	@# 这样仍能给当前 HEAD 打 tag 触发发版。
	@if git diff --cached --quiet; then \
	  echo "No binary changes to commit; tagging current HEAD."; \
	else \
	  git commit -m "release: macOS binaries for $(VERSION)"; \
	fi
	git tag $(VERSION)
	git push origin $(MAIN_BRANCH)   # 先推代码 + LFS 对象
	git push origin $(VERSION)       # 再推 tag,触发 CI
```

这里有两个来自实战的细节：

**坑 2：release 构建可复现，重跑会「nothing to commit」。** 开了 LTO 的 release 构建通常字节一致，如果重新发一版而二进制没变，`git commit` 会因「没有改动」失败并中断整个 `make release`。用 `git diff --cached --quiet` 判断有没有待提交内容，没有就跳过 commit、直接打 tag，让流程继续。

**坑 3：版本校验要放在构建之前。** `release` 依赖 `check-version` 和 `stage-macos`，把 `check-version` 写在**前面**，这样忘了传 `VERSION`（或 tag 已存在）时能立刻失败，而不是白跑一遍几分钟的 `cargo build` 才报错。

```makefile
check-version:
	@test -n "$(VERSION)" || { echo "ERROR: set VERSION, e.g. make release VERSION=v1.2.3"; exit 1; }
	@# 拒绝复用已存在的 tag(手动模式防重复;自动模式已 +1 不会撞)。
	@if git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null; then \
	  echo "ERROR: tag $(VERSION) already exists"; exit 1; fi
```

于是发版就一句话：

```bash
make release                  # 自动版本号(上一个 tag patch +1)
make release VERSION=v1.0.0   # 手动指定
```

### 关于 Git LFS

`dist/` 里的二进制走 Git LFS，靠 `.gitattributes` 声明：

```
dist/tool-darwin-arm64 filter=lfs diff=lfs merge=lfs -text
dist/tool-darwin-x64   filter=lfs diff=lfs merge=lfs -text
```

LFS 的本质是：**大文件不进 git 历史，git 里只留一个几十字节的指针，真实文件存在 LFS 服务端**。这样发一百版也不会把仓库 `.git` 撑大。代价是 CI runner 拉代码时必须开启 LFS，否则只会拿到指针文本而非真实二进制——后面 CI 里会专门校验这一点。

---

## 第二段：GitLab CI —— 多平台构建与上传

CI 里最值得抽象的是「构建 → 打包 → 上传」这段公共逻辑,用一个隐藏 job 模板 `.release-binary` 承载,各平台用 `extends` 复用、靠 `parallel:matrix` 展开。

### 公共模板

```yaml
.release-binary:
  stage: release
  only: [tags]
  variables:
    PACKAGE: tool-terminal
    PACKAGES_API: "${CI_API_V4_URL}/projects/${CI_PROJECT_ID}/packages/generic"
  script:
    - |
      if [ -n "${RUST_TARGET:-}" ]; then          # 有 RUST_TARGET 则交叉编
        rustup target add "$RUST_TARGET"
        cargo build --release --bin tool --target "$RUST_TARGET"
        BIN_DIR="target/${RUST_TARGET}/release"
      else                                          # 否则原生编
        cargo build --release --bin tool
        BIN_DIR="target/release"
      fi
    - TARBALL="tool-${PLATFORM}.tar.gz"
    - tar -czf "$TARBALL" -C "$BIN_DIR" tool
    - VERSION="${CI_COMMIT_TAG:-$CI_COMMIT_SHORT_SHA}"   # 永不为空
    - |
      curl -fsSL --header "JOB-TOKEN: ${CI_JOB_TOKEN}" \
        --upload-file "$TARBALL" \
        "${PACKAGES_API}/${PACKAGE}/${VERSION}/${TARBALL}"
```

几个要点：
- **`only: [tags]`** —— 只在打 tag 时跑,平时推分支不触发发版。
- **`JOB-TOKEN`** —— CI 内置的临时令牌,天然有权限往本项目的包仓库上传,不用配额外密钥。
- **`VERSION` 兜底** —— `${CI_COMMIT_TAG:-$CI_COMMIT_SHORT_SHA}`,确保上传 URL 里的版本段永不为空(空会导致 HTTP 400)。

### Linux：一个 job 出两个架构

```yaml
release-binary:linux:
  extends: .release-binary
  parallel:
    matrix:
      - PLATFORM: linux-x64                              # 原生
      - PLATFORM: linux-arm64                            # 交叉
        RUST_TARGET: aarch64-unknown-linux-gnu
        CARGO_TARGET_AARCH64_UNKNOWN_LINUX_GNU_LINKER: aarch64-linux-gnu-gcc
```

`parallel:matrix` 会把这个 job 展开成两个并行实例,x64 走原生分支、arm64 走交叉分支。

**坑 4：交叉编 Rust 里带 C 代码的 crate,要装「目标架构的 libc 开发头文件」,不只是交叉 gcc。** 很多加密/TLS 相关的 crate 会编译 C 代码,链接时需要 `aarch64` 的 libc 头。只装 `gcc-aarch64-linux-gnu` 不够——它不带头文件;要装 `crossbuild-essential-arm64`(它捆绑了交叉 gcc + `libc6-dev-arm64-cross`),而且**不能加 `--no-install-recommends`**,否则头文件会被跳过。

**坑 5:选纯 Rust 的 TLS 后端,省掉交叉编 OpenSSL 的地狱。** HTTP 客户端如果用系统 OpenSSL,交叉编到 aarch64 时会到处找目标架构的 OpenSSL 安装,极其麻烦。把 TLS 后端换成纯 Rust 实现(如 `rustls`),二进制不再依赖 `libssl`,交叉编直接清爽。

### macOS：不构建,只上传预构建产物

前面说了 mac 二进制在本地编好、提交进 LFS,所以 CI 的 mac job 不 `cargo build`,只是**把 `dist/` 里的文件打包上传**:

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
    - cp "$SRC" "$STAGE/tool"; chmod +x "$STAGE/tool"   # tarball 内必须叫 tool
    - tar -czf "tool-${PLATFORM}.tar.gz" -C "$STAGE" tool
    # ...(上传同上)
  parallel:
    matrix:
      - PLATFORM: darwin-arm64
      - PLATFORM: darwin-x64
```

**坑 6:一定要校验「不是 LFS 指针」。** 如果 runner 没开 LFS,checkout 出来的 `dist/tool-darwin-arm64` 会是一段文本指针(以 `version https://git-lfs...` 开头),而不是真二进制。直接打包上传的话,用户装完跑起来直接崩。用 `head -c 64 … | grep git-lfs` 提前拦下,报错比让用户踩雷强。

### version.txt：让 install.sh 知道「最新版」

单独一个 job 上传 `latest/version.txt`,内容就是本次 tag。install.sh 靠读它来解析最新版本:

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

## 第三段：install.sh —— 一键安装

用户侧的目标是一条命令:

```bash
curl -fsSL https://raw.githubusercontent.com/spursy/cli-terminal/refs/heads/master/scripts/install.sh | sh
```

`-fsSL` 这四个 flag 缺一不可:`-f` 让 HTTP 错误(如 404)直接失败而不是把错误页喂给 sh;`-s` 静默;`-S` 出错时仍打印原因;`-L` 跟随重定向。这是「下载脚本喂给 shell」的标准安全写法。

脚本核心分三步。

### 1. 探测平台

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
  echo "${OS}-${ARCH}"        # 拼成 linux-x64 / darwin-arm64 ...
}
```

注意这里把 `aarch64`/`arm64`、`x86_64`/`amd64` 都归一化——不同系统 `uname -m` 的叫法不一样,不归一化就会拼出仓库里不存在的 tarball 名。

### 2. 读最新版 + 下载(私有仓库要带 token)

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

**坑 7:不要用 `2>/dev/null` 吞掉 curl 的错误。** 我一开始把拉取写成 `curl -fsSL … 2>/dev/null`,结果私有仓库返回 401 时,脚本只会打印一句误导性的「Check your network」,排查半天才发现是**鉴权**问题不是网络问题。改成**捕获 HTTP 状态码**、对 401/403 给出「设置 token」的明确指引,才好用。

包仓库如果继承自私有项目,匿名请求会吃 401。脚本读一个环境变量 `TOOL_REPO_TOKEN`,有则作为认证头带上:

```sh
TOOL_REPO_TOKEN="${TOOL_REPO_TOKEN:-}"
if [ -n "$TOOL_REPO_TOKEN" ]; then
  AUTH_HEADER="PRIVATE-TOKEN: ${TOOL_REPO_TOKEN}"
fi
```

用户用一个「只读包仓库」权限的 access token 即可:

```bash
curl -fsSL https://<host>/install.sh | TOOL_REPO_TOKEN=<token> sh
```

> 注意:`VAR=x curl … | sh` 里变量只加到了 curl,**没进 sh**;要写成 `curl … | VAR=x sh` 或提前 `export`,脚本才读得到。这是管道两侧是两个独立进程导致的。

### 3. 解压 + 配 PATH

下载的 tarball 解压出一个 `tool` 二进制,放进 `~/.local/bin`(默认),再把这个目录写进登录 shell 的 rc 文件。这里也有个小坑:

**坑 8:判断用哪个 rc 文件要看 `$SHELL`,不能看 `$0`。** `curl | sh` 会让 `$0` 恒为 `sh`,拿它判断没意义。应按 `$SHELL` 的 basename 分派(`zsh`→`.zshrc`,`bash`→`.bashrc`,macOS 的 bash 登录 shell 还得额外写 `.bash_profile`)。

### 让服务自己 serve install.sh

install.sh 得有个地方 host。如果这个 CLI 本身带一个后端服务(用 axum 之类),最省事的办法是让服务**把脚本内容编译期内嵌、加一个路由返回**:

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

`include_str!` 在编译期把脚本烤进二进制,运行时零 IO、且不依赖文件系统里有这个脚本——镜像少一个文件依赖,也不会因为路径错而 404。

---

## 小结:三段各自解决什么

| 段 | 工具 | 解决的问题 | 关键坑 |
|----|------|-----------|--------|
| 本地发版 | Makefile | 把多步发版收敛成一条命令、自动版本号、mac 本地编 | 数字排序、可复现构建的 nothing-to-commit、校验前置 |
| CI 构建上传 | GitLab CI | 多平台并行构建、上传包仓库 | 交叉编 libc 头、纯 Rust TLS、LFS 指针校验 |
| 用户安装 | install.sh | 探测平台、拉最新版、一键装 | 别吞 curl 错误、arch 归一化、按 `$SHELL` 配 PATH |

三段之间的契约就一句话:**`<os>-<arch>` 命名与 tarball 内的二进制名,在产出、下载、解压三处必须完全一致**。对齐了这条,`git tag` 一推、`curl | sh` 一跑,整条链就通了。

完整示例见 [github.com/spursy/tool-terminal](https://github.com/spursy/tool-terminal)。
