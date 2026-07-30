# Rust Demos

博客文章配套的 Rust 可运行示例。每个 demo 是一个**独立的 cargo crate**，可单独 `cargo run`。

## 目录结构

```
rust/
├── README.md
└── <demo-name>/      # 一个 demo 一个 crate
    ├── Cargo.toml
    └── src/
        └── main.rs
```

## 如何运行

进入某个 demo 目录直接运行：

```bash
cd rust/<demo-name>
cargo run
```

## 新增一个 demo

在 `rust/` 目录下用 cargo 新建一个 crate：

```bash
cd rust
cargo new <demo-name>      # 生成 <demo-name>/Cargo.toml 和 src/main.rs
cd <demo-name>
cargo run
```

> 每个 demo 是彼此独立的 crate，互不干扰，可放任意多个。
> （如需统一管理，后续也可以在 `rust/` 根加一个 `Cargo.toml` 的 `[workspace]` 把各 crate 收拢为 workspace members。）

## 现有 demo

| Demo | 主题 | 对应博客 |
|------|------|---------|
| _(暂无)_ | | |
