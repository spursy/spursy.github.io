+++
title = "用 Rust 实现 OIDC 单点登录：从鉴权原理到 CSRF / SSRF 防御"
date = 2026-10-15
description = "从「什么是 OIDC」讲起，梳理 OIDC 如何基于 OAuth 2.0 授权码流程实现单点登录（SSO），配合时序图说明 discovery、state、code→token、userinfo 四步鉴权；再结合 oidc-poc 的 Rust 代码逐段拆解 CSRF 与 SSRF 两类漏洞的成因、攻击路径与防御手段，最后总结 axum 实现 RESTful 接口的代码特点。"
[taxonomies]
tags = ["oidc", "sso", "rust", "csrf", "security"]
+++

每次点「使用 Google 登录」「用企业账号登录」，你都在走同一套标准：**OpenID Connect（OIDC）**。它把「我是谁」这件事从每个应用自己维护，收敛到一个统一的身份提供方（IdP），这就是单点登录（SSO）的底座。

本文用一个最小示例工程 [oidc-poc](https://github.com/spursy/oidc-poc)（Rust + axum + React）把整条链路讲清楚：OIDC 是什么、它怎么实现 SSO、代码里是如何防住 CSRF 与 SSRF 的，以及用 Rust 写这类 RESTful 接口有什么特点。

## 一、什么是 OIDC

OIDC 是架在 **OAuth 2.0** 之上的一层**身份认证**协议。这里要先分清两件常被混为一谈的事：

- **OAuth 2.0 解决「授权」(Authorization)**：让 A 应用在你同意下，拿到访问 B 资源的**令牌**。它本身不关心「你是谁」。
- **OIDC 解决「认证」(Authentication)**：在 OAuth 2.0 的授权码流程之上，增加一个标准化的 `id_token`（JWT）和 `userinfo` 端点，让应用能可信地知道**登录者的身份**（email、sub 等）。

一句话：**OAuth 管「能不能拿」，OIDC 管「你是谁」。** OIDC = OAuth 2.0 授权码流程 + 身份层。

OIDC 定义了三个关键端点，它们都能从一份 **discovery 文档**里发现：

| 端点 | 作用 |
| ---- | ---- |
| `authorization_endpoint` | 用户被重定向到这里登录 + 授权同意 |
| `token_endpoint` | 后端用一次性 `code` 换 `access_token` / `id_token` |
| `userinfo_endpoint` | 用 `access_token` 换取用户身份信息（email 等） |

discovery 文档固定位于 `{issuer}/.well-known/openid-configuration`。这意味着应用**不需要硬编码任何端点**，只要配一个 `OIDC_ISSUER`，端点在运行时自动发现。换 IdP（比如从自建 Keycloak 换成别家）是一次配置变更，而非代码变更。oidc-poc 里就是这么做的：

```rust
#[derive(Clone, Deserialize)]
struct OidcMetadata {
    authorization_endpoint: String,
    token_endpoint: String,
    userinfo_endpoint: String,
}

async fn oidc_metadata(state: &AppState, issuer: &str) -> Option<OidcMetadata> {
    if let Some(meta) = state.oidc_meta.lock().unwrap().clone() {
        return Some(meta); // 命中缓存
    }
    let url = format!(
        "{}/.well-known/openid-configuration",
        issuer.trim_end_matches('/')
    );
    let resp = reqwest::Client::new().get(&url).send().await.ok()?;
    // ...解析并缓存到 AppState
}
```

## 二、OIDC 如何实现 SSO

SSO 的本质是：**多个应用共享同一个 IdP 的会话**。你在 IdP 上登录过一次，之后每个接入的应用只要把你重定向到 IdP，IdP 发现你已有会话，就直接带着身份跳回来，无需再次输入密码。

oidc-poc 实现的是标准的 **Authorization Code Flow（授权码流程）**，整条鉴权链路分四步：

```text
浏览器(Browser)          oidc-poc(后端)            IdP(Keycloak)
     │                        │                          │
     │ ① GET /login/oidc      │                          │
     ├───────────────────────>│                          │
     │                        │ 读 discovery(缓存)        │
     │                        │ 生成 state、种 cookie      │
     │ ② 302 到 authorize + Set-Cookie(state)            │
     │<───────────────────────┤                          │
     │                        │                          │
     │ ③ 跟随 302(携带 IdP 已有会话)                       │
     ├──────────────────────────────────────────────────>│
     │                        │          用户登录/已登录   │
     │                        │          + 同意授权        │
     │ ④ 302 回 /login/oidc/callback?code=&state=         │
     │<──────────────────────────────────────────────────┤
     │ ⑤ GET /callback(浏览器自动带上 state cookie)        │
     ├───────────────────────>│                          │
     │                        │ ⑥ 校验 state == cookie    │
     │                        │ ⑦ code 换 token ─────────>│
     │                        │   (后端直连,带 client_secret)
     │                        │<──────────── access_token │
     │                        │ ⑧ access_token 换 userinfo>│
     │                        │<──────────────────── email│
     │ ⑨ 302 /login?signed_in=<email> + 清除 cookie       │
     │<───────────────────────┤                          │
```

四步鉴权的信任链是这样闭合的：

1. **IdP 已认证用户** → 才会发 `code`；
2. **`code` 只能被持有 `client_secret` 的后端换成 token** → 证明换令牌的是合法 client，且 `code` 一次性、短时效，即使泄露也难以复用；
3. **`state == cookie`** → 证明这次回调绑定的是发起请求的那个浏览器（见下节 CSRF）；
4. **`userinfo` 返回的 email** → 是后端自己去 IdP 问来的可信身份，而非 URL 里的任何明文。

四者齐备，才认定登录有效。关键安全点：**oidc-poc 从不把任何未经校验的 URL 参数当作身份**——`email` 永远来自后端对 `userinfo_endpoint` 的调用。

## 三、CSRF：跨站请求伪造

### 它是什么

CSRF（Cross-Site Request Forgery）利用的是**浏览器会自动携带 cookie** 这一行为。当你已登录 A 站，攻击者诱导你访问一个恶意页面，该页面向 A 站发起请求，浏览器会自动带上你在 A 站的 cookie，于是请求「看起来」是你本人发起的。

在 OIDC 场景里，有一种特殊变体叫 **Login CSRF（登录 CSRF）**：攻击者不偷你的身份，而是**把你悄悄登录成攻击者的账号**。

### 攻击流程

1. 攻击者在 IdP 上正常走到授权码流程的中途，拿到一个**属于攻击者账号的 `code`**；
2. 攻击者构造一个链接：`https://victim-app/login/oidc/callback?code=<攻击者的code>`；
3. 诱导受害者点击；
4. 如果回调端点**不校验这次回调是否由本人发起**，受害者的浏览器就会用攻击者的 `code` 完成登录——此后受害者在该应用里的操作（上传文件、保存笔记、绑定支付）全都进了攻击者的账号，攻击者事后登入即可窃取。

### oidc-poc 如何防御

核心武器是 OAuth 2.0 的 `state` 参数，oidc-poc 把它和一个**浏览器 cookie 绑定**：

**发起时**（`/login/oidc`）生成随机 `state`，同时种进 cookie：

```rust
fn new_state() -> String {
    let mut bytes = [0u8; 16];
    rand::rngs::OsRng.fill_bytes(&mut bytes); // 128 位,来自操作系统 CSPRNG
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

fn set_state_cookie(state: &str) -> String {
    let secure = if oidc_redirect_uri().starts_with("https") { "; Secure" } else { "" };
    format!(
        "{OIDC_STATE_COOKIE}={state}; Path=/login/oidc; HttpOnly; SameSite=Lax; Max-Age=600{secure}"
    )
}
```

**回调时**（`/login/oidc/callback`）校验 URL 里的 `state` 与 cookie 里的 `state` 完全相等：

```rust
let csrf = query.state.unwrap_or_default();
let cookie_state = cookie_value(&headers, OIDC_STATE_COOKIE).unwrap_or_default();
if csrf.is_empty() || csrf != cookie_state {
    println!("/login/oidc/callback -> unknown or missing state; rejecting");
    return Redirect::to("/login?error=oidc_state").into_response();
}
```

为什么这能防住 Login CSRF？攻击者伪造的回调链接里，`state` 是攻击者自己那次流程的值；而受害者浏览器里的 `oidc_state` cookie 要么**没有**（受害者从没发起过），要么是**另一个值**。两者对不上，回调被直接拒绝。**攻击者无法替受害者的浏览器种下匹配的 cookie**，这是防御成立的根本。

cookie 的几个属性都在为安全服务：

| 属性 | 作用 |
| ---- | ---- |
| `HttpOnly` | 页面 JS 读不到,挡住 XSS 窃取 state |
| `SameSite=Lax` | 既挡住大部分跨站自动提交,又能在从 IdP 跳回的顶级 GET 导航里保留 cookie |
| `Path=/login/oidc` | 只在回调路径发送,缩小暴露面 |
| `Max-Age=600` | 10 分钟过期,未完成的登录不会长期有效 |
| `Secure`(HTTPS 时) | 只走加密信道 |

还有一个额外好处:把 state 放 cookie 而非服务端内存，使校验**无状态**——多副本部署、Pod 重启都不影响，任何一个实例都能校验任何一次回调。（这正是从内存 `HashSet` 迁移到 cookie 的直接动因。）

## 四、SSRF：服务端请求伪造

### 它是什么

SSRF（Server-Side Request Forgery）是让**服务器**替攻击者去发请求。危险在于：服务器往往位于内网，能访问外部打不到的地址——云厂商元数据服务（`169.254.169.254`，可拿到临时凭证）、内部管理后台、数据库端口等。一旦攻击者能控制「服务器去请求哪个 URL」，就可能以服务器的身份横向渗透。

### 攻击流程

1. 应用有一处「根据参数去拉取某个 URL」的逻辑；
2. 攻击者把该参数指向内网地址，例如 `http://169.254.169.254/latest/meta-data/iam/security-credentials/`；
3. 服务器照单发起请求，把内网响应（如云凭证）返回或落库；
4. 攻击者据此拿到凭证，进一步攻击。

在 OIDC 里，天然存在一处「服务端拉取 URL」的逻辑——**discovery 请求**：后端会去 `GET {issuer}/.well-known/openid-configuration`。如果 `issuer` 可被攻击者控制，这就是一个标准的 SSRF 入口。

### oidc-poc 如何防御

oidc-poc 的防御是**从设计上消除攻击面**：`issuer` **不来自任何请求参数，只来自可信的环境变量** `OIDC_ISSUER`：

```rust
fn oidc_issuer() -> Option<String> {
    std::env::var("OIDC_ISSUER").ok().filter(|s| !s.is_empty())
}
```

发起和回调两处用到 discovery 的地方，`issuer` 全程取自这个函数，用户无从注入：

```rust
let issuer = match oidc_issuer() {   // 来自 env,不是来自 query / body
    Some(i) => i,
    None => return Redirect::to("/login?error=oidc_unconfigured").into_response(),
};
let meta = match oidc_metadata(&state, &issuer).await { /* ... */ };
```

这就是防 SSRF 最稳的一招:**不让用户输入参与到「服务器请求哪个地址」的决策中**。配置项由运维在部署时设定，属于可信边界内。

如果某些业务确实需要让用户/租户传入 issuer（多租户 SSO），则必须叠加纵深防御：

- **白名单**：只允许预先登记的 issuer 域名；
- **协议与地址校验**：强制 `https`，解析出 IP 后拒绝私网段（`10/8`、`172.16/12`、`192.168/16`、`127/8`、`169.254/16`、`::1` 等）；
- **禁用重定向跟随**：避免 `https://evil → 302 → http://169.254.169.254` 的绕过；
- **出网代理 / egress 策略**：从网络层限制服务器能访问的目的地。

oidc-poc 因为把 issuer 锁死在配置里，直接站在了最安全的那一侧。

## 五、oidc-poc 中 Rust 实现 RESTful 接口的代码特点

把 OIDC 这条链路落到 axum 上，有几个值得说的工程特点：

**1. handler 即异步函数，用「提取器」声明依赖。** axum 的路由把 URL 映射到 `async fn`，函数签名里写什么参数，框架就自动从请求里提取什么。oidc-poc 的回调函数一次用了三个提取器：

```rust
async fn oidc_callback(
    State(state): State<AppState>,   // 共享状态(discovery 缓存)
    headers: HeaderMap,              // 原始请求头(读 cookie)
    Query(query): Query<OidcCallbackQuery>, // 解析查询串为强类型
) -> Response { /* ... */ }
```

提取器的顺序约定很关键：像 `State`、`Query`、`HeaderMap` 这类不消费请求体的，实现了 `FromRequestParts`，可以有多个、顺序随意；而消费请求体的（如 `Json<T>`）实现 `FromRequest`，只能有一个且必须放最后。

**2. 用 `#[derive(Deserialize)]` 把请求/响应「类型化」。** 查询串、token 响应、userinfo 响应都是声明一个 struct，serde 负责反序列化，缺字段用 `Option` 兜底，多余字段自动忽略。代码里只声明「我关心的字段」：

```rust
#[derive(Deserialize)]
struct OidcCallbackQuery { code: Option<String>, state: Option<String>, error: Option<String> }

#[derive(Deserialize)]
struct OidcTokenResponse { access_token: String }

#[derive(Deserialize)]
struct OidcUserinfo { email: Option<String> }
```

**3. 共享状态走 `State<AppState>` + `Arc<Mutex<…>>`。** discovery 文档拉一次就缓存，`AppState` 内部用 `Arc` 让多个请求共享同一份、`Mutex` 保证并发安全。`#[derive(Clone)]` 让 axum 能把它注入每个 handler——clone 的只是 `Arc` 指针，不是数据本身：

```rust
#[derive(Clone, Default)]
pub struct AppState {
    oidc_meta: Arc<Mutex<Option<OidcMetadata>>>,
}
```

**4. 统一返回 `Response`，把「错误」表达为「重定向」。** 这是一个 Web 登录流而非 JSON API，所以所有分支——成功、各类失败——都返回 `Response`，失败时 302 回 `/login?error=<code>`，前端再把 error code 映射成本地化提示。错误不是抛异常，而是流程的一个正常出口：

```rust
return Redirect::to("/login?error=oidc_state").into_response();
```

**5. 自包含、少依赖。** 连 URL 编码都是手写一个最小实现（只放行 RFC 3986 unreserved 字符），避免为一个小功能引入额外依赖；前端 SPA 则通过 `rust-embed` 在编译期嵌入二进制，部署就是一个文件。整体风格是「够用、可控、无魔法」。

## 六、小结

- **OIDC = OAuth 2.0 授权码流程 + 身份层**：OAuth 管授权，OIDC 管认证。
- **SSO 的实现**靠共享 IdP 会话 + discovery 驱动的授权码流程：discovery → state → code→token → userinfo，四步闭合信任链，身份只认 `userinfo`。
- **CSRF（含 Login CSRF）** 用「`state` 绑定浏览器 cookie」防御，攻击者无法为受害者种下匹配 cookie；cookie 放 state 还顺带换来无状态、可水平扩展。
- **SSRF** 从设计上消除：issuer 只来自可信环境变量，不让用户输入决定「服务器请求谁」；若需用户传入则叠加白名单 + 私网拦截 + 禁跳转。
- **Rust/axum 的 RESTful 代码特点**：提取器声明依赖、serde 类型化、`State + Arc<Mutex>` 共享状态、错误即重定向、自包含少依赖。

完整代码见 [github.com/spursy/oidc-poc](https://github.com/spursy/oidc-poc)。
