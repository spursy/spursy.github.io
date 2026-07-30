# CLAUDE.md — Spursy's Blog

这是一个 **Zola** 静态博客工程，发布到 `https://spursy.github.io`（GitHub Pages）。
本文件给未来的 Claude 会话提供上下文，重点是**如何正确生成一篇博客文章**。

## 工程速览

- **生成器**：[Zola](https://www.getzola.org/)（Rust），版本以 CI 为准（当前 v0.22.1）。
- **主题**：`anpu`（git submodule，位于 `themes/anpu/`，**禁止手改**）。
- **部署**：推送到 `main` 分支 → GitHub Actions（`.github/workflows/deploy.yml`）自动 `zola build` 并发布。

## 目录结构

```
config.toml            # 站点全局配置（唯一入口）
content/               # ★ 所有文章 / 页面（Markdown），Claude 主要在这里写
  <slug>.md            #   一篇文章，文件名即 URL slug
  pages/<name>.md      #   独立页面（如 about）
templates/index.html   # 覆盖主题首页模板（项目模板优先级 > 主题）
themes/anpu/           # 主题子模块，勿改
sass/  static/         # 自定义样式 / 静态资源（图片放 static/）
public/                # zola build 产物，勿手动编辑
```

## ★ 写文章的规范（最重要）

### 文件位置与命名
- 普通文章：`content/<slug>.md`，`<slug>` 用**英文小写连字符**（如 `understanding-raft.md`），它会成为 URL。
- 独立页面：`content/pages/<name>.md`。

### Front-matter 格式（TOML，用 `+++` 包裹）

文章模板：

```toml
+++
title = "文章标题"
date = 2026-07-30          # 必填，YYYY-MM-DD，首页按此倒序排列
[taxonomies]              # 可选
tags = ["kubernetes", "devops"]
categories = ["后端"]
+++

这里写摘要段落（会显示在首页列表）。

<!-- more -->             # 分隔符：之前的内容成为列表页 summary

## 正文标题
正文用标准 Markdown……
```

页面模板（About 这类，无需 date）：

```toml
+++
title = "About"
description = "页面描述"
+++
```

### 硬性规则
1. **一律用 `+++`（TOML）front-matter**，不要用 `---`（YAML），保持工程一致。
2. `date` **必填**，格式 `YYYY-MM-DD`。今天的日期由会话上下文提供，不要臆造。
3. `[taxonomies]` 下只能用 **`config.toml` 已声明的分类法**：当前是 `tags` 和 `categories`。新增其它分类法前，必须先在 `config.toml` 的 `[[taxonomies]]` 里声明。
4. `<!-- more -->` 之前是摘要，可省略；正文用标准 Markdown（支持代码块高亮、表格、脚注）。
5. 图片等静态资源放 `static/`，正文用 `/图片名` 引用。
6. **不要修改 `themes/anpu/` 和 `public/`**。

### 中英文
- 正文语言随文章主题，可中文可英文。
- slug（文件名）用英文。

## 多语言（中文 / 英文）

本站已开启 Zola 多语言：**默认语言中文（`/` 无前缀），英文位于 `/en/`**。配置见 `config.toml` 的 `default_language = "zh"` 与 `[languages.en]`。

写双语文章的规则：
1. 中文版正常命名：`content/<slug>.md`。
2. 英文版加 `.en.md` 后缀、**同一个 slug**：`content/<slug>.en.md`。二者 front-matter 各自独立（`title` 用对应语言），`date` / `slug` 保持一致才能互相识别为译文。
3. section 与页面同理需成对：`content/_index.md` ↔ `content/_index.en.md`，`content/pages/about.md` ↔ `content/pages/about.en.md`。
4. 只写单语言也可以：没有 `.en.md` 的文章只出现在中文站，不影响构建。
5. 语言切换器已在 `templates/index.html` 的 `<nav>` 里实现：自动链到当前页的另一语言版本，无译文时回退到该语言首页。导航菜单（Tags/About）也会按当前语言自动加 `/en` 前缀。
6. 新增 / 修改这两处需要注意：`config.toml` 的 `[languages.en]`（含 `taxonomies`），以及 `templates/index.html` 的语言前缀逻辑。**Zola 不支持按语言配置 `[extra]`**，所以菜单的语言感知是在模板里用 `lang` 变量做的，不要试图写 `[languages.en.extra]`（会报 `unknown field extra`）。

## 本地预览 / 构建

```bash
zola serve      # 本地实时预览： http://127.0.0.1:1111
zola build      # 生成到 public/（CI 会自动做，一般无需手动）
zola check      # 校验链接和内容
```

## 已知待办 / 注意事项

- `content/` 下**缺少 `content/_index.md`**。主题约定它用于配置首页排序与分页：
  ```toml
  +++
  sort_by = "date"
  paginate_by = 10
  +++
  ```
  若首页排序 / 分页异常，补上此文件。
- 新增文章后，本地 `zola serve` 确认渲染正常再提交。
- 提交后推送 `main` 即触发自动部署，无需手动发布。
