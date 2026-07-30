# Golang Demos

博客文章配套的 Go 可运行示例。整个 `golang/` 是**一个 Go module**，示例**按来源项目分组**（如 `etcd/`），每个 demo 独占一个子文件夹，各自是 `package main`，可独立运行。

## 目录结构

```
golang/
├── go.mod                  # 整个 demos 集合共用一个 module
├── README.md
└── <project>/              # 按来源项目分组，如 etcd
    └── <demo-name>/        # 一个 demo 一个文件夹
        └── main.go         #   package main，含 main()
```

当前：

```
golang/
└── etcd/
    └── pkg-wait/           # etcd pkg/wait 提案-等待模型
        └── main.go
```

## 如何运行

在 `golang/` 目录下，用相对包路径运行任意一个 demo：

```bash
cd golang
go run ./etcd/pkg-wait          # 运行 etcd 的 pkg-wait demo
```

## 新增一个 demo

1. 若是新来源项目，先建分组文件夹 `golang/<project>/`；
2. 在其下新建 `golang/<project>/<demo-name>/`；
3. 里面放 `main.go`，声明 `package main` 并实现 `func main()`；
4. `go run ./<project>/<demo-name>` 即可执行。

> 每个 demo 子文件夹是独立的 `package main`，彼此隔离、互不冲突，所以可以放任意多个 demo。

## 现有 demo

| Demo | 主题 | 对应博客 |
|------|------|---------|
| `etcd/pkg-wait` | etcd `pkg/wait` 提案-等待模型（Register/Trigger、缓冲为 1、分片锁）| `content/etcd-pkg-wait-propose-and-wait.md` |
