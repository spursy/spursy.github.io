+++
title = "cli-terminal：一个 OAuth 设备码 CLI 里几个考究的设计"
date = 2026-08-17
description = "从 cli-terminal 这个演示 RFC 8628 设备码登录的 Rust CLI 出发，拆解四个值得沉淀的工程设计：Token 加密落盘 + 机器绑定、Bearer 默认请求头、惰性回填的令牌桶限流、以及调用前透明续期。"
[taxonomies]
tags = ["rust", "oauth"]
+++

[cli-terminal](https://github.com/spursy/cli-terminal) 是一个演示 **OAuth 2.0 设备授权码模式（Device Authorization Grant, RFC 8628）** 的 Rust 命令行工具，俗称 “Device Flow” / 设备码登录。它的功能很小——`auth login` / `logout` / `status` 加一个 `whoami`——但麻雀虽小，几处围绕「凭据安全」和「客户端自律」的设计很值得单独拎出来讲。

这篇不讲设备码流程本身（那是 RFC 的事），而是聚焦四个可以直接搬到别的项目里的工程设计点。

<!-- more -->

## 一、先一句话交代背景

设备码模式解决的是「**输入受限设备**（TV、CLI、IoT）怎么登录」：设备拿一个 `device_code`、给用户一个短链接，用户在手机/电脑的浏览器里完成授权，设备则轮询 `POST /token` 直到拿到 access token。

cli-terminal 把拿到的 token 存在本地（`~/.cli-terminal/openapi/cli-auth`），之后所有受保护的调用都自动带上它。围绕「怎么存、怎么带、怎么用、怎么续」这四件事，代码里藏了四个考究的选择。

## 二、设计点 1：Token 加密落盘 + 机器绑定（最值得学）

CLI 的 token 存在文件里，最朴素的做法是明文 JSON。cli-terminal 没有这么做，而是**加密落盘**，并且让密钥**绑定到当前机器**——一份被偷走的凭据文件，换台机器就解不开。

文件格式自描述、极简：

```
MAGIC(b"AG\x01") || NONCE[12] || CIPHERTEXT+TAG
```

密钥不是写死的，而是用 **HKDF-SHA256 从机器唯一标识派生**出一把 AES-256 密钥（`src/token.rs`）：

```rust
/// HKDF `info` 串；带版本，方便日后演进派生方案。
const HKDF_INFO: &[u8] = b"cli-token-v1";

/// 用 HKDF-SHA256 从机器 ID 派生 AES-256 密钥。
fn machine_derived_key() -> Key<Aes256Gcm> {
    let id = machine_id();
    let hk = Hkdf::<sha2::Sha256>::new(None, id.as_bytes());
    let mut key_bytes = [0u8; 32];
    // 输出长度 <= 255*HashLen 时 expand 不会失败。
    let _ = hk.expand(HKDF_INFO, &mut key_bytes);
    *Key::<Aes256Gcm>::from_slice(&key_bytes)
}
```

加密时用 AES-256-GCM，随机 nonce，把 `MAGIC || NONCE || 密文` 拼成一段写盘：

```rust
fn encrypt(plaintext: &[u8]) -> Result<Vec<u8>> {
    let key = machine_derived_key();
    let cipher = Aes256Gcm::new(&key);
    let nonce = Aes256Gcm::generate_nonce(&mut OsRng);
    let ciphertext = cipher
        .encrypt(&nonce, plaintext)
        .map_err(|e| anyhow::anyhow!("AES-GCM encrypt failed: {e}"))?;

    let mut out = Vec::with_capacity(MAGIC.len() + nonce.len() + ciphertext.len());
    out.extend_from_slice(MAGIC);
    out.extend_from_slice(&nonce);
    out.extend_from_slice(&ciphertext);
    Ok(out)
}
```

这里有几个细节值得停下来看：

### 1. 机器绑定 = 纵深防御

派生密钥的输入是 `machine_id()`——机器唯一标识。这意味着密文**只能在生成它的那台机器上解密**。凭据文件即使被拷走、上传到别处，攻击者也拿不到明文 token。这是在「文件权限」之外多加的一道纵深防御。

### 2. 版本化的 `HKDF_INFO`

`b"cli-token-v1"` 里那个 `v1` 不是装饰。HKDF 的 `info` 参数会参与密钥派生，把它版本化意味着**将来换派生方案时**（比如加盐、换算法），旧密文自然解不开、可平滑迁移，而不会和新方案“撞”在同一把密钥上。这是一个很便宜的前瞻性设计。

### 3. GCM 自带完整性校验

AES-**GCM** 是 AEAD，密文尾部带认证 tag。任何对文件的篡改（哪怕翻转一个 bit）都会让解密失败，而不是悄悄解出一段垃圾。仓库里就有针对这一点的测试：

```rust
#[test]
fn decrypt_fails_on_tampered_ciphertext() {
    let mut encrypted = encrypt(b"some token data").expect("encrypt failed");
    // 翻转密文区（MAGIC + NONCE 之后）的一个字节。
    let idx = MAGIC.len() + 12;
    encrypted[idx] ^= 0xFF;
    assert!(decrypt(&encrypted).is_err(), "tampered data must not decrypt");
}
```

### 4. 原子写 + 0600 权限

落盘用的是「**先写临时文件、再 rename**」的经典原子替换，避免写到一半崩溃留下半个损坏文件；Unix 上还顺手把权限收紧到 `0600`：

```rust
let tmp = path.with_extension("tmp");
fs::write(&tmp, &encrypted)?;
fs::rename(&tmp, &path)?;   // rename 在同一文件系统上是原子的
harden_file_permissions(&path); // Unix: chmod 0600
```

### 5. 向后兼容的「读时迁移」

如果某个历史版本曾经写过明文 JSON，`load()` 通过「**开头没有 MAGIC**」识别出旧格式，解析后**顺手用加密格式重写一遍**，下次读就是密文了——用户无感升级：

```rust
if bytes.starts_with(MAGIC) {
    // 新格式：解密
} else {
    // 旧的明文 JSON：解析后再用加密格式存回去
    let token: StoredToken = serde_json::from_slice(&bytes)?;
    let _ = save(&token);
    Ok(Some(token))
}
```

> 一个务实的取舍：机器 ID 取不到时（比如极简容器）会回退成空串。加密仍然可用，只是丢掉了「机器绑定」这一层——降级而非报错，把「能用」放在「最安全」之前。

## 三、设计点 2：Bearer 作为默认请求头 + 敏感值不入日志

拿到 token 之后，怎么让**每一个** API 调用都带上它？一种写法是每个 call site 手动 `.header("Authorization", ...)`——重复且易漏。cli-terminal 的做法是：把 Bearer 设成 HTTP 客户端的**默认请求头**，构造一次，后续所有请求自动携带（`src/api/client.rs`）：

```rust
pub fn build_authenticated_client(access_token: &str) -> Result<reqwest::Client> {
    let mut bearer = HeaderValue::from_str(&format!("Bearer {access_token}"))?;
    bearer.set_sensitive(true); // 关键：让它不进 debug 日志

    let mut headers = HeaderMap::new();
    headers.insert(AUTHORIZATION, bearer);

    reqwest::Client::builder()
        .timeout(Duration::from_secs(15))
        .default_headers(headers)  // 之后每个请求都自动带上
        .build()
        .context("failed to build authenticated HTTP client")
}
```

两个点：

- **`default_headers` 把凭据「一次注入、处处生效」**，call site（如 `whoami`）就退化成一行请求，不用关心鉴权。
- **`set_sensitive(true)`** 是容易被忽略但很关键的一步：它给这个 header 打上敏感标记，reqwest 在打印 `HeaderMap` 的 `Debug` 时会用 `Sensitive` 遮盖，避免 token 意外被 `{:?}` 打进日志。安全凭据「默认不可观测」，这是个好习惯。

## 四、设计点 3：令牌桶限流（惰性回填，无后台定时器）

对开放平台友好的客户端应当**自我约束**请求速率，而不是等服务端 429 才收手。cli-terminal 内置了一个**令牌桶限流器**（`src/api/rate_limiter.rs`），最妙的是它**没有后台定时器**——回填完全由请求本身「惰性」驱动：

```rust
pub async fn acquire(&self) {
    loop {
        self.refill_tokens().await;          // 按「距上次回填过去了多久」补令牌

        if let Ok(permit) = self.semaphore.try_acquire() {
            permit.forget();                 // 消费掉：不在 drop 时归还
            return;
        }

        // 没令牌：算出下一颗令牌多久后到，睡到那时再试。
        let elapsed = Instant::now().duration_since(*self.last_refill.lock().await);
        let time_per_token = Duration::from_secs_f64(1.0 / f64::from(self.tokens_per_second));
        let wait_time = time_per_token.saturating_sub(elapsed).max(Duration::from_millis(1));
        sleep(wait_time).await;
    }
}
```

设计上的巧思：

- **用 `Semaphore` 的 permit 数当「桶里的令牌数」**。`try_acquire()` 成功即拿到一颗令牌，再 `permit.forget()` **阻止它在 drop 时被归还**——这样令牌只会被 `refill_tokens` 显式补充，永不“自愈”，桶的语义才正确。
- **惰性回填**：`refill_tokens` 根据「距上次回填的真实时间」× 速率补令牌，上限是 `max_tokens`（允许突发）。没有任何后台 tokio task 在空转，CLI 退出也没有需要清理的定时器。
- **只有真正补进令牌时才推进 `last_refill`**，避免不足一颗令牌的零头时间被反复丢弃导致长期偏慢。

在这之上，`execute()` 再叠一层**服务端 429 的指数退避重试**，把「自律」和「兜底」合二为一：

```rust
if is_rate_limit && retry_count < MAX_RETRIES {
    retry_count += 1;
    sleep(backoff).await;
    backoff *= 2;   // 1s → 2s → 4s
    continue;
}
```

即：**先用令牌桶把自己压在限额内**，万一还是撞上服务端限流，**再用指数退避退让**。客户端两个层次的速率礼貌。

## 五、设计点 4：调用前透明续期 + slow_down 退避

最后两个小而美的点，都关于「时间」。

### 1. 受保护调用前先透明续期

所有受保护请求都统一走 `request_json_body`，它的第一步永远是 `refresh_if_expired()`——**在真正发请求之前**先检查 access token 是否过期，过期就用 refresh token 悄悄换一张新的，用户完全无感：

```rust
pub async fn request_json_body(/* ... */) -> Result<serde_json::Value> {
    crate::auth::refresh_if_expired().await?;   // ① 过期就先续
    let stored = require_token()?;               // ② 此时 token 一定新鲜
    let http = build_authenticated_client(&stored.access_token)?;
    // ③ 带着新鲜 token 发请求……
}
```

判断「是否过期」时还留了 **60 秒安全余量**，避免 token 在「检查通过」到「请求真正到达服务端」这段网络时延里恰好过期：

```rust
pub fn is_expired(&self) -> bool {
    let now = /* unix now */;
    self.expires_at <= now + 60   // 提前 60s 视为过期
}
```

### 2. 轮询时遵守 slow_down（RFC 8628 §3.5）

设备码轮询 `POST /token` 时，服务端可能回 `slow_down` 要求放慢。cli-terminal 老老实实照做——每次收到就把轮询间隔**加 5 秒**（`src/auth.rs`）：

```rust
match err_resp["error"].as_str() {
    Some("authorization_pending") => {}          // 用户还没点授权，继续轮询
    Some("slow_down") => interval_ms += 5000,    // §3.5：至少加 5 秒
    Some(other) => anyhow::bail!("Authorization failed: {other}"), // 终态错误
    None => anyhow::bail!("Unexpected token poll response"),
}
```

同时每轮 sleep 都用 `interval.min(remaining)` 夹住，保证**不会睡过 `expires_in` 的截止点**——细节上不留「多睡一会导致错过窗口」的坑。

## 六、小结

四个设计点，抽象出来其实是四条通用经验：

| 设计点 | 通用经验 |
|--------|---------|
| Token 加密 + 机器绑定 | 本地凭据别裸奔；密钥绑定环境是廉价的纵深防御；格式自描述 + 版本化便于演进 |
| Bearer 默认请求头 + `set_sensitive` | 凭据「一次注入、处处生效」；敏感值默认不可观测 |
| 令牌桶 + 惰性回填 + 429 退避 | 客户端先自律再兜底；用 permit 计数省掉后台定时器 |
| 调用前透明续期 + 安全余量 + slow_down | 把「续期」藏在请求路径里；对时间边界留余量、对服务端信号照单遵守 |

它们单拎出来都不复杂，但合在一起，让一个几百行的演示 CLI 在「凭据安全」和「客户端礼貌」上都相当扎实。完整代码见 [github.com/spursy/cli-terminal](https://github.com/spursy/cli-terminal)。

## 关键源码位置索引

| 内容 | 位置 |
|------|------|
| 加密落盘 / 机器派生密钥 / 读时迁移 | [`src/token.rs`](https://github.com/spursy/cli-terminal/blob/main/src/token.rs) |
| Bearer 默认头 + `set_sensitive` | [`src/api/client.rs`](https://github.com/spursy/cli-terminal/blob/main/src/api/client.rs) |
| 令牌桶限流 + 429 退避 | [`src/api/rate_limiter.rs`](https://github.com/spursy/cli-terminal/blob/main/src/api/rate_limiter.rs) |
| 设备码流程 / 透明续期 / slow_down | [`src/auth.rs`](https://github.com/spursy/cli-terminal/blob/main/src/auth.rs) |
