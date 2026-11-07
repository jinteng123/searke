# Searke 开发计划（二期）：对齐 Sealos 常用命令

- 文档状态：批次 0-2 已实现（exec/scp 本机实跑，多节点真机待验）；批次 3-5 未开始
- 更新日期：2026-11-07
- 前置：一期计划与实现状态见 `docs/DEVELOPMENT_PLAN.md`

## 1. 背景与范围

一期已有的命令是 `run/apply/add/uninstall/status/upgrade/snapshot/login/logout/image build|push|save|load`，覆盖了“装、扩、卸”。对照 Sealos 的命令清单（`sealos --help`，官方文档 sealos.run/docs/.../commands），还缺节点缩容、远程运维原语、证书维护和一些辅助命令。

| Sealos | Searke 现状 | 二期动作 |
| --- | --- | --- |
| `run` / `apply` / `status` / `add` | 已有 | — |
| `reset` | `uninstall`（整集群） | — |
| `login` / `logout` / `push` / `save` / `load` | 已有 | — |
| `build` | `image build`（模型不同） | — |
| `delete` | **缺失** | P1：`searke delete` |
| `exec` / `scp` | 缺失 | P2：`searke exec` / `searke scp` |
| `gen` / `version` / `completion` | 缺失（completion 由 cobra 自带） | P2 |
| `pull` / `inspect` | 底层已有（`PullToLayout`/`IndexMeta`），没有 CLI | P2：`image pull` / `image inspect` |
| `cert` | 缺失 | P3：`searke cert` |
| `registry` | 缺失 | P3：`searke image serve` |
| — | Sealos 自动管理 kubeconfig | P2：`searke kubeconfig` |
| — | 离线环境节点名解析、克隆 VM 主机名重复（实测踩坑） | P2：`searke hosts sync` / `searke hostname` |

### 明确不做

`create/tag/rmi/images/merge/manifest/prune/env/docs/diff`。这些命令属于 Sealos 的“集群即 OCI 镜像、本地镜像存储”模型。Searke 的模型是“bundle + OCI Index 制品分发，不运行镜像”（一期决策 6），照搬只会增加复杂度。一期的“不做”清单继续有效：不创建 VIP/LB，不做 GUI，不支持任意发行版或 CNI。

## 2. 共用约定（所有新命令遵守）

1. **节点选择统一**：新增 `selectNodes(c, names)`，参数可以是节点名或地址，空表示全部节点；另提供 `--roles server|agent` 过滤。找不到节点时直接报错，不能静默忽略。
2. **连接统一**：一律走 `Connect()`，本机地址自动使用 `LocalExecutor`。SSH 配置从 `spec.ssh` 构造，抽成一个 helper，替换现在 status/snapshot/uninstall 里各自重复的 `SSHConfig{...}`。
3. **ctx 贯穿**：每个节点之间检查 `ctx.Err()`，保证 Ctrl-C 能中断。
4. **失败汇总**：多节点命令逐个节点执行，收集失败，最后以非零退出码返回，不在第一个节点失败时中断（与 uninstall 一致）。
5. **破坏性命令**：一律要求 `--yes`，并提供 `--dry-run`（打印计划，不碰节点、不改文件）。
6. **cluster.yaml 写入**：只在集群侧操作成功后写，使用原子写（临时文件 + rename），保留注释（复用 `descriptor.go` 的 yaml.Node 编辑方式）。
7. **敏感信息**：kubeconfig、node token、证书私钥不打印到终端或日志。落盘权限为 0600。
8. **测试**：复用 `preflight_test.go` 里的 `fakeExecutor`（按需挪到共享的测试文件），每个命令至少覆盖“正常路径 + 一个护栏”。
9. **文档**：每完成一个命令，就在 README 命令表登记，状态只写实际验证过的程度（“已实现” vs “已真机验证”）。

## 3. P1：`searke delete`（节点缩容）

```text
searke delete --agents worker-2[,10.0.0.22] -f cluster.yaml --yes
searke delete --servers server-2,server-3   -f cluster.yaml --yes
# 别名：--nodes = --agents，--masters = --servers（同 add，用 SetNormalizeFunc）
```

### 流程（逐节点执行，先 agent 后 server）

1. **预检（全部通过才开始动手）**
   - 待删节点都在 descriptor 里。
   - 能连通一个**不在删除列表里**的 server（下文称“控制 server”），并且它的 kubectl 可用。
   - server 护栏见下文。
2. `kubectl cordon` + `kubectl drain --ignore-daemonsets --delete-emptydir-data --timeout=<N>`，在控制 server 上执行（复用 `upgrade.go` 的 `drainNode`）。
3. 若删除的是 server：先确认 etcd 健康，再移除 etcd 成员（见“注意”）。
4. 在控制 server 上执行 `kubectl delete node <name>`。
5. 节点可达时，在节点上运行 killall + uninstall（复用 `uninstallNode`，带卸载后核验），并清理 `/opt/searke`。
6. 从 cluster.yaml 中删除该节点（新增 `cluster.RemoveFromDescriptor`），从 state 中删除节点记录（新增 `State.RemoveNode`），并在 history 里记录一条。

### server 护栏

- **不能删除首个 server**（`FirstServer()` = descriptor 里第一个 server）。status、snapshot、upgrade 取 token 都固定使用它。要删它需要先调整 descriptor 顺序，这一点在文档里说明，代码不自动处理。
- **不能删除 registrationAddress 指向的 server**。没有 LB 时，注册地址通常就是某台 server 的 IP；删掉它以后，后续 join 和 agent 重连都会断。
- **不能删光 server**：要删除整个集群请用 `uninstall`。
- **奇数约束（已定，方案A）**：`Validate()` 的奇数要求保持不变，`delete` 同样只允许删完后为奇数的组合（3→1、5→3 等）。替换坏节点的路径是 `add` 新节点 → 再 `delete` 旧节点，或一次删两台凑奇数。不引入 `--allow-even`。

### 注意

- **RKE2 宿主机没有 `etcdctl`**。etcd 成员移除有两种做法：
  1. 依赖 RKE2 的 etcd 控制器：删除 Node 对象后它会自动移除对应的 etcd 成员（k3s/RKE2 的成员管理行为）；
  2. 在 etcd 静态 pod 里执行 `kubectl exec ... etcdctl member remove`。
  先用做法 1，然后通过 `etcdctl member list`（在 pod 内执行）**核验**成员确实已移除，核验失败再回退到做法 2。**必须真机验证**，这是全计划风险最高的一步。
- **节点已经宕机或不可达**：`--force` 跳过 drain 和节点上的卸载，只做集群侧删除（delete node + etcd 成员）。日志里明确提示“该机器上的 RKE2 残留需人工清理”。
- **drain 卡住**（PDB、本地存储）：必须设置超时，超时后报错退出且不改 descriptor，提示用户先处理 PDB，或加 `--force` / `--skip-drain`。
- **写入顺序**：集群侧删除成功后才改 yaml。如果节点上的卸载失败，仍然从 yaml 删除该节点（它已经不在集群里了），但退出码非零，并提示手动执行 `rke2-uninstall.sh`。这样设计是为了避免 yaml 里残留一个已不在集群里的节点，导致后续 apply 卡住。
- **`searke run` 单机集群**：只有一个 server，删除它会被护栏拦住；删除 agent 时，命令在本机执行（与 add 相同的混合执行）。
- `--dry-run` 要列出：要删除的节点、使用的控制 server、是否涉及 etcd、descriptor 的 diff。

### 测试

- 单测：护栏（首个 server、注册地址节点、删光、奇数）、未知节点、descriptor 删除后保留注释、失败时文件不变。
- 真机：
  1. 删除 agent → 节点 NotFound → `searke add` 加回去 → Ready；
  2. 3-server 拓扑删除 2 台 server，etcd 成员只剩 1 个，集群仍可用。当前 VM 是 1 server + 2 agent，需要先 uninstall，再按 3 server 重新 apply。这同时补上一期未验证的“3-server HA”。

## 4. P2：运维原语与辅助命令

### 4.1 `searke exec`

```text
searke exec -f cluster.yaml [--nodes a,b] [--roles server] [--sudo] -- 'getenforce; ip route'
```
- 默认用普通用户执行，`--sudo` 时提权（与 sealos 默认 root 不同，因为这里更安全）。
- 输出按节点分段，带 `[name]` 前缀；节点退出码非零时汇总报错。
- 先串行执行，`--parallel N` 留作后续扩展（YAGNI）。
- 注意：命令原样交给远端 `sh -c`，引号由用户负责，文档里给出示例。

### 4.2 `searke scp`

```text
searke scp -f cluster.yaml [--nodes ...] ./local.conf /etc/foo/local.conf
```
- 复用 `UploadFile`：SSH + sudo 时先传到 staging 目录，再以 root 身份移动到目标位置。
- 一期只支持单个文件，目录可后续用 `TarDir` 打包、远端解包实现。
- 默认文件模式 0644，提供 `--mode` 修改。目标父目录不存在时报错，不自动创建，避免写错路径。

### 4.3 `searke hosts sync`

- 在每个节点的 `/etc/hosts` 里维护一个 `# searke-managed-begin/end` 块，内容为 `IP 节点名`。幂等：先删除旧块，再追加新块。
- 记录为 127.0.0.1 的节点（`searke run` 生成的）需要换成真实 IP。注册地址并不一定是该节点的 IP，所以这种节点要求用户用 `--address name=ip` 显式提供，否则跳过并给出警告。**不能猜**。
- 注意：**cloud-init `manage_etc_hosts: true`（阿里云 ECS 常见）会在重启时覆盖 /etc/hosts**。命令检测到 cloud-init 时需要提示这一点。
- 写之前先备份 `/etc/hosts.searke.bak`。

### 4.4 `searke hostname`

- 把每个节点的主机名设成 descriptor 里的节点名（`hostnamectl set-hostname`），解决克隆 VM 主机名重复导致 preflight 拦截的问题。
- 护栏：**节点已安装 RKE2 时拒绝执行**（改主机名会影响 kubelet 证书和节点身份），除非 `--force`。节点名不符合 DNS-1123 时拒绝。
- 应在 apply 之前使用，文档里写入推荐流程：`hostname` → `hosts sync` → `apply`。

### 4.5 `searke kubeconfig`

```text
searke kubeconfig -f cluster.yaml [-o ./kubeconfig] [--merge]
```
- 从首个 server 下载 `/etc/rancher/rke2/rke2.yaml`，把 `https://127.0.0.1:6443` 改写成 `spec.endpoints.apiAddress`。
- cluster/user/context 名称从 `default` 改成集群名，避免 `--merge` 进 `~/.kube/config` 时发生冲突。用 yaml 结构化修改，不做字符串替换。
- 输出文件权限 0600。不指定 `-o` 时写入 `~/.searke/<cluster>/kubeconfig`，**不输出到 stdout**，因为里面有 admin 客户端证书。
- apiAddress 必须在 server 证书的 SAN 里，否则 TLS 会失败。`run` 和 `apply` 已经把 apiAddress 写进 tls-san，文档里要说明这一点。

### 4.6 `searke image pull` / `image inspect`

- `image pull <ref> -o <layout-dir>`：CLI 接到现有的 `PullToLayout`，支持 `--insecure`。场景是把 ACR 上的制品摆渡到离线环境。
- `image inspect <ref|layout|.oci.tar>`：输出 RKE2 版本、CNI、平台、每个平台的 digest、层 media type（能看出 zstd 还是 gzip，排查 ACR 问题时用得上）、总大小。支持 `-o json`。

### 4.7 `searke gen` / `version` / `completion`

- `gen cluster|bundle`：输出带注释的模板。**模板只维护一份**：`examples/` 改成一个 Go 包（`examples/embed.go` 用 `//go:embed` 导出），CLI 引用它，不复制到 `internal/cli/templates`，避免两份内容漂移。
- `version`：通过 `-ldflags -X` 注入 version、commit、构建时间，并打印 Go 版本和平台。Dockerfile 与 README 的构建命令同步加上 ldflags。
- `completion`：cobra 已经自带，只需在 README 里补上用法。

## 5. P3：证书与内置 registry

### 5.1 `searke cert`

```text
searke cert check  -f cluster.yaml                 # 各 server 证书到期时间
searke cert san    -f cluster.yaml --add lb.example.com,10.0.0.200 --yes
searke cert rotate -f cluster.yaml --yes
```
- **check**：从每个 server 下载 `/var/lib/rancher/rke2/server/tls/*.crt`，**在本地用 Go `crypto/x509` 解析**（节点上不一定装了 openssl），输出 NotAfter 和 SAN。
- **san**：把新增 SAN 合并进 cluster.yaml 的 `rke2Config.tls-san`，再逐台更新各 server 的 `/etc/rancher/rke2/config.yaml` 并滚动重启 rke2-server（一次一台，等节点 Ready 后再处理下一台）。RKE2 的 dynamiclistener 会在重启后签发包含新 SAN 的证书。**必须真机验证**：用新的 SAN 地址执行 kubectl 能通过 TLS。
- **rotate**：逐台执行 `systemctl stop rke2-server` → `rke2 certificate rotate` → `systemctl start`，滚动进行，每台等 Ready。agent 侧需要重启 rke2-agent 来刷新客户端证书。
- 注意：
  1. RKE2 在证书到期前 120 天内重启时会自动续期，文档要说明“通常不需要手动 rotate”；
  2. 滚动重启时 server 数为 1 的集群会短暂不可用，执行前提示；
  3. rotate 前建议执行一次 `snapshot save`，默认自动做，`--skip-snapshot` 可关闭。

### 5.2 `searke image serve`

```text
searke image serve dist/rke2-oci --listen :5000 [--tls-cert ... --tls-key ...]
```
- 进程内的**只读** OCI Distribution 服务，直接把本地 layout 目录暴露成 registry：`GET/HEAD /v2/`、`/v2/<name>/manifests/<tag|digest>`、`/v2/<name>/blobs/<digest>`，支持 Range。
- 价值所在：当前 `apply` 的 bundle 由操作机解析后通过 SFTP 推给节点，**用不到它**。它主要服务于“多台机器各自执行 `searke run`”和“内网没有 registry、跳板机临时充当 registry”这两类场景。优先级最低，放在最后做。
- 实现优先考虑 `go-containerregistry/pkg/registry`（加一层只读包装，让写请求返回 405），不要手写协议。没有 TLS 时提示客户端使用 `--insecure`。
- 默认只监听指定地址，不做认证，文档要标注“仅限可信内网”。

## 6. 实施顺序与里程碑

依赖关系：P2 的共用设施（节点选择、SSH helper、fakeExecutor）也会被 delete 用到，所以先做这部分。

| 批次 | 内容 | 验证 | 状态 |
| --- | --- | --- | --- |
| 0 | 共用设施：`selectNodes`、`forEachNode` 聚合错误、SSH helper 收敛为 `sshConfig()`、ldflags 版本注入 | 单测 | ✅ 已实现 |
| 1 | `version` / `gen` / `image pull` / `image inspect`（`completion` 为 cobra 自带） | `inspect`/`gen`/`version` 本地实跑 | ✅ 已实现 |
| 2 | `exec` / `scp` / `hosts sync` / `hostname` / `kubeconfig` | exec/scp 本机（local executor）实跑；其余逻辑单测；**多节点真机待验** | ✅ 已实现，真机待验 |
| 3 | **`delete`**（`RemoveFromDescriptor` / `State.RemoveNode` / drain 超时 / etcd member 核验 / 方案A 奇数护栏 / 首server+registration 保护） | 守卫逻辑单测全过；**3-server 真机待验** | ✅ 已实现，真机待验 |
| 4 | `cert check/san/rotate` | 真机：新 SAN 能通过 TLS，rotate 后 NotAfter 刷新 | ⬜ 未开始 |
| 5 | `image serve` | `searke run <serve地址>/rke2:tag --insecure` 端到端 | ⬜ 未开始 |

已落地实现要点：

- `selectNodes`/`ParseNodeSelectors`/`forEachNode` 在 `internal/deploy/ops.go`；节点按 name 或 address 选中，`--nodes` 缺省为全部；单节点失败不中断，最后聚合报错。
- ops 命令用 `cluster.Load`（schema 校验）而非完整安装校验；`searke run` 生成的描述同样可用。
- `hosts sync` 用 `# searke-hosts-begin/end` 管理块幂等替换；检测到 `/etc/cloud` 下 `manage_etc_hosts` 时给出 cloud-init 覆盖警告。
- `hostname`：存在 `/var/lib/rancher/rke2` 时默认拒绝，`--force` 放行。
- `kubeconfig`：从 FirstServer 拉 `rke2.yaml`，`https://127.0.0.1:6443` 改写为 server 地址，写 `<cluster>-kubeconfig` 0600，不进 stdout。

每个批次单独提交：`go test ./...`、`go vet`、`gofmt` 全部通过，README 和本文档的状态表同步更新。

## 7. 风险与待决策

| 项 | 说明 | 处理 |
| --- | --- | --- |
| etcd 成员移除 | 依赖 RKE2 控制器自动移除，各版本行为未在本项目验证过 | 删除后必须核验 member list，失败时回退到 pod 内 etcdctl；真机验证前不标注“已验证” |
| server 奇数约束 | 3→2 被 `Validate` 禁止 | **已定（方案A）**：delete 同样只允许奇数结果；换节点走"先加后删" |
| 首个 server 绑定 | status/snapshot/upgrade 都固定使用 `FirstServer()` | delete 禁止删除首个 server；将来若要支持，需要把“控制 server”改成“任一健康 server” |
| 3-server 测试环境 | 现有 VM 只能组成 1 server + 2 agent | 批次 3 前重装为 3 server，或新建 VM |
| cloud-init 覆盖 hosts | ECS 重启后可能丢失 hosts 块 | 检测后提示，不去改 cloud-init 配置 |
| kubeconfig 泄露 | 文件里有 admin 证书 | 不输出到 stdout，权限 0600 |
| image serve 无认证 | 内网裸奔 | 文档标注，并支持 TLS |

## 8. Definition of Done

- 每个命令：实现 + 单测 + README 登记 + 本文档状态更新。
- `delete`、`cert san/rotate`、`hosts sync`、`kubeconfig` 必须在真机上验证过才能标注“已验证”。
- 破坏性命令都有 `--yes` 与 `--dry-run`，失败时 cluster.yaml 保持不变（由单测断言）。
- 不引入新的外部运行时依赖：节点上仍然不需要 Docker、Python、openssl 或 etcdctl。
