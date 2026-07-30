# CLAUDE.md — Spursy's Blog

这是一个 **Zola** 静态博客工程，发布到 `https://spursy.github.io`（GitHub Pages）。
本文件给未来的 Claude 会话提供上下文，重点是**如何正确生成一篇博客文章**。

## 工程速览

- **生成器**：[Zola](https://www.getzola.org/)（Rust），当前本地 **v0.22.1**（CI 以 workflow 为准）。
- **主题**：[`tabi`](https://github.com/welpo/tabi)（v4.1.0，位于 `themes/tabi/`）。**普通文件**（非 submodule），可读勿随意改；定制走项目侧覆盖。
- **部署**：推送到 `main` 分支 → GitHub Actions（`.github/workflows/deploy.yml`）自动 `zola build` 并发布。

## 目录结构

```
config.toml              # 站点全局配置（唯一入口，tabi 大量选项在 [extra]）
content/                 # ★ 所有文章 / 页面（Markdown）
  _index.md              #   首页 landing（tabi：header + 拉取 blog 最新文章）
  blog/                  #   ★ 文章都放这里
    _index.md            #     blog section 配置（sort_by / paginate_by）
    <slug>.md            #     一篇文章
  pages/                 #   独立页面（about 等），_index.md 设 render=false
    about.md
themes/tabi/             # 主题文件，勿随意改
sass/  static/           # 自定义样式 / 静态资源（图片放 static/）
public/                  # zola build 产物，勿手动编辑
```

## ★ 写文章的规范（最重要）

### 文件位置与命名
- 普通文章：**`content/blog/<slug>.md`**，`<slug>` 用**英文小写连字符**（如 `understanding-raft.md`）。
- 独立页面：`content/pages/<name>.md`，front-matter 加 `template = "info-page.html"`。

### Front-matter 格式（TOML，用 `+++` 包裹）

文章模板：

```toml
+++
title = "文章标题"
date = 2026-07-30          # 必填，YYYY-MM-DD，倒序排列
description = "一句话摘要"  # 可选，用于列表页与社交卡片
[taxonomies]
tags = ["etcd"]           # 只有 tags（见 config）
[extra]                   # 可选，tabi 单篇开关
toc = true                # 目录
# katex = true            # 公式
# mermaid = true          # 图表
+++

正文用标准 Markdown……
```

独立页面模板（about 这类）：

```toml
+++
title = "关于"
description = "关于我"
template = "info-page.html"
+++
```

### 硬性规则
1. **一律用 `+++`（TOML）front-matter**，不要用 `---`（YAML）。
2. `date` **必填**，格式 `YYYY-MM-DD`。今天的日期由会话上下文提供，不要臆造。
3. `[taxonomies]` 只有 **`tags`**（在 `config.toml` 声明）。新增分类法要先在 config 里加。
4. 文章一律放 **`content/blog/`**；正文用标准 Markdown（代码块高亮 / 表格 / 脚注）。
5. 图片等静态资源放 `static/`，正文用 `/图片名` 引用。
6. **不要改 `themes/tabi/` 和 `public/`**；定制走 config `[extra]` 或 `static/` 额外样式表。

### 中英文
- 正文语言随主题，可中可英；slug（文件名）用英文。

## 多语言（中文 / 英文）

本站开启 Zola 多语言：**默认语言 `zh-Hans`（`/` 无前缀），英文位于 `/en/`**。
> ⚠️ 必须用 **`zh-Hans`** 而非 `zh`——tabi 的 i18n 只提供 `zh-Hans` / `zh-Hant`，用 `zh` 界面会退回英文。

写双语的规则：
1. 中文版正常命名：`content/blog/<slug>.md`。
2. 英文版加 `.en.md` 后缀、**同一个 slug**：`content/blog/<slug>.en.md`。`date` / `slug` 一致才互认为译文。
3. **每个 section 都要成对**：`content/_index.md` ↔ `_index.en.md`、`content/blog/_index.md` ↔ `_index.en.md`、`content/pages/_index.md` ↔ `_index.en.md`。**缺任一语言的 `_index.<lang>.md` 会导致 `get_section` 构建报错**。
4. 首页 landing 的 `[extra].section_path` 要指向**对应语言**的 blog 索引：中文 `_index.md` 写 `section_path = "blog/_index.md"`，英文 `_index.en.md` 写 `section_path = "blog/_index.en.md"`。
5. 语言切换器由 tabi 自带（有译文则互链，无则回退首页）。

## tabi 关键配置（config.toml）

- `default_language = "zh-Hans"`；英文在 `[languages.en]`。
- `build_search_index = false`（顶层）——**Zola 的 elasticlunr 不支持中文分词**，默认语言开搜索会构建失败；英文可在 `[languages.en]` 单独开。
- 代码高亮：`[markdown.highlighting] style = "class"`（+ `theme = "catppuccin-mocha"` 仅为满足 enum，class 模式下调色板不生效）。Zola 输出 `.z-*` 类名，由 tabi 的 `_syntax_theme.scss` 着色（随皮肤/明暗自适应、含圆角与复制按钮）。class 模式是 CSP-safe，**不要**再设 `enable_csp = false`。
- 常用 `[extra]`：`skin`（配色皮肤 teal/blue/lavender/...）、`theme_switcher`、`copy_button`、`show_reading_time`、`menu`、`socials`、`favicon_emoji`。
- 菜单 `menu` 里的 `name` 是 i18n 键（`blog`/`tags`/`about`…），会按语言自动翻译成 博客/标签/关于。

## 本地预览 / 构建

```bash
zola serve      # 本地实时预览： http://127.0.0.1:1111
zola build      # 生成到 public/（CI 会自动做）
zola check      # 校验链接和内容
```

## 注意事项

- 新增文章后，本地 `zola serve` 确认渲染正常再提交。
- 提交后推送 `main` 即触发自动部署，无需手动发布。
- tabi 更多选项见官方文档：<https://welpo.github.io/tabi/blog/mastering-tabi-settings/>。
