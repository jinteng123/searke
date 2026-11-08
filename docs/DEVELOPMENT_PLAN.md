# Searke 开发计划：RKE2 多架构集群镜像与 apply

- 文档状态：提案（阶段 0–3 代码已实现，见下方实现状态）
- 更新日期：2026-11-06
- 目标产品：`searke` 单文件命令行工具

## 实现状态（2026-11-06）

代码位于 `cmd/searke` + `internal/{bundle,cluster,ociimg,deploy,cli}`，用法见根目录 `README.md`。

| 计划项 | 状态 | 说明 |
| --- | --- | --- |
| 阶段 0：schema + OCI 原型 | ✅ 完成 | `bundle.yaml`/`cluster.yaml` 解析与校验；多架构 OCI Index 构建、按平台提取均经单元测试与真实制品验证 |
| 阶段 1：`bundle fetch`/`verify` | ✅ 完成 | `v1.36.5+rke2r1` amd64+arm64 全量下载、上游 SHA256 校验、`bundle.lock.yaml` 生成真实通过；超时/重试/断点续传/原子落盘已实现 |
| 阶段 2：`image build/push/save/load` | ✅ 完成 | build/save/load 真实验证（1.5GB 双架构归档）；push 已在阿里云 ACR（`registry.cn-hangzhou.aliyuncs.com`）真实推送验证 |
| 阶段 3：`validate/plan/apply` | ✅ 完成 | Parallels Ubuntu 22.04 arm64 VM 实测：SSH apply 单节点 + agent join 组成 2 节点集群、幂等重跑、本机节点本地执行安装全部通过；preflight 拦获并处理了缺省路由、主机名重复、known_hosts 算法不匹配等实机问题 |
| 阶段 4：`status`+生命周期 | ✅ 基本验证 | `status`（状态+live kubectl）、`snapshot save/list/pull`、3-server HA 拓扑、`add`/`delete` 全链路、`upgrade` 滚动升级（v1.36.5→v1.36.6：前置 etcd 快照、逐台 drain→换包→就绪等待→uncordon、etcd quorum 全程保持、幂等重跑/断点续跑）均已在 Parallels 3-server 真机验证；仅剩 `snapshot restore` HA 恢复流程未实机验证 |

**二期计划：** 对齐 Sealos 常用命令（`delete` 缩容、`exec`/`scp`、`cert`、`kubeconfig` 等）见 `docs/SEALOS_PARITY_PLAN.md`。

**实现期新增决策：** 开发环境实测 `raw.githubusercontent.com` 与 GitHub Release 资产域名间歇不可达。下载器采用**信任锚模型**：先经 `api.github.com` 读取 release 元数据（兼作"tag 与资产存在"预检）；sha256sum 清单与 `install.sh` 只从 GitHub 控制端点获取（Release 资产 API / contents API），绝不走第三方镜像；大体积制品允许经 gh-proxy.com / ghproxy.net 传输，但全部按受信清单校验 SHA256。连接级超时 10s、响应头 30s、body 停滞 45s 即切换候选并断点续传。全部下载先入临时目录，校验通过后整体原子替换输出目录。

**本地执行（local execution，仿 sealos）：** 节点 `address` 指向本机时 `Connect` 返回 `LocalExecutor` 而非 SSH client——命令经 `sh -c` 直跑、文件分发变成本地拷贝，apply/upgrade/snapshot/status 全部生效且编排代码零改动。探测范围：loopback、`localhost`、本机 hostname、本机网卡地址，与远端 SSH 节点可在同一集群混用。root 运行 searke 时特权操作直跑；非 root + `ssh.sudo: true` 走 `sudo -n` 与 staging 目录，语义与 SSH 路径一致。全部节点为本机地址时 `spec.ssh.user` 可省略。

**`searke run`（仿 `sealos run --single`）：** 免 cluster.yaml 的单机安装入口——合成单节点集群（本机 127.0.0.1 → 本地执行）、默认路由网卡 IPv4 作 registration/apiAddress、hostname 作节点名；生成的 cluster.yaml 与状态写入 `~/.searke/<name>/` 供 `status`/`snapshot`/续跑复用；重跑复用已生成文件（flags 冲突报错，仅 bundle 可变更）；`--dry-run` 仅生成文件并执行 plan。连接日志标注 `ssh`/`local exec`；单节点集群 upgrade 自动跳过 drain；preflight 的 root 校验改为实测 uid=0。

## 1. 背景与目标

当前 RKE2 离线部署依赖 Ansible 和 Python 运行环境，在 ARM64 控制节点上遇到 Python wheel / 原生扩展兼容问题。`searke` 的目标是用一个 Go 编译的命令行工具描述、打包和分发 RKE2 制品，并通过 YAML 驱动集群安装。

首期目标拓扑是 3 个 RKE2 server 和 3 个 agent。首期验证环境按 Ubuntu 22.04、Linux `amd64` 与 `arm64`、默认 Canal CNI 规划。RKE2 版本必须显式指定；现有 ARM64 离线样例 `v1.36.5+rke2r1` 可用于首个开发验证，不作为工具默认版本。

### 首期目标

- 用 Go 单文件 CLI 完成离线制品下载、校验、多架构 OCI 镜像打包和 `apply` 部署。
- 使用 OCI Image Index 将同一 RKE2 版本的 `linux/amd64`、`linux/arm64` 子镜像放在同一个镜像引用下。
- 支持 Registry 引用和本地 OCI 归档，安装节点不需要 Docker、Ansible 或 Python。
- 支持 `validate`、`plan`、`apply` 和 `status`，并允许 `apply` 在中断后安全重跑。

### 首期不做

- 不实现 Ansible 的通用任务引擎。
- 不创建云主机、不配置操作系统、不创建负载均衡器或 VIP；集群 YAML 提供已经可用的节点地址和 HA 注册地址。
- 不支持任意 Kubernetes 发行版、Windows 节点、多种 Linux 发行版和所有 CNI。
- 不做 Sealos 的应用部署平台、图形界面和多租户控制面。
- 不在集群镜像内保存 SSH 私钥、集群 token、kubeconfig 或集群专属配置。

## 2. 关键设计决策

1. **集群描述与软件制品分开。** `cluster.yaml` 描述在哪些节点安装、节点角色、SSH 凭据引用和集群配置；`bundle.yaml` 描述 RKE2 版本、平台、制品和 CNI。一个 bundle 可以用于多个集群。
2. **复用 RKE2 官方离线安装流程。** 初版打包上游 `install.sh`，通过 `INSTALL_RKE2_ARTIFACT_PATH` 指向本地制品目录。暂不自行复制安装脚本的 systemd、SELinux 和安装路径处理逻辑。
3. **OCI Index 按目标节点架构选择。** 一个标签包含多个平台子镜像；`apply` 查询目标节点架构后选择对应子镜像，不能按运行 CLI 的电脑架构选择。
4. **版本不可漂移。** 构建必须固定 RKE2 完整版本号和 release tag，不以 `latest` 或 `stable` 作为 bundle 的隐式版本。
5. **采用可恢复、幂等的安装阶段。** 首个 server 初始化后，再逐个加入其他 server，最后加入 agent。对已初始化节点不执行重置。
6. **OCI 镜像是制品分发包。** `searke` 读取并提取镜像层中的文件，不运行这个 OCI 镜像作为容器。

## 3. 命令与用户流程

```text
searke bundle fetch -f bundle.yaml --output ./dist/rke2-bundle
searke bundle verify ./dist/rke2-bundle
searke image build ./dist/rke2-bundle --tag registry.example.com/searke/rke2:v1.36.5-rke2r1
searke image push registry.example.com/searke/rke2:v1.36.5-rke2r1

# 离线场景
searke image save registry.example.com/searke/rke2:v1.36.5-rke2r1 \
  --output ./dist/rke2-v1.36.5-rke2r1-multiarch.oci.tar
searke apply -f cluster.yaml
```

`cluster.yaml` 的 `spec.rke2.bundle` 可以填写 Registry 引用或本地 OCI 归档路径。构建机需要访问制品源；目标节点只需能通过 SSH 被控制端访问。

## 4. RKE2 制品来源与下载规则

### 输入参数

`bundle.yaml` 至少指定：

```yaml
apiVersion: searke.io/v1alpha1
kind: RKE2Bundle
spec:
  version: v1.36.5+rke2r1
  cni: canal
  platforms:
    - linux/amd64
    - linux/arm64
```

### 官方制品映射

每个平台下载以下文件：

| 平台 | RKE2 二进制包 | 默认镜像包 | 校验文件 |
| --- | --- | --- | --- |
| `linux/amd64` | `rke2.linux-amd64.tar.gz` | `rke2-images.linux-amd64.tar.zst` | `sha256sum-amd64.txt` |
| `linux/arm64` | `rke2.linux-arm64.tar.gz` | `rke2-images.linux-arm64.tar.zst` | `sha256sum-arm64.txt` |

下载 URL 由固定 release tag 和资产文件名组成。RKE2 版本中的 `+` 在 Release URL 中编码成 `%2B`，例如 `v1.36.5+rke2r1` 对应 `v1.36.5%2Brke2r1`。安装脚本从对应源码 tag 获取，例如 `https://raw.githubusercontent.com/rancher/rke2/v1.36.5%2Brke2r1/install.sh`。

### 具体下载样例

以下示例以现有 ARM64 制品的版本为例，同时下载 `amd64` 和 `arm64`。实际构建时，版本由 `bundle.yaml` 指定；脚本不能把示例版本当默认值。

```bash
VERSION_TAG='v1.36.5%2Brke2r1'
RELEASE_BASE="https://github.com/rancher/rke2/releases/download/${VERSION_TAG}"
INSTALLER_URL="https://raw.githubusercontent.com/rancher/rke2/${VERSION_TAG}/install.sh"

mkdir -p dist/rke2-bundle/payload/linux-amd64
mkdir -p dist/rke2-bundle/payload/linux-arm64

curl -fL "${INSTALLER_URL}" \
  -o dist/rke2-bundle/install.sh

for ARCH in amd64 arm64; do
  DEST="dist/rke2-bundle/payload/linux-${ARCH}"
  curl -fL "${RELEASE_BASE}/rke2.linux-${ARCH}.tar.gz" \
    -o "${DEST}/rke2.linux-${ARCH}.tar.gz"
  curl -fL "${RELEASE_BASE}/rke2-images.linux-${ARCH}.tar.zst" \
    -o "${DEST}/rke2-images.linux-${ARCH}.tar.zst"
  curl -fL "${RELEASE_BASE}/sha256sum-${ARCH}.txt" \
    -o "${DEST}/sha256sum-${ARCH}.txt"
done

searke bundle verify dist/rke2-bundle
```

也就是说，ARM64 镜像包的完整下载地址为：

```text
https://github.com/rancher/rke2/releases/download/v1.36.5%2Brke2r1/rke2-images.linux-arm64.tar.zst
```

`searke bundle fetch` 实现同样的流程，但应使用 Go HTTP 客户端而不是依赖系统里的 `curl`：由版本和架构生成文件名与 URL，跟随 GitHub Release 下载跳转，将响应流写入临时文件；HTTP 成功且文件完整后，再移到目标目录。可用 GitHub Release 元数据先确认该 tag 包含预期资产；当前后端也可按上游固定命名直接请求资产 URL。缺失资产不得静默跳过。

校验时解析 `sha256sum-<arch>.txt`，按文件名取出本次下载文件对应的期望 SHA256，再用 Go 的 SHA256 实现校验文件内容。校验清单还会列出本次未下载的其他可选资产，因此验证器只检查 bundle 声明的必需和可选文件，不把未下载的行当成错误，也不忽略已下载文件的哈希错误。通过后再生成 bundle 元数据和 OCI 层。

如果以后选择 Cilium 等非默认 CNI，下载清单按所选插件改为 `rke2-images-core.linux-<arch>.tar.zst` 加 `rke2-images-<cni>.linux-<arch>.tar.zst`；bundle manifest 要记录这组文件和对应 SHA256。

下载器实现要求：

- 只接受显式版本和明确支持的平台；拒绝 `latest`、未知架构和不完整版本。
- 可先读取 GitHub Release 元数据确认 tag 与资产名；下载使用确认过的 release 资产地址。若 API 暂不可用，可用受支持的固定文件名构造 Release URL，但下载失败必须明确报错。
- 下载到临时目录，支持超时、有限重试和断点续传；成功后原子移动到输出目录。
- 解析上游 `sha256sum-<arch>.txt`，校验本次下载的二进制包和镜像包。哈希不匹配时不得生成 OCI 镜像。
- 在 `bundle.yaml` 或生成的 lock 文件中记录 RKE2 完整版本、源码 URL、平台、CNI、资产名、大小和 SHA256。
- 若兼容较老的 RKE2 release，可支持 `.tar.gz` 镜像包回退；首期当前版本优先使用 `.tar.zst`，不可静默混用。

`rke2-images.linux-<arch>.tar.zst` 包含默认 Canal 相关镜像。首期固定 Canal；以后支持其他 CNI 时，根据选择改为 `rke2-images-core` 加对应插件镜像包，并校验集群配置与 bundle 内容一致。[RKE2 Air-Gap Install](https://docs.rke2.io/install/airgap)

## 5. Bundle 源目录格式

```text
rke2-bundle/
├── bundle.yaml
├── install.sh
└── payload/
    ├── linux-amd64/
    │   ├── rke2.linux-amd64.tar.gz
    │   ├── rke2-images.linux-amd64.tar.zst
    │   └── sha256sum-amd64.txt
    └── linux-arm64/
        ├── rke2.linux-arm64.tar.gz
        ├── rke2-images.linux-arm64.tar.zst
        └── sha256sum-arm64.txt
```

每个 `payload/linux-<arch>` 目录保留 RKE2 要求的原始资产名称。`install.sh` 可以作为公共文件；部署时，`INSTALL_RKE2_ARTIFACT_PATH` 指向所选平台的 payload 目录。OCI 子镜像中只放该架构的 payload，避免把错误架构的文件交给节点。

`bundle.yaml` 不存放集群节点、网络地址、SSH 私钥和 token。`install.sh`、二进制、镜像包和上游校验清单作为来源文件一并保留。

## 6. 多架构 OCI 镜像格式

构建结果是一个 OCI Image Index：

```text
registry.example.com/searke/rke2:v1.36.5-rke2r1
└── OCI Image Index
    ├── linux/amd64 -> amd64 子镜像
    └── linux/arm64 -> arm64 子镜像
```

每个子镜像的文件层包含：

```text
/opt/searke/bundle.yaml
/opt/searke/install.sh
/opt/searke/payload/rke2.linux-<arch>.tar.gz
/opt/searke/payload/rke2-images.linux-<arch>.tar.zst
/opt/searke/payload/sha256sum-<arch>.txt
```

`searke image build` 负责创建两个 platform-specific image manifest，再创建 OCI Image Index。镜像配置和 Index descriptor 要正确标注 `os=linux`、`architecture=amd64|arm64`。本地归档采用 OCI Image Layout；其中有 `oci-layout`、`index.json` 和 `blobs/`。不要求安装 Docker daemon 才能构建、读取或部署。[OCI Image Index](https://github.com/opencontainers/image-spec/blob/main/image-index.md) [OCI Image Layout](https://github.com/opencontainers/image-spec/blob/main/image-layout.md)

## 7. 集群 YAML 与 `apply`

集群 YAML 的结构草案：

```yaml
apiVersion: searke.io/v1alpha1
kind: Cluster
metadata:
  name: rke2-lab
spec:
  rke2:
    bundle: ./dist/rke2-v1.36.5-rke2r1-multiarch.oci.tar
    cni: canal
  ssh:
    user: ubuntu
    identityFile: ~/.ssh/id_ed25519
    sudo: true
  endpoints:
    registrationAddress: 10.0.0.100:9345
    apiAddress: 10.0.0.100:6443
  servers:
    - { name: master-1, address: 10.0.0.11 }
    - { name: master-2, address: 10.0.0.12 }
    - { name: master-3, address: 10.0.0.13 }
  agents:
    - { name: worker-1, address: 10.0.0.21 }
    - { name: worker-2, address: 10.0.0.22 }
    - { name: worker-3, address: 10.0.0.23 }
  rke2Config:
    cluster-cidr: 10.42.0.0/16
    service-cidr: 10.43.0.0/16
```

`apply` 阶段：

1. 校验 YAML schema、bundle 版本、CNI、镜像中的平台清单和节点角色。
2. SSH 检查 OS、架构、唯一主机名、权限、磁盘、默认路由和必要端口。
3. 查询每台目标节点的架构：`x86_64` 映射到 `amd64`，`aarch64` 映射到 `arm64`；按节点选择 OCI 子镜像，而不是按 CLI 主机选择。
4. 校验所选 payload 后，将其分发到各节点的工作目录。
5. 写入 `/etc/rancher/rke2/config.yaml`，共享 server 的关键字段保持一致；通过 `INSTALL_RKE2_TYPE=server|agent` 调用上游离线安装脚本。
6. 启动首个 server，等 API 和注册端点就绪；通过 SSH 安全读取首个 server 的 token，逐台加入其他 server，再加入 agent。
7. 检查 systemd 服务与 Kubernetes 节点 Ready 状态，保存本次 apply 的阶段记录，输出 kubeconfig 的安全获取方式。

三 server HA 要求提供稳定注册地址；首期从 `cluster.yaml` 读取现成的 VIP 或负载均衡地址，不负责创建它。[RKE2 High Availability](https://docs.rke2.io/install/ha)

幂等要求：相同 bundle 和集群配置重复执行时，只恢复未完成步骤；遇到已经属于其他集群的节点、版本不匹配或配置漂移时停止并指出冲突，不自动 `reset` 或卸载已有集群。`plan` 不执行远端变更。

## 8. 分阶段开发计划

每个开发项控制在约 1–2 天；阶段日期待第一次 OCI 与下载原型验证后再排。先消除最大技术不确定性，不并行开发互相依赖的模块。

### 阶段 0：资源与 OCI 原型

- 定义 `bundle.yaml` 与 `cluster.yaml` 的首版字段和兼容策略。
- 验证指定版本的 AMD64、ARM64 Release 资产、安装脚本 tag 和 SHA256 文件名。
- 创建含两个 platform manifest 的本地 OCI Image Index，并能逐平台读出 payload。

**完成标准：** 用现有 ARM64 制品和一个 AMD64 Release 生成 OCI 归档；索引能正确列出两个平台；每个平台提取出的资产哈希与上游清单一致。

### 阶段 1：`bundle fetch` 与 `bundle verify`

- YAML 解析、字段校验、版本和架构归一化。
- Release 元数据解析及指定文件下载，支持重试和临时目录。
- 上游 SHA256 校验及 bundle lock 信息生成。
- 错误报告指出版本、架构、资产 URL 和期望/实际哈希。

**完成标准：** 能从空目录生成完整 ARM64+AMD64 bundle；错误版本、缺失资产、网络中断和哈希不符均明确失败，不留下可误用的半成品。

### 阶段 2：`image build/push/save/load`

- 为每个平台构建单独的 OCI 子镜像和 Image Index。
- 输出本地 OCI Layout，支持归档导出、导入和 Registry push/pull。
- OCI 元数据标注 RKE2 版本、平台和 CNI；推送前检查镜像与源 bundle 的 digest。

**完成标准：** 本地归档可在无 Registry 环境打开；同一 tag 可识别 amd64 和 arm64；push 后 pull 并提取的 payload 校验不变。构建和归档过程中不依赖 Docker daemon。

### 阶段 3：`validate/plan/apply` MVP

- 实现 SSH、SFTP、远端 preflight 和分发。
- 实现单 server + agent 验证流，再扩展为三 server + 三 agent。
- 实现 token 获取、节点角色配置、systemd 启动、健康检查和失败恢复记录。
- 实现重复执行保护和 `plan` 只读输出。

**完成标准：** ARM64 和 AMD64 集群分别可从本地 OCI 归档离线安装；无 Python/Ansible 运行时；中断后重跑可续做；错误架构、错误 checksum 和不满足 preflight 时不会启动安装。

### 阶段 4：`status` 与后续生命周期

- 查看节点、服务、RKE2 版本和最近操作记录。
- 增加 etcd snapshot/restore 工作流。
- 增加按 server 后 agent 的滚动升级。
- 根据实际需求再扩展节点增删与 CNI profiles。

**完成标准：** 每个生命周期命令有明确前置检查、可观察进度和可恢复行为；升级严格遵守 RKE2 server 先于 agent 的顺序。[RKE2 Manual Upgrades](https://docs.rke2.io/upgrades/manual)

## 9. 主要风险与处理

| 风险 | 处理方式 |
| --- | --- |
| 不同 RKE2 版本的资产名或压缩格式不同 | 按 tag 查 Release 资产；支持列表之外的格式直接失败，不猜测文件名。 |
| 下载文件损坏或平台串包 | 上游 SHA256 + bundle 元数据双层校验；OCI digest 固定构建结果。 |
| 控制端架构与节点架构不同 | 依据远端 preflight 结果逐节点选择 OCI 子镜像。 |
| CNI 与镜像包不匹配 | 首期限制 Canal；bundle 与 cluster 的 CNI 值必须一致。 |
| 多 server 配置不一致导致加入失败 | 由单一集群配置生成所有 server 文件，并校验关键字段一致。 |
| 首个 server 已启动但 apply 中断 | 保存阶段记录；重跑先读取远端实际状态，避免重复初始化。 |
| SSH 密钥或集群 token 泄露 | 镜像不含密钥；token 只经 SSH 传递，不写入普通日志；本地状态文件限制权限。 |
| RKE2 需要的 OS 依赖不满足 | 首期限定 Ubuntu 22.04 并在 preflight 中检查；不在初版自动替换系统包。 |

## 10. Definition of Done

- 可由明确版本、CNI 和平台列表构建多架构 OCI 镜像。
- 所有源文件和 OCI 层均可按摘要追溯和校验。
- Registry 与本地 OCI 归档两种输入都能驱动 `apply`。
- `apply` 通过目标节点架构选择载荷，并以无 Ansible/Python 依赖方式部署 RKE2。
- 三 server + 三 agent 集群完成 HA 注册、节点 Ready 和重复 apply 验证。
- README、bundle schema、cluster schema、故障排查和版本升级说明随实现更新。

## 11. 官方参考

- [RKE2 Air-Gap Install](https://docs.rke2.io/install/airgap)
- [RKE2 v1.36.5+rke2r1 install.sh](https://raw.githubusercontent.com/rancher/rke2/v1.36.5%2Brke2r1/install.sh)
- [RKE2 High Availability](https://docs.rke2.io/install/ha)
- [RKE2 Manual Upgrades](https://docs.rke2.io/upgrades/manual)
- [OCI Image Index Specification](https://github.com/opencontainers/image-spec/blob/main/image-index.md)
- [OCI Image Layout Specification](https://github.com/opencontainers/image-spec/blob/main/image-layout.md)
- [Sealos build 命令](https://sealos.io/docs/k8s/reference/sealos/commands/build/)
- [Sealos apply 命令](https://sealos.io/docs/k8s/reference/sealos/commands/apply/)
