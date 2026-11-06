# searke

`searke` 是一个 Go 单文件 CLI，用于离线（air-gap）场景下的 RKE2 多架构集群部署：
下载上游制品、校验、打包为多架构 OCI 镜像，并通过 SSH + YAML 驱动安装，
不依赖 Docker daemon、Ansible 或 Python。

详细设计见 [docs/DEVELOPMENT_PLAN.md](docs/DEVELOPMENT_PLAN.md)。

## 构建

```bash
go build -o searke ./cmd/searke
```

## 命令一览

| 命令 | 状态 | 说明 |
| --- | --- | --- |
| `searke bundle fetch -f bundle.yaml -o <dir>` | ✅ 已实现并真实验证 | 按 bundle.yaml 从 GitHub Release 下载制品，断点续传、失败重试、主源不可达时自动切换镜像（jsdelivr / gh-proxy），全部文件经上游 SHA256 校验后生成 `bundle.lock.yaml` |
| `searke bundle verify <dir>` | ✅ 已实现 | 重新校验已有 bundle：必需文件存在 + 哈希一致；manifest 中未下载的可选资产忽略 |
| `searke image build <dir> -o <oci-dir>` | ✅ 已实现并真实验证 | 按平台各建一个子镜像，产出 OCI Image Index（OCI Image Layout），标注 `os`/`arch`/`io.searke.rke2.version`/`io.searke.cni` |
| `searke image push <ref> --from <oci-dir>` | ✅ 已实现 | `remote.WriteIndex` 推送整个 index，使用默认 docker keychain 认证 |
| `searke image save <ref-or-dir> -o <tar>` | ✅ 已实现并真实验证 | 导出为含 OCI Layout 的 tar 归档（离线搬运） |
| `searke image load <tar> -o <oci-dir>` | ✅ 已实现并真实验证 | 导入本地归档，还原 OCI Layout |
| `searke validate -f cluster.yaml` | ✅ 已实现 | 本地 schema + bundle 注解一致性 + SSH 连接 + 远端 preflight（只读） |
| `searke plan -f cluster.yaml` | ✅ 已实现 | validate 之上列出每个节点将执行的动作，不做远端变更 |
| `searke apply -f cluster.yaml` | ✅ 已实现 | 分阶段安装：preflight → 按节点架构分发 payload → 写 config.yaml → install.sh → 首 server → 其他 server → agent → Ready；阶段落盘可断点续跑，已属其他集群的节点会拒绝重置 |
| `searke status -f cluster.yaml` | ✅ 已实现 | 读取本地 apply 状态 + 从首 server 拉取 `kubectl get nodes` 实时视图 |
| `searke upgrade -f cluster.yaml` | ✅ 已实现 | 滚动升级：server 逐台先于 agent；分发新 payload → 重跑 install.sh → 重启服务 → 等 Ready；`apply` 检测到版本漂移会引导改用本命令 |
| `searke snapshot save/list/pull/restore` | ✅ 已实现 | etcd 快照工作流：首 server 上 `rke2 etcd-snapshot save`、列举、SFTP/`sudo cat` 拉回本地、cluster-reset 恢复（需 `--yes`，其余 server 需经 apply 重入） |

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

## 安全约定

- OCI 镜像内不含 SSH 私钥、集群 token、kubeconfig
- token 仅经 SSH 在内存中传递，不写入日志和状态文件
- 状态文件 `.searke/state-<cluster>.json` 权限 0600

## 远端节点布局

```text
/opt/searke/install.sh            上游安装脚本
/opt/searke/artifacts/            INSTALL_RKE2_ARTIFACT_PATH 指向的制品目录
/etc/rancher/rke2/config.yaml     searke 渲染的节点配置
/var/lib/rancher/searke-staging/  SFTP 上传暂存区（传输后清理）
```

## 测试

```bash
go test ./...
```

单元测试覆盖：schema 校验、资产命名、sha256sum 解析、VerifyPlatform、
httptest 假 Release 端到端 fetch（含断点续传）、OCI build/extract/save/load
回环、config 渲染、状态机。真实环境已验证：`v1.36.5+rke2r1` 双架构 fetch +
verify + build + save + load 全部通过。

## 故障排查

- **下载卡死/i/o timeout**：GitHub Release 与 raw 域名在部分网络不可达。下载器对每个资产按 主源 → gh-proxy.com → ghproxy.net 轮换重试；全部失败时错误会列出尝试过的 URL。`bundle.lock.yaml` 记录实际生效的源。
- **`sha256 mismatch`**：上游清单与文件不符（串包或损坏）。删除对应文件重跑 fetch；校验未过不会生成镜像。
- **SSH dial 失败/known_hosts**：节点必须已在 `~/.ssh/known_hosts`（先手动 `ssh` 一次或 `ssh-keyscan`）；密钥需无密码或在 ssh-agent 中。
- **`passwordless sudo required`**：`spec.ssh.sudo: true` 时要求 `sudo -n` 可用；否则用 root 用户。
- **节点已属其他集群**：apply/preflight 发现远端 `config.yaml` 的 `server:` 与本集群注册地址不同会拒绝继续，不会自动 reset。
- **中断续跑**：`.searke/state-<cluster>.json` 记录每节点阶段；重跑 `apply` 自动跳过已完成阶段。版本不一致的状态文件需 `upgrade` 或删除重建。
- **kubeconfig**：`ssh <user>@<首server> 'sudo cat /etc/rancher/rke2/rke2.yaml'`，把 server 地址换成 `apiAddress`。

## 已知限制 / 待办

- `image push` 代码已实现但未在真实 registry 验证
- `apply`/`plan`/`upgrade`/`snapshot`/`status` 已实现完整逻辑，尚无真实多节点环境端到端验证
- `snapshot restore` 仅覆盖首 server 的 cluster-reset；其余 server 需 `apply` 重入
- 首期节点 OS 限定 Ubuntu 22.04 由 preflight 检查
- 节点增删、CNI profile 扩展未做（计划明确缓议）
