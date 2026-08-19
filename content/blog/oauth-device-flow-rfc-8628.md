+++
title = "OAuth 2.0 设备码模式（RFC 8628）到底是怎么工作的"
date = 2026-08-19
description = "从「输入受限设备怎么登录」讲起，梳理 OAuth 2.0 设备授权码模式（Device Authorization Grant, RFC 8628）的动机、与其它授权流程的区别、device_code 与 user_code 的分工，以及客户端轮询的三态收敛，并用 cli-terminal 的 Rust 代码逐段印证。"
[taxonomies]
tags = ["rust", "oauth"]
+++

`gh auth login`、`aws sso login`、在电视上登录 YouTube/Netflix——这些登录体验有一个共同点：设备本身不让你输密码，而是给你一个短码和一个网址，让你去手机或电脑的浏览器里完成授权。这背后是同一套标准：**OAuth 2.0 设备授权码模式（Device Authorization Grant, RFC 8628）**，俗称 Device Flow / 设备码模式。

这篇不讲具体某个工具，而是把这套流程本身讲清楚：它解决什么问题、和别的 OAuth 流程差在哪、每一步在传什么。文中用 [cli-terminal](https://github.com/spursy/cli-terminal)（一个演示该流程的 Rust CLI）的代码逐段印证。

<!-- more -->

## 一、它解决的问题：输入受限设备怎么登录

设想你要在这些设备上登录：命令行工具、智能电视、路由器、无头服务器、IoT 设备。它们的共同困境是 **输入受限 / 没有（好用的）浏览器**：

- 在电视遥控器上敲密码是折磨；
- CLI 里让用户输账号密码，还要处理 SSO、2FA、扫码，几乎不可能优雅；
- 无头服务器压根没有浏览器。

设备码模式的核心思路只有一句话：

> **把「授权」这个动作，从受限设备转移到用户手上那台有浏览器的设备去完成。**

受限设备只做两件很轻的事——**显示一个短码和一个网址**，然后**轮询等结果**。真正的登录（可以走 SSO、企业单点、2FA、扫码）在用户的手机/电脑浏览器里发生。

## 二、和其它 OAuth 流程比，它特殊在哪

OAuth 2.0 有好几种授权流程，选哪种取决于「谁在登录、设备有没有浏览器」：

| 流程 | 适用场景 | 设备是否碰用户密码 | 是否需要浏览器回调 |
|------|----------|----------------|--------------------|
| Authorization Code | 有浏览器的 Web / 桌面应用 | 否 | 是（需回调 URL / 端口） |
| **Device Flow（本文）** | **输入受限设备 / CLI** | **否** | **否（改用轮询）** |
| Resource Owner Password | 高度可信的第一方应用 | **是**（不推荐） | 否 |
| Client Credentials | 服务间调用（无用户参与） | N/A | 否 |

设备码模式最关键的两个特性：

1. **设备永远拿不到用户密码。** 密码只在「浏览器 ↔ 认证服务器」之间流转，设备最终只拿到一个 access token。就算设备被攻破，泄露面也仅限于这个可撤销的 token。
2. **不需要回调端口 / 公网地址。** Authorization Code 流程要在 `localhost` 起一个回调服务器接收授权结果；设备码模式改用**客户端主动轮询**，因此在防火墙、NAT 后面也能正常工作——这对 CLI 和服务器环境特别友好。

## 三、两个「码」的分工：device_code vs user_code

流程一开始，服务器会返回两个码，别搞混：

- **`user_code`**——**给人看的**。短、好念、好输入（比如 `MOCK-0042`），显示在受限设备上，用户把它输进浏览器里。
- **`device_code`**——**给机器用的**。长、不可读，是设备后续轮询 `POST /token` 时的身份凭据。用户永远不需要看到它。

再加一个 `verification_uri`（用户要打开的网址）。很多服务器还会给一个 `verification_uri_complete`——**把 user_code 拼进 URL 的查询参数里**，这样用户点开链接就不用手动输码了，体验更好。

## 四、完整流程走一遍

整套流程分「拿码」和「轮询」两个阶段。

### 阶段 1：拿码（POST /device/authorize）

设备带上自己的 `client_id` 请求授权，服务器返回四样东西：`device_code`、`verification_uri_complete`、轮询间隔 `interval`、以及有效期 `expires_in`。cli-terminal 用一个结构体承载它：

```rust
struct DeviceAuthorization {
    device_code: String,
    verification_uri_complete: String,
    interval: u64,
    expires_in: u64,
}
```

解析响应时有个小小的健壮性处理——优先用 `verification_uri_complete`（带码的完整链接），服务器没给就退回裸的 `verification_uri`：

```rust
let verification_uri_complete = resp["verification_uri_complete"]
    .as_str()
    .or_else(|| resp["verification_uri"].as_str())
    .unwrap_or_default()
    .to_owned();
```

拿到后，设备把网址显示给用户，并尽力尝试帮用户直接打开浏览器（打不开也不影响，用户手动打开即可）：

```rust
let opened = open_browser(verification_url);
println!("Open the following URL in your browser to authorize:");
println!("{verification_url}");
if opened {
    println!("Browser opened. Waiting for authorization...");
} else {
    println!("Waiting for authorization...");
}
```

### 阶段 2：轮询（POST /token）

现在设备进入等待：每隔 `interval` 秒，带着 `device_code` 去问服务器「授权好了吗」。请求的关键是那个又长又标准的 `grant_type`：

```rust
let raw = http_client
    .post(&url)
    .form(&[
        ("client_id", client_id),
        ("grant_type", "urn:ietf:params:oauth:grant-type:device_code"),
        ("device_code", device_code),
    ])
    .send()
    .await?;
```

与此同时，用户在浏览器里登录、完成 2FA、点下「授权」。这一切设备都看不见，它只是在轮询。

## 五、轮询的三态收敛：这是设备码模式的灵魂

轮询不是「傻等成功」。每次 `POST /token` 的回应，会把状态机推向三类结局之一。理解这三态，就理解了 RFC 8628 的客户端该怎么写：

| 服务器返回 | 含义 | 客户端动作 |
|------------|------|-----------|
| HTTP 2xx（带 token） | 用户已授权 | 解析并保存 token，**成功结束** |
| `authorization_pending` | 用户还没点授权 | **继续**按 interval 轮询 |
| `slow_down` | 你轮询太快了 | 间隔**至少 +5 秒**（§3.5），再继续 |
| `access_denied` / `expired_token` / … | 用户拒绝 / 码过期等 | **立即终止**报错 |

对应到代码，就是一个 `match` 把「继续」和「终止」两类分得清清楚楚：

```rust
let err_resp = raw.json::<serde_json::Value>().await.unwrap_or_default();
match err_resp["error"].as_str() {
    // 用户还没完成授权 —— 继续轮询
    Some("authorization_pending") => {}
    // RFC 8628 §3.5：轮询太快，间隔至少加 5 秒
    Some("slow_down") => interval_ms += 5000,
    // 其它错误码（access_denied、expired_token…）都是终态
    Some(other) => anyhow::bail!("Authorization failed: {other}"),
    None => anyhow::bail!("Unexpected token poll response"),
}
```

三个要点：

1. **`authorization_pending` 是常态，不是错误。** 它对应一个空的 match 分支——什么都不做，让循环自然进入下一轮。轮询期间绝大多数回应都是它。
2. **`slow_down` 是服务端的限流信号，必须照单遵守。** RFC 规定收到后间隔至少加 5 秒（`interval_ms += 5000`）。这是「客户端要对服务端有礼貌」的协议级约定。
3. **其它错误码是终态，要立即冒泡。** 用户拒绝了（`access_denied`）、码过期了（`expired_token`）就没必要再轮询，直接报错退出。

## 六、别忘了那条截止线：expires_in

`device_code` 不是永久有效的——服务器给了 `expires_in`（cli-terminal 里默认兜底 300 秒）。轮询循环必须尊重这条截止线，否则就是无限等待。

cli-terminal 的处理很讲究：先算出一个绝对的 `deadline`，每轮循环检查剩余时间；而且**每次 sleep 都用 `min(remaining)` 夹住**，保证不会「多睡一会儿而错过截止点」：

```rust
let deadline = Instant::now() + Duration::from_secs(expires_in);
loop {
    let remaining = deadline.saturating_duration_since(Instant::now());
    if remaining.is_zero() {
        anyhow::bail!("Device authorization timed out");
    }
    tokio::time::sleep(Duration::from_millis(interval_ms).min(remaining)).await;
    // ... POST /token，然后按上一节的三态处理
}
```

`saturating_duration_since` 顺手避免了时间倒流时的下溢——都是些不显眼但能让轮询循环真正健壮的细节。

## 七、成功之后

某一轮轮询终于拿到 HTTP 2xx，响应里带着 `access_token`，通常还有 `refresh_token` 和 `expires_in`。设备把它们连同一个**绝对过期时间戳**一起存下来：

```rust
let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs();
return Ok(StoredToken {
    client_id: client_id.to_string(),
    access_token: access_token.to_string(),
    refresh_token,
    expires_at: now + expires_in,   // 存绝对时间，之后判断是否过期只需和 now 比
});
```

至此，登录完成。之后所有受保护的调用都带上这个 access token；等它快过期时，再用 refresh_token 静默续期，用户不必重新登录。

## 八、小结

设备码模式的精髓，其实就三句话：

- **把授权动作挪到有浏览器的设备上**，受限设备只显示「短码 + 网址」并轮询等结果；
- **两个码各司其职**：`user_code` 给人输，`device_code` 给机器轮询；
- **轮询是一个三态收敛的状态机**：`authorization_pending` 继续、`slow_down` 退避、终态错误立即退出，外加一条 `expires_in` 截止线兜底。

正因为它不碰用户密码、不需要回调端口、还能复用浏览器完整的登录能力，才成了 CLI、电视、IoT 登录的事实标准。想看一份可以本地端到端跑通的最小实现，见 [github.com/spursy/cli-terminal](https://github.com/spursy/cli-terminal)。

## 关键源码位置索引

| 内容 | 位置 |
|------|------|
| 设备授权请求 / 轮询三态 / expires_in 截止 | [`src/auth.rs`](https://github.com/spursy/cli-terminal/blob/main/src/auth.rs) |
| 可本地跑的最小 mock OAuth 服务器（含 /device/authorize、/token） | [`src/bin/oauth/authorization.rs`](https://github.com/spursy/cli-terminal/blob/main/src/bin/oauth/authorization.rs) |
