# docker-kit

[![CI](https://github.com/soulteary/docker-kit/actions/workflows/ci.yml/badge.svg)](https://github.com/soulteary/docker-kit/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/soulteary/docker-kit.svg)](https://pkg.go.dev/github.com/soulteary/docker-kit)

用 Go 驱动 docker CLI：执行命令、读出容器**实际**是按什么参数创建的，并报告它与当前配置之间的差异。零依赖。

**文档:** [English](README.md) · 中文

## 它要解决的问题

容器的镜像、网络、挂载、环境变量、附加组，全都在 `docker create` 那一刻定死。之后什么都改不了它们 —— `docker start` 只是把已经存在的那个容器再拉起来。

于是改配置**看起来**生效了：配置文件写着新值，界面提示已保存，容器却继续按旧值运行。而且没有任何地方报告这个不一致。

促成本包的那次故障：配置早就改成不挂宿主机 docker socket 了，容器还挂着。改配置想要的隔离根本没发生，而一切看上去都正常。

```go
facts, _ := dockerkit.Inspect(ctx, "app")
if diffs := spec.Drift(facts, imageID); len(diffs) > 0 {
    log.Printf("容器 %s 是按旧配置创建的: %s", spec.Name, dockerkit.DiffsString(diffs))
    // → container_image: app:v1 → app:v2; mount /data: /srv/old → /srv/new
}
```

## 安装

```bash
go get github.com/soulteary/docker-kit
```

## 一份 Spec，两个方向

`Spec` 同时是「按它创建容器」和「拿它比对容器」的唯一定义：

```go
spec := dockerkit.Spec{
    Name:    "app",
    Image:   "app:v2",
    Network: "app-net",
    Binds:   []string{"/srv/data:/data"},
    Env:     map[string]string{"MODE": "prod"},
    EnvSet:  map[string]string{"TOKEN": secret},
    Limits:  dockerkit.Limits{CPUs: "2", Memory: "4g"},
}

args, err := spec.CreateArgs()          // docker create …
diffs := spec.Drift(facts, imageID)     // 哪里不再匹配
```

合在一处才是关键。两边各写一套的话，改了创建逻辑、比对还停在旧写法上 —— 它本该发现的漂移，恰恰发现不了。

`CreateArgs` 是确定性的：map 按 key 排序输出。调用方会把这些命令记进日志、做 diff、粘进 shell 执行；参数顺序每次都变的话，这三件事全都做不了。

## `Env` 与 `EnvSet` 的区别

`Env` 比对取值，`EnvSet` **只比对「有没有」** —— 这个区分正是它存在的理由。

创建时注入的密钥（比如容器用来鉴权的令牌）不该按取值比对：轮换密钥是运行期的事，容器会以「鉴权失败」把它暴露出来；把取值不一致当成漂移，等于为了注入一个它反正会拿到的值，去删掉一个正在正常工作的容器。

但**「没有」是另一回事**。在密钥这个特性之前创建的容器根本没有这个值，而读到空值的代码通常会退化成「不鉴权」—— 静默地退化，没有任何失败能暴露它。这种才需要重建。取值为空等同于没有，因为消费方就是这么读的，而且镜像自己也可能带一句 `ENV TOKEN=`。

## 为什么把网络写成 label

「它连在正确的网络上吗」是个错误的问题。

`docker create --network` 只设一个网络，但容器事后可以被 `docker network connect` 接进别的网络。当配置从 `net-a` 改成 `net-b`、而容器恰好两个都连着时，「任一命中」的检查会放行 —— 于是它继续连着 `net-a`，恰恰是这次改配置想断掉的那条。

所以 `CreateArgs` 把网络记成 label，`Drift` 读 label。`NetworkMode` 是给 label 出现之前创建的容器兜底的；随之而来的重建会写上 label，因此这条路径最多走一次。

## 输出分类

「容器不存在」与「连不上 daemon」共用同一个非零退出码，但两者的处理方式完全相反 —— 前者通常没事，后者从来都有事。

| 函数 | 含义 |
|---|---|
| `NotFound(out)` | 没有这个容器（覆盖多语言输出） |
| `PermissionDenied(out)` | 连不上 daemon：socket 权限，或根本没在监听 |
| `UnrecoverableStart(out)` | `docker start` 失败且重试永远修不好 —— 多为 `compose down` 删掉了网络。必须删掉重建；一味重试的调用方会永远重试下去 |
| `Error(op, out, err)` | 把失败**连同输出**一起包装，必要时附上 daemon 访问提示 |

`Error` 一定带上输出：只说 `exit status 1` 的错误，会让读它的人多跑一趟宿主机去看 docker 究竟说了什么。

## 资源上限

```go
l := dockerkit.Limits{CPUs: "2", Memory: "4g", MemorySwap: "8g", PidsLimit: 512}
l.Validate()          // 把 docker 只在 create 时才报的错提前
l.Args()              // --cpus 2 --memory 4g …
l.UpdateArgs("app")   // docker update … app
```

`Validate` 值得在读配置时就跑：docker 自己的拒绝发生在 `docker create` 那一刻，那时错误信息已经完全看不出是哪个配置项引起的。它能挡住 `1e3`、`NaN`（`ParseFloat` 接受、docker 不接受），以及对 `memory-swap` 最常见的误读 —— 那个值是**内存 + swap 的总量**，所以它永远不可能小于 memory。

`UpdateArgs` 存在是因为 `Args` 只在创建时生效。升级后才加上限的部署，存量容器拿不到任何好处，停止再启动也没用。`docker update` 能把它们施加到已经存在的容器上，不必重建。

## 串行化生命周期操作

```go
var locks dockerkit.Locks

func start(name string) {
    defer locks.Lock(name)()
    // …
}
```

一个守护程序往往有好几条路径会启动同一个容器：启动时的扫描、定时巡检、界面点击。一旦「重建」意味着先 `docker rm` 再 `docker create`，两条路径交叉执行就可能删掉对方刚建好的容器。撞名字只是日志里的一条抱怨；这是一个**静默消失**的容器。

## 缓存镜像 ID

```go
cache := &dockerkit.ImageIDCache{}   // 默认 10s
id := cache.Get(ctx, "app:v2")
```

列出 N 个容器的状态页，它们通常共用同一个镜像；不缓存的话每刷新一次就多出 N 个 `docker image inspect` 进程。

**不要在启停路径上用它。**「刚重新构建完镜像，然后点启动」是很常见的操作，那里读到缓存的旧 ID，意味着这次构建被静默忽略 —— 恰恰是漂移检查要抓的那个 bug。启停时直接用 `ImageID`。

## 测试

`Runner.Exec` 可以替换掉进程执行，于是测试不需要 daemon：

```go
r := dockerkit.Runner{Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
    return []byte(`[{"State":{"Running":true}}]`), nil
}}
facts, err := r.Inspect(ctx, "app")
```

`Runner.Binary` 还能把本包指向兼容的 CLI，比如 `podman`。

## 许可证

Apache 2.0，见 [LICENSE](LICENSE)。
