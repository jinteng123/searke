# searke

`searke` 是一个 Go 单文件 CLI，用于离线（air-gap）场景下的 RKE2 多架构集群部署：
下载上游制品、校验、打包为多架构 OCI 镜像，并通过 SSH + YAML 驱动安装，
不依赖 Docker daemon、Ansible 或 Python。

详细设计见 [docs/DEVELOPMENT_PLAN.md](docs/DEVELOPMENT_PLAN.md)。

## 构建

```bash
go build -o searke ./cmd/searke
```

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
| `searke image build <dir> -o <oci-dir>` | ✅ 已实现并真实验证 | 按平台各建一个子镜像，产出 OCI Image Index；纯 OCI media type（manifest/config/zstd 层）、固定时间戳可复现构建、`--tag` 写入 `image.ref.name` 注解 |
| `searke image push <ref> --from <oci-dir>` | ✅ 已实现 | `remote.WriteIndex` 推送整个 index，使用默认 docker keychain 认证 |
| `searke image save <ref-or-dir> -o <tar>` | ✅ 已实现并真实验证 | 导出为含 OCI Layout 的 tar 归档（离线搬运） |
| `searke image load <tar> -o <oci-dir>` | ✅ 已实现并真实验证 | 导入本地归档，还原 OCI Layout |
| `searke run <bundle>` | ✅ 已实现 | 单机安装（仿 `sealos run --single`）：合成单节点集群走本地执行，生成 cluster.yaml 于 `~/.searke/<name>/`；重跑复用文件；`--dry-run` 仅生成 + plan |
| `searke validate -f cluster.yaml` | ✅ 已实现 | 本地 schema + bundle 注解一致性 + SSH 连接 + 远端 preflight（只读） |
| `searke plan -f cluster.yaml` | ✅ 已实现 | validate 之上列出每个节点将执行的动作，不做远端变更 |
| `searke apply -f cluster.yaml` | ✅ 已实现 | 分阶段安装：preflight → 按节点架构分发 payload → 写 config.yaml → install.sh → 首 server → 其他 server → agent → Ready；阶段落盘可断点续跑，已属其他集群的节点会拒绝重置 |
| `searke status -f cluster.yaml` | ✅ 已实现 | 读取本地 apply 状态 + 从首 server 拉取 `kubectl get nodes` 实时视图 |
| `searke upgrade -f cluster.yaml` | ✅ 已实现 | 滚动升级：server 逐台先于 agent；升级前自动 etcd 快照 + drain/uncordon（可 `--skip-*` 关闭，单节点集群自动跳过 drain）；拒绝降级与跨 minor 跳跃；`apply` 检测到版本漂移会引导改用本命令 |
| `searke snapshot save/list/pull/restore` | ✅ 已实现 | etcd 快照工作流：首 server 上 `rke2 etcd-snapshot save`、列举、拉回本地；restore 按 RKE2 HA 流程停全部 server → 首 server cluster-reset → 其余 server 清 db 重入（需 `--yes`） |
| `searke uninstall -f cluster.yaml --yes` | ✅ 已实现 | 从所有节点移除 RKE2：探测 `/usr/local/bin` 与 `/opt/rke2/bin` 的上游卸载脚本（agent 先于 server），**卸载后校验残留**而非只看退出码；逐节点容错——部分失败时继续卸载其余节点、保留状态并返回非零，`--ignore-unreachable` 可强制清状态；需 `--yes` 确认 |
| `searke add --nodes/--masters <ip>...` | ✅ 已实现 | sealos 风格扩容：`--agents`/`--servers` 加 worker/master（`--nodes`/`--masters` 为真别名可混用）；**先对扩展后拓扑连节点做 preflight，全部通过才写回** cluster.yaml（地址写错不会污染描述文件）；`--dry-run` 只看计划；`--ssh-user/--ssh-key/--ssh-port` 补全 `searke run` 集群的 ssh 配置（与已有值冲突会警告）；server 总数仍受 etcd 奇数校验约束 |

## 快速流程

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

# 3. 打包多架构 OCI 镜像
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

示例文件见 `examples/`。

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

- `spec.servers` 数量必须为奇数（etcd）；首个按名字排序的 server 是 bootstrap 节点
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
回环、config 渲染、状态机、本地执行器与本机地址探测。真实环境已验证：
`v1.36.5+rke2r1` 双架构 fetch + verify + build + save + load 全部通过。

## 故障排查

- **下载卡死/慢速连接**：大文件候选顺序为 GitHub → gh-proxy.com → ghproxy.net，连接级超时 10s、停滞 45s 即切换下一候选并断点续传。信任锚文件（sha256sum、install.sh）不走镜像，只回退到 api.github.com。`bundle.lock.yaml` 记录实际生效 URL。
- **`sha256 mismatch`**：上游清单与文件不符（串包或损坏）。删除对应文件重跑 fetch；校验未过不会生成镜像。
- **SSH dial 失败/known_hosts**：节点必须已在 `~/.ssh/known_hosts`（先手动 `ssh` 一次或 `ssh-keyscan`）。注意 Go ssh 客户端按协商算法校验——只录了 ed25519 而协商出 rsa/ecdsa 会报 `key mismatch`，用 `ssh-keyscan -t ed25519,rsa,ecdsa <ip> >> ~/.ssh/known_hosts` 录全。密钥需无密码或在 ssh-agent 中。本机地址节点不走 SSH，跳过此检查。
- **`passwordless sudo required`**：`spec.ssh.sudo: true` 时要求 `sudo -n` 可用；否则用 root 用户，或以 root 运行 searke（本机节点）。
- **节点已属其他集群**：apply/preflight 发现远端 `config.yaml` 的 `server:` 与本集群注册地址不同会拒绝继续，不会自动 reset。
- **中断续跑**：`.searke/state-<cluster>.json` 记录每节点阶段；重跑 `apply` 自动跳过已完成阶段。版本不一致的状态文件需 `upgrade` 或删除重建。
- **kubeconfig**：`ssh <user>@<首server> 'sudo cat /etc/rancher/rke2/rke2.yaml'`，把 server 地址换成 `apiAddress`。

## 已知限制 / 待办

- `image push` 代码已实现但未在真实 registry 验证
- 已真实验证（Parallels Ubuntu 22.04 **arm64** VM，RKE2 v1.36.5+rke2r1）：SSH `apply` 单节点 + agent join 成 2 节点集群、幂等重跑跳过、`status` 实时视图、`snapshot save/list/pull`、VM 内 `searke run` 本地执行安装单节点、`uninstall` 卸载后全新重装闭环、`searke add` 增量扩容（3 节点 Ready）
- 未真实验证：`upgrade`、HA `snapshot restore`、3-server HA 拓扑、`image push`
- 首期节点 OS 限定 Ubuntu（22.04 为验证版本；preflight 强制 Ubuntu，非 22.04 仅警告）
- 节点增删、CNI profile 扩展未做（计划明确缓议）
