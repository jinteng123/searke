# searke

`searke` 是一个 Go 单文件 CLI，用于离线（air-gap）场景下的 RKE2 多架构集群部署：
下载上游制品、校验、打包为多架构 OCI 镜像，并通过 SSH + YAML 驱动安装，
不依赖 Docker daemon、Ansible 或 Python。安装后的集群运维（扩缩容、滚动升级、
etcd 快照、证书、批量执行）也由同一个二进制完成，命令风格对齐 Sealos。

- 目标节点零额外依赖：不需要 Docker、Python、openssl、etcdctl、curl
- 支持 `linux/amd64` + `linux/arm64`，一个 OCI Image Index 覆盖混合架构集群
- 制品来源：OCI layout 目录、`.oci.tar` 归档、registry 引用三选一
- 所有安装/运维步骤可断点续跑、可幂等重跑；破坏性操作需 `--yes`，大多支持 `--dry-run`

详细设计见 [docs/DEVELOPMENT_PLAN.md](docs/DEVELOPMENT_PLAN.md)，Sealos 对齐计划见
[docs/SEALOS_PARITY_PLAN.md](docs/SEALOS_PARITY_PLAN.md)。

## 构建

```bash
go build -o searke ./cmd/searke
```

### Docker 镜像

```bash
docker build -t searke .
# 多架构（需要 buildx）：
docker buildx build --platform linux/amd64,linux/arm64 -t searke .
# Go 模块代理受限时覆盖 GOPROXY（默认已用 goproxy.cn）：
docker build --build-arg GOPROXY=https://proxy.golang.org,direct -t searke .
```

运行时把 SSH 凭据和工作目录挂进容器（镜像基于 `scratch`，无 shell，非 root uid 65532）：

```bash
docker run --rm \
  -v ~/.ssh:/home/searke/.ssh:ro \          # 私钥 + known_hosts
  -v /path/to/workdir:/work -w /work \      # cluster.yaml 与 .searke 状态目录
  searke status -f cluster.yaml
```

注意 cluster.yaml 里 `spec.rke2.bundle` 写的是宿主机路径，需按**相同绝对路径**再挂一次（`-v /abs/bundle:/abs/bundle:ro`）。容器内适用远程操作（validate/plan/apply/status/add/uninstall/snapshot）；`searke run` 的本机安装语义不适用容器场景。

## 单机快速安装（searke run）

等价于 `sealos run --single`：不写 cluster.yaml，一条命令在本机装单节点集群。

```bash
sudo searke run ./rke2-oci
# 或归档 / registry 引用
sudo searke run ./rke2-v1.36.5-rke2r1-multiarch.oci.tar
```

行为：唯一节点为本机（address 固定 127.0.0.1 → 本地执行）；registration/API
地址取默认路由所在网卡的 IPv4（`--address` 可覆盖）；节点名取 hostname（DNS-1123 化）；
cluster-cidr/service-cidr 默认 10.42/10.43。生成的 cluster.yaml 和 apply 状态写入
`~/.searke/<name>/`，之后可以直接 `searke status|snapshot|apply -f` 复用或续跑。

- `sudo searke run` 时 HOME 解析为 `/root`，产物实际在 `/root/.searke/<name>/`
- 重跑 `searke run` 会**复用**已生成的 cluster.yaml：flags 与记录不符时报错而非悄悄覆盖；
  只有 bundle 参数允许变化（用于换 bundle 后 converge，版本变化会引导走 `upgrade`）
- `--dry-run` 只生成 cluster.yaml 并执行 plan（preflight + 动作列表），不做任何变更
- 非 root 运行需要 `sudo -n` 可用（preflight 会校验真实 uid=0，而非仅命令可执行）

## 命令一览

| 命令 | 状态 | 说明 |
| --- | --- | --- |
| `searke bundle fetch -f bundle.yaml -o <dir>` | ✅ 已实现并真实验证 | 按 bundle.yaml 下载制品：先经 GitHub API 确认 release 元数据；sha256sum 清单与 install.sh（信任锚）只从 GitHub 控制的端点获取（Release API / contents API），大文件可经第三方镜像传输但一律按受信清单验哈希；断点续传、停滞检测、原子化落盘，产物记录 `bundle.lock.yaml` |
| `searke bundle verify <dir>` | ✅ 已实现 | 重新校验已有 bundle：必需文件存在 + 哈希一致；manifest 中未下载的可选资产忽略 |
| `searke image build <dir> -o <oci-dir>` | ✅ 已实现并真实验证 | 按平台各建一个子镜像，产出 OCI Image Index；纯 OCI media type、固定时间戳可复现构建、`--tag` 写入 `image.ref.name` 注解；`--compression zstd|gzip`（默认 zstd，gzip 兼容拒绝 zstd 层的 registry，如 ACR） |
| `searke image push <ref> --from <oci-dir>` | ✅ 已实现并真实验证 | 各架构子镜像按 digest 并行推送（不产生多余 tag），再写 index；`--insecure` 走 HTTP；已在本机 HTTP+htpasswd registry 和阿里云 ACR 实测 |
| `searke login <registry> -u <user>` | ✅ 已实现并真实验证 | 写 `~/.docker/config.json`（与 `docker login` 互通），先向 registry 验证凭据再落盘；`--password-stdin` 供密码（`-p` 会泄露到 ps/history，使用时告警）；`--insecure` 把 registry 记入 `~/.searke/registries.json` 持久 HTTP 名单；`searke logout` 移除凭据 |
| `searke image save <ref-or-dir> -o <tar>` | ✅ 已实现并真实验证 | 导出为含 OCI Layout 的 tar 归档（离线搬运） |
| `searke image load <tar> -o <oci-dir>` | ✅ 已实现并真实验证 | 导入本地归档，还原 OCI Layout |
| `searke run <bundle>` | ✅ 已实现 | 单机安装（仿 `sealos run --single`）：合成单节点集群走本地执行，生成 cluster.yaml 于 `~/.searke/<name>/`；重跑复用文件；`--dry-run` 仅生成 + plan |
| `searke validate -f cluster.yaml` | ✅ 已实现 | 本地 schema + bundle 注解一致性 + SSH 连接 + 远端 preflight（只读） |
| `searke plan -f cluster.yaml` | ✅ 已实现 | validate 之上列出每个节点将执行的动作，不做远端变更 |
| `searke apply -f cluster.yaml` | ✅ 已实现 | 分阶段安装：preflight → 按节点架构分发 payload → 写 config.yaml → install.sh → 首 server → 其他 server → agent → Ready；阶段落盘可断点续跑，已属其他集群的节点会拒绝重置 |
| `searke status -f cluster.yaml` | ✅ 已实现 | 读取本地 apply 状态 + 从首 server 拉取 `kubectl get nodes` 实时视图 |
| `searke upgrade -f cluster.yaml` | ✅ 已实现（3-server 真机已验） | 滚动升级（cluster.yaml 换成新版本 bundle 后执行）：升级前自动 etcd 快照 → server 逐台先于 agent：drain → 传新制品 → install.sh → 重启 → 等节点 Ready → uncordon。支持 patch（v1.36.5→v1.36.6）、`+rke2rN` 修订和 +1 minor；拒绝降级、跨 minor、跨 major。中断后重跑跳过已升级节点并自动 uncordon；单节点集群自动跳过 drain；`--skip-drain`/`--skip-snapshot` |
| `searke snapshot save/list/pull/restore` | ✅ 已实现（3-server 真机已验） | etcd 快照工作流：首 server 上 `rke2 etcd-snapshot save`、列举、拉回本地。restore（需 `--yes`）先确认全部 server 可达且快照存在，然后按 RKE2 HA 流程：停全部 server → 首 server cluster-reset → 等它就绪 → 其余 server 清 etcd 数据重入（保留它们本地的快照目录） |
| `searke uninstall -f cluster.yaml --yes` | ✅ 已实现 | 从所有节点移除 RKE2：探测 `/usr/local/bin` 与 `/opt/rke2/bin` 的上游卸载脚本（agent 先于 server），**卸载后校验残留**而非只看退出码；逐节点容错——部分失败时继续卸载其余节点、保留状态并返回非零，`--ignore-unreachable` 可强制清状态；需 `--yes` 确认 |
| `searke add --nodes/--masters <ip>...` | ✅ 已实现（3-server 真机已验） | sealos 风格扩容：`--agents`/`--servers` 加 worker/master（`--nodes`/`--masters` 为真别名可混用）；**先对扩展后拓扑连节点做 preflight，全部通过才写回** cluster.yaml（地址写错不会污染描述文件）；新节点自动命名 `server-N`/`agent-N`，若会排到首 server 之前则改用 `<首server名>-N`，保证 bootstrap 节点不变；`--dry-run` 只看计划；`--ssh-user/--ssh-key/--ssh-port` 补全 `searke run` 集群的 ssh 配置（与已有值冲突会警告）；server 总数仍受 etcd 奇数校验约束 |
| `searke version` | ✅ 已实现 | 输出版本/commit/构建时间/Go 版本（`-ldflags -X` 注入） |
| `searke gen <cluster\|bundle>` | ✅ 已实现 | 输出带注释的 cluster.yaml/bundle.yaml 模板（与 `examples/` 同一来源）；`-o` 写文件且拒绝覆盖 |
| `searke image pull <ref> -o <dir>` | ✅ 已实现 | registry → 本地 OCI layout（`--insecure` 走 HTTP） |
| `searke image inspect <ref\|dir\|tar>` | ✅ 已实现（registry 真机已验） | 打印 index 的 rke2 版本/CNI/平台与 manifest digest；指向单架构 manifest 的 tag（如 `-arm64`）自动包装成单条 index 读取，inspect/pull/`bundle:` 引用均可用 |
| `searke exec -f cluster.yaml [--nodes a,b] -- <cmd>` | ✅ 已实现（3 节点真机已验） | 在选中节点上以 root 执行命令，输出带 `[node]` 前缀；单节点失败不中断，最后聚合报错 |
| `searke scp -f cluster.yaml [--nodes a,b] <local> <remote>` | ✅ 已实现（3-server 真机已验） | 上传文件到所有选中节点；`--download` 反向拉取到 `<local>/<node>-<basename>` |
| `searke hosts sync -f cluster.yaml` | ✅ 已实现（3 节点真机已验） | 向每个节点 `/etc/hosts` 写入 `# searke-hosts-begin/end` 管理块（幂等替换）；检测到 cloud-init `manage_etc_hosts` 时警告可能被重启覆盖 |
| `searke hostname [name] [--force]` | ✅ 已实现（3-server 真机已验） | `hostnamectl` 改名（缺省用描述里的节点名）并同步 `/etc/hosts` 的 `127.0.1.1` 行；显式给名字时必须用 `--nodes` 恰好选中 1 台；已装 RKE2 的节点默认拒绝 |
| `searke kubeconfig -f cluster.yaml -o <dir>` | ✅ 已实现（3-server 真机已验） | 从首 server 拉 `rke2.yaml`，server 地址优先改写为 `apiAddress`（回环时退回首 server 地址，IPv6 自动加方括号），写 `<cluster>-kubeconfig` 0600；不打印到 stdout |
| `searke delete --servers/--agents <name>...` | ✅ 已实现（3-server 真机全链路已验） | 节点缩容：drain（`--drain-timeout`，超时即中止不改 yaml）→ `kubectl delete node` → server 额外核验 etcd member 移除（经 crictl 在存活 server 的 etcd 容器内执行 etcdctl，按 peer URL 精确匹配，不依赖 API）→ 节点本机跑上游卸载脚本 → **最后**才原子改写 cluster.yaml 与 state。可断点续跑：节点已从 API 消失时跳过 drain/delete 继续后续步骤。护栏：server 必须保持奇数（方案A）、首 server 与 registration 地址不可删、需 `--yes`、`--dry-run` 出计划、`--skip-drain` 可跳过驱逐；别名 `--masters/--nodes` |
| `searke cert check` | ✅ 已实现（3-server 真机已验） | 拉取各 server apiserver 服务证书本地解析：到期时间 + SAN 列表，<30 天告警；不需要节点上 openssl |
| `searke cert san <ip/domain>... --yes` | ✅ 已实现（3-server 真机已验，含 yaml 持久化） | 每台 server：读远端 config.yaml 合并 tls-san → 逐台重启，等 service active **且本机 apiserver `/readyz` 通过**才处理下一台（保 etcd quorum）→ 重拉证书核验新 SAN 生效 → 全部成功才把 tls-san 写回 spec.rke2Config（升级/重写配置不丢） |
| `searke cert rotate --yes` | ✅ 已实现（3-server 真机已验） | 按上游顺序逐台 stop → `rke2 certificate rotate` → start → 等 `/readyz`；对比轮换前后证书 serial 输出 `rotated=true/false` 与新到期日 |
| `searke image serve <dir> --address :port` | ✅ 已实现并端到端验证 | 本地 OCI layout 目录暴露为只读 HTTP registry（无认证，请绑可信网卡）；digest 严格校验 64 位 hex；已验证 `image pull --insecure` 完整拉回且 digest 一致 |
| `searke completion bash\|zsh\|fish\|powershell` | ✅ 已实现 | shell 补全脚本 |

## 快速流程

**零制作起步（推荐先试）**：公共仓库 `registry.cn-hangzhou.aliyuncs.com/searke/rke2`
已推送开箱即用的 bundle 镜像，cluster.yaml 直接引用即可跳过下面第 1–4 步：

```yaml
spec:
  rke2:
    bundle: registry.cn-hangzhou.aliyuncs.com/searke/rke2:v1.36.5-rke2r1
```

- `v1.36.5-rke2r1`：双架构 index（linux/amd64 + linux/arm64，canal，gzip 层）
- `v1.36.5-rke2r1-amd64` / `v1.36.5-rke2r1-arm64`：单架构 tag，镜像减半（约 730MB），
  异构集群混用两个 tag 时每个集群选其一即可
- 工具镜像 `registry.cn-hangzhou.aliyuncs.com/searke/searke:latest` 同样公开，
  可按上面"Docker 镜像"一节的方式 `docker run` 使用

自己制作（新版 RKE2、换 CNI、完全离线搬运）：

```bash
# 1. 声明制品
cat > bundle.yaml <<EOF
apiVersion: searke.io/v1alpha1
kind: RKE2Bundle
spec:
  version: v1.36.5+rke2r1
  cni: canal
  platforms: [linux/amd64, linux/arm64]
EOF

# 2. 下载 + 校验（本仓库已用此流程验证，双架构约 1.5GB）
searke bundle fetch -f bundle.yaml -o dist/rke2-bundle

# 3. 打包多架构 OCI 镜像（registry 校验严格时用 --compression gzip）
searke image build dist/rke2-bundle -o dist/rke2-oci

# 4a. 有 Registry：推送
searke image push registry.example.com/searke/rke2:v1.36.5-rke2r1 --from dist/rke2-oci

# 4b. 纯离线：导出归档
searke image save dist/rke2-oci -o dist/rke2-multiarch.oci.tar

# 5. cluster.yaml 的 spec.rke2.bundle 指向 registry 引用、
#    .oci.tar 归档或 OCI layout 目录，然后：
searke validate -f cluster.yaml
searke plan      -f cluster.yaml
searke apply     -f cluster.yaml
searke status    -f cluster.yaml
```

示例文件见 `examples/`，或用 `searke gen cluster|bundle` 生成带注释的模板。

## 日常运维流程

```bash
F=cluster.yaml

# 扩容 / 缩容（缩容前先 --dry-run 看计划）
searke add    -f $F --masters 10.0.0.12,10.0.0.13 --nodes 10.0.0.21
searke delete -f $F --agents agent-1 --dry-run
searke delete -f $F --agents agent-1 --yes

# 滚动升级：先 fetch/build 新版本 bundle，再把 spec.rke2.bundle 指向它
searke upgrade -f $F

# etcd 快照
searke snapshot save    -f $F --name before-change
searke snapshot list    -f $F
searke snapshot pull    -f $F before-change-<node>-<ts> -o ./backups
searke snapshot restore -f $F before-change-<node>-<ts> --yes

# 批量运维
searke exec -f $F --nodes m1,m2 -- uptime
searke scp  -f $F ./tune.sh /usr/local/bin/tune.sh
searke hosts sync -f $F
searke kubeconfig -f $F -o ~/.kube && export KUBECONFIG=~/.kube/<cluster>-kubeconfig

# 证书
searke cert check  -f $F
searke cert san    -f $F lb.example.com 10.0.0.100 --yes
searke cert rotate -f $F --yes

# 整集群卸载
searke uninstall -f $F --yes
```

### 与 Sealos 命令对照

| Sealos | searke |
| --- | --- |
| `run` / `run --single` | `run`（单机）/ `apply -f cluster.yaml` |
| `add --masters/--nodes` | `add --masters/--nodes`（同义 `--servers/--agents`） |
| `delete --masters/--nodes` | `delete --masters/--nodes`（同义 `--servers/--agents`） |
| `reset` | `uninstall --yes` |
| `exec` / `scp` / `hosts` / `hostname` | 同名命令（`hosts sync`） |
| `cert --alt-names` | `cert san`（另有 `cert check` / `cert rotate`） |
| `build` / `push` / `pull` / `save` / `load` | `image build/push/pull/save/load` |
| `inspect` | `image inspect` |
| `login` / `logout` / `gen` / `version` / `completion` | 同名命令 |
| — | `bundle fetch/verify`、`validate`、`plan`、`status`、`upgrade`、`snapshot`、`kubeconfig`、`image serve` |

Sealos 基于本地镜像存储的命令（`create`/`tag`/`rmi`/`images`/`merge`/`manifest`/`prune`/`env`/`registry`）
与 searke 的 bundle + OCI index 模型不匹配，有意不实现。

## bundle.yaml

```yaml
apiVersion: searke.io/v1alpha1
kind: RKE2Bundle
spec:
  version: v1.36.5+rke2r1        # 必须完整固定；latest/stable/不完整版本被拒绝
  cni: canal                     # canal | calico | cilium | flannel | none（默认 canal）
  platforms: [linux/amd64, linux/arm64]
```

下载产物布局（`payload/linux-<arch>` 保留上游原始文件名）：

```text
rke2-bundle/
├── bundle.yaml / bundle.lock.yaml
├── install.sh
└── payload/
    ├── linux-amd64/{rke2.linux-amd64.tar.gz, rke2-images.linux-amd64.tar.zst, sha256sum-amd64.txt}
    └── linux-arm64/{…同上 arm64 版…}
```

非 canal CNI 自动切换为 `rke2-images-core` + `rke2-images-<cni>` 两个包。

## cluster.yaml

字段草案见 `examples/cluster.yaml` 与开发计划第 7 节。要点：

- `spec.servers` 数量必须为奇数（etcd）；**按名字排序的第一个 server 是 bootstrap 节点**。
  手工改名或新增 server 时不要让新名字排到它前面；apply 发现 bootstrap 变化会拒绝执行，
  以免新节点自己初始化出一个独立集群
- `spec.endpoints.registrationAddress` 必须是已存在的 VIP/LB 地址，searke 不创建它
- `spec.rke2Config` 中禁止 `server`/`token`/`node-name`（由工具管理）
- 节点名必须唯一，远端主机名也必须唯一（preflight 检查）

### 本机节点（local execution）

节点 `address` 指向本机时自动改走本地执行、不走 SSH，无需任何配置——探测范围
包括 `127.0.0.0/8`、`::1`、`localhost`、本机 hostname、以及本机所有网卡地址。同一
cluster.yaml 里本机节点和 SSH 远端节点可以混用。

- 推荐 `sudo searke apply` 直接以 root 运行：所有命令、文件分发（本地拷贝）都是
  root 直跑，无 sudo 开销
- 普通用户运行且 `spec.ssh.sudo: true`：特权命令走 `sudo -n`，上传经 staging 目录
  再 `install -D` 就位，与 SSH 路径同一语义
- 全部节点均为本机地址时 `spec.ssh.user`/`identityFile` 可省略
- 单机集群最小示例：`servers` 只写一个 `127.0.0.1` 节点即可（仍要求奇数台）

## 安全约定

- **信任锚模型**：sha256sum 清单与 install.sh 只从 GitHub 控制端点（github.com / raw.githubusercontent.com / api.github.com）获取；第三方镜像只传大文件且必须过受信清单的 SHA256 校验，清单本身绝不走镜像
- OCI 镜像内不含 SSH 私钥、集群 token、kubeconfig
- token 仅经 SSH 在内存中传递，不写入日志和状态文件
- 状态文件 `.searke/state-<cluster>.json` 权限 0600

## 远端节点布局

```text
/opt/searke/install.sh            上游安装脚本
/opt/searke/artifacts/            INSTALL_RKE2_ARTIFACT_PATH 指向的制品目录
/etc/rancher/rke2/config.yaml     searke 渲染的节点配置
/var/lib/rancher/searke-staging/  上传暂存区（SFTP/非 root 本地执行共用，传输后清理）
```

## 测试

```bash
go test ./...
```

单元测试覆盖：schema 校验、资产命名、sha256sum 解析、VerifyPlatform、
httptest 假 Release 端到端 fetch（含断点续传）、OCI build/extract/save/load
回环、registry 认证与 image serve、config 渲染（tls-san/labels）、状态机、
本地执行器与本机地址探测、升级版本路径、delete 护栏与 etcd member 匹配、
证书解析、descriptor 原子改写、快照名校验、扩容命名不改变 bootstrap。

真机测试环境见 `testlab/`：Parallels Ubuntu 24.04 arm64 autoinstall seed +
`cluster-parallels.yaml`（3 server）。克隆 VM 会复用 machine-id 导致 DHCP 撞 IP、
复用 host key 导致 known_hosts 冲突，建议每台独立安装。

## 故障排查

- **下载卡死/慢速连接**：大文件候选顺序为 GitHub → gh-proxy.com → ghproxy.net，连接级超时 10s、停滞 45s 即切换下一候选并断点续传。信任锚文件（sha256sum、install.sh）不走镜像，只回退到 api.github.com。`bundle.lock.yaml` 记录实际生效 URL。
- **`sha256 mismatch`**：上游清单与文件不符（串包或损坏）。删除对应文件重跑 fetch；校验未过不会生成镜像。
- **SSH dial 失败/known_hosts**：节点必须已在 `~/.ssh/known_hosts`（先手动 `ssh` 一次或 `ssh-keyscan`）。注意 Go ssh 客户端按协商算法校验——只录了 ed25519 而协商出 rsa/ecdsa 会报 `key mismatch`，用 `ssh-keyscan -t ed25519,rsa,ecdsa <ip> >> ~/.ssh/known_hosts` 录全。密钥需无密码或在 ssh-agent 中。本机地址节点不走 SSH，跳过此检查。
- **`passwordless sudo required`**：`spec.ssh.sudo: true` 时要求 `sudo -n` 可用；否则用 root 用户，或以 root 运行 searke（本机节点）。
- **节点已属其他集群**：apply/preflight 发现远端 `config.yaml` 的 `server:` 与本集群注册地址不同会拒绝继续，不会自动 reset。
- **中断续跑**：`.searke/state-<cluster>.json` 记录每节点阶段；重跑 `apply` 自动跳过已完成阶段。版本不一致的状态文件需 `upgrade` 或删除重建。`upgrade`、`delete` 中断后直接重跑同一命令即可。
- **`downgrades are not supported` / `skips N minor versions`**：RKE2 只支持同 minor 内升级或一次 +1 minor；跨多个 minor 需逐级准备 bundle 依次 upgrade。
- **`bootstrap server changed`**：cluster.yaml 中按名字排序的第一个 server 变了（通常是新增/改名的 server 排到了前面），按提示改名。
- **kubeconfig**：`searke kubeconfig -f cluster.yaml -o <dir>`。
- **RHEL 系 sudo 找不到 rke2**：RHEL 的 `secure_path` 不含 `/usr/local/bin`；searke 调 rke2 时会显式补上 `/usr/local/bin` 与 `/opt/rke2/bin`，手工排查时用全路径。

## 已知限制 / 待办

- `image push`/`login` 已在本机 `registry:2`（HTTP + htpasswd）和阿里云 ACR 真实验证：`searke/rke2:v1.36.5-rke2r1`（gzip 层双架构 index + `-amd64`/`-arm64` 单架构 tag）、`searke/searke:latest`/`v0.1.0`（工具镜像）。注意 ACR 拒绝 zstd 层（`BLOB_TYPE_INVALID`），推送前需 `image build --compression gzip`
- cluster.yaml `spec.rke2.insecure: true` 可让 `bundle: <registry-ref>` 走 HTTP；`searke run` 对应 `--insecure`
- 已真实验证（Parallels Ubuntu 24.04 **arm64** 独立 VM ×3，真实 SSH）：**3-server HA 拓扑完整生命周期**——`apply` 三控制面全 Ready、`exec`/`scp` 上下行、`hosts sync`、`kubeconfig` 本地直连、`cert check/san/rotate` 全链路、`delete` 缩容（3→1：drain→delete node→etcd member 核验→卸载→yaml 更新，member list 确认为 1）、`add` 回补（1→3 全 Ready）、`status` 实时视图、`upgrade` 滚动升级（v1.36.5→v1.36.6：前置 etcd 快照、逐台 drain→重传制品→install.sh→就绪等待→uncordon、etcd quorum 全程保持、中断重跑自愈 cordon、升完重跑幂等退出）、`snapshot restore` HA 恢复（预检→全停→首 server cluster-reset→就绪等待→peer 清 etcd 重入且保留本地快照目录；快照后写入的 marker 确认消失）、`uninstall` 3 节点全量卸载 + 全新 `apply` 重装（含首 server `/readyz` 就绪门）。另在早期 2 节点环境验过 `searke run` 单节点、`snapshot save/list/pull`、幂等重跑
- 全部已声明功能均完成真机验证
- 节点 OS：RKE2 tar 安装与发行版无关，preflight 仅警告非 Ubuntu 发行版（Ubuntu 22.04 / 24.04 已真机验证）。硬性限制：SELinux 必须为 Permissive/Disabled（tar 安装不含 rke2-selinux 策略）；firewalld active / NetworkManager 未豁免 `cali*`/`flannel*` / 缺 iptables 各给一条警告
- `image serve` 无 TLS/认证，仅供可信内网
- `cert rotate` 只处理 server；agent 的客户端证书由 kubelet 自动轮转
- 单条远端命令没有超时/取消（drain 自带 `--timeout`，kubectl 统一 `--request-timeout=90s`）；Ctrl-C 只在步骤之间生效
- CNI profile 扩展未做（计划明确缓议）
