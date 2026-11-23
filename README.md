# searke

一个命令行工具，用来**安装和运维 RKE2 Kubernetes 集群**，特别适合内网或离线环境。

- **一个二进制文件就够**：目标机器不需要装 Docker、Python、Ansible
- **x86 和 ARM 都能用**：同一个安装包同时包含 `amd64` 和 `arm64`，混合架构的集群也可以
- **有现成的安装包**：直接引用下面的公共镜像就能装，不用自己下载、打包
- **装完还能继续管**：扩容、缩容、升级、备份恢复、证书、批量执行命令，命令风格和 Sealos 相近

## 工作方式

```text
          你的电脑 / 跳板机（运行 searke）
                │
                │  1. 读取 cluster.yaml（集群里有哪些机器）
                │  2. 拉取 RKE2 安装包（公共镜像 / 本地文件）
                │  3. 通过 SSH 把安装包发到各节点并安装
                ▼
   ┌──────────┐   ┌──────────┐   ┌──────────┐
   │ server 1 │   │ server 2 │   │ agent 1  │   ← 节点不需要能上网
   └──────────┘   └──────────┘   └──────────┘
```

运行 searke 的那台机器需要能访问安装包，被安装的节点不需要联网。

---

## 1. 安装 searke

从 [Releases](https://github.com/jinteng123/searke/releases) 下载对应平台的文件：

```bash
# Linux x86_64（ARM 机器把 amd64 换成 arm64；macOS 用 darwin-arm64 / darwin-amd64）
curl -LO https://github.com/jinteng123/searke/releases/latest/download/searke-linux-amd64
sudo install -m 755 searke-linux-amd64 /usr/local/bin/searke
searke version
```

## 2. 可以直接使用的 RKE2 安装包

已上传到阿里云镜像仓库，**公开，无需登录**：

| 镜像地址 | 包含架构 |
| --- | --- |
| `registry.cn-hangzhou.aliyuncs.com/searke/rke2:v1.36.5-rke2r1` | amd64 + arm64（推荐，任何机器都能用） |
| `registry.cn-hangzhou.aliyuncs.com/searke/rke2:v1.36.5-rke2r1-amd64` | 只有 amd64，体积约一半 |
| `registry.cn-hangzhou.aliyuncs.com/searke/rke2:v1.36.5-rke2r1-arm64` | 只有 arm64，体积约一半 |

格式为 `registry.cn-hangzhou.aliyuncs.com/searke/rke2:<版本号>`。网络插件为 canal。

查看安装包内容：

```bash
searke image inspect registry.cn-hangzhou.aliyuncs.com/searke/rke2:v1.36.5-rke2r1
```

---

## 3. 用法一：单机安装（一条命令）

在**要安装 Kubernetes 的那台机器上**执行：

```bash
sudo searke run registry.cn-hangzhou.aliyuncs.com/searke/rke2:v1.36.5-rke2r1
```

完成后即可直接使用：

```bash
kubectl get nodes        # kubeconfig 已自动写入 ~/.kube/config
crictl ps                # kubectl/crictl/ctr/rke2 已链接到 /usr/local/bin
```

说明：

- 需要 root 权限
- 节点名默认取主机名，集群地址默认取本机主网卡 IP（可以用 `--address` 指定）
- 生成的配置保存在 `/root/.searke/<集群名>/cluster.yaml`，之后可以拿它做扩容、升级、备份等操作
- 中途失败时，再执行一次同样的命令会接着上次的进度继续
- 机器需要通过代理上网时：`sudo HTTPS_PROXY=http://代理:端口 searke run ...`
- 安装完成后节点可直接使用 `kubectl`、`crictl`、`ctr`、`rke2`（已链接到 /usr/local/bin）；`rke2.yaml` 会自动放进 root 和执行 sudo 用户的 `~/.kube/config`（指向其他集群的已有 config 不会被覆盖，会另存为 `config-<集群名>`；指向本集群的旧 config 视为过期并直接替换）
- `--dry-run` 只做检查、列出将要执行的步骤，不改动任何东西

---

## 4. 用法二：多节点集群

### 4.1 准备机器

每个节点需要满足：

1. 可以用 **SSH 密钥或密码**登录（密码用 `--password`，仅当次生效、不落盘），登录用户**免密 sudo**，或者直接用 root
2. 主机密钥：首次连接会自动信任并写入 `~/.ssh/known_hosts`（与 OpenSSH `accept-new` 相同，之后密钥变更仍会拒绝连接）。不信任首次连接时加全局参数 `--strict-host-key`，或先手动 `ssh-keyscan`

3. 主机名互不相同；端口 6443、9345、10250 没有被占用
4. SELinux 为 Permissive 或 Disabled

操作系统：Ubuntu 22.04 / 24.04 已验证，其他 systemd 发行版通常也可以（检查时会给出警告）。

### 4.2 方式一：一条命令生成并安装（类似 `sealos run`）

```bash
searke run registry.cn-hangzhou.aliyuncs.com/searke/rke2:v1.36.5-rke2r1 \
  --masters 10.0.0.11,10.0.0.12,10.0.0.13 \
  --nodes 10.0.0.21,10.0.0.22 \
  --ssh-user root --ssh-key ~/.ssh/id_rsa
```

cluster.yaml 会自动生成在 `~/.searke/searke/cluster.yaml`（可用 `--name`/`--home` 更改），masters 的第一台会成为引导节点。SSH 密码登录用 `--password`（仅本次生效，不会写入文件）。`--dry-run` 只生成配置和计划。

### 4.3 方式二：手工编写 cluster.yaml

可以用 `searke gen cluster -o cluster.yaml` 生成一份带注释的模板，或者参考下面的例子：

```yaml
apiVersion: searke.io/v1alpha1
kind: Cluster
metadata:
  name: my-cluster
spec:
  rke2:
    bundle: registry.cn-hangzhou.aliyuncs.com/searke/rke2:v1.36.5-rke2r1
  ssh:
    user: ubuntu                     # 登录用户
    identityFile: ~/.ssh/id_ed25519  # 私钥
    sudo: true                       # 非 root 用户设为 true
  endpoints:
    registrationAddress: 10.0.0.11:9345   # 没有负载均衡时，填第一台 server 的 IP
    apiAddress: 10.0.0.11:6443
  servers:                           # 控制平面节点，数量必须是 1、3、5 等奇数
    - { name: master-1, address: 10.0.0.11 }
    - { name: master-2, address: 10.0.0.12 }
    - { name: master-3, address: 10.0.0.13 }
  agents:                            # 工作节点，可以不写
    - { name: worker-1, address: 10.0.0.21 }
```

### 4.4 安装

```bash
searke validate -f cluster.yaml   # 检查配置、SSH 连接、节点环境（不做改动）
searke plan     -f cluster.yaml   # 列出每个节点将执行的步骤（不做改动）
searke apply    -f cluster.yaml   # 开始安装
```

`apply` 可以重复执行：已经装好的节点会自动跳过，中途失败时再次执行会从断点继续。

安装时会实时显示制品解包和上传进度、`install.sh` 输出及 RKE2 systemd 日志；等待服务或节点 Ready 时每 5 秒报告一次状态。`run`/`apply`/`upgrade`/`add`/`delete`/`uninstall`/`snapshot`/`cert` 等变更类命令的完整日志保存在 `~/.searke/logs/<集群名>/`（保留最近 10 份），启动时会打印本次日志文件路径。

### 4.5 用 kubectl 访问

```bash
searke kubeconfig -f cluster.yaml -o ~/.kube
export KUBECONFIG=~/.kube/my-cluster-kubeconfig
kubectl get nodes

# apiAddress 或 bootstrap server 宕机时，从任意存活 server 拉取并把入口写成它
# （可以写 IP、IP:端口 或节点名）：
searke kubeconfig -f cluster.yaml --server 10.0.0.12:6443
```

另外，安装时已自动在每个节点上把 `rke2`/`kubectl`/`crictl`/`ctr` 链接到 `/usr/local/bin`，并在 server 节点为 root 和 SSH 登录用户写好 `~/.kube/config`——登录到节点可以直接 `kubectl`。

---

## 5. 日常运维

下面的命令都使用 `-f cluster.yaml` 指定集群。单机安装的集群使用 `/root/.searke/<集群名>/cluster.yaml`。不写 `-f` 且当前目录没有 cluster.yaml 时，会自动使用 `~/.searke/` 下唯一的生成配置（有多个集群时仍需 `-f` 指定）。

| 要做什么 | 命令 |
| --- | --- |
| 查看集群状态 | `searke status -f cluster.yaml` |
| 增加工作节点 | `searke add --nodes 10.0.0.22,10.0.0.23`（`-f` 可省略） |
| 增加控制平面节点 | `searke add --masters 10.0.0.14,10.0.0.15`（增加后总数仍须是奇数） |
| 删除节点（先预览） | `searke delete -f cluster.yaml --nodes worker-1 --dry-run` |
| 删除节点 | `searke delete -f cluster.yaml --nodes worker-1 --yes` |
| 备份 etcd | `searke snapshot save -f cluster.yaml --name before-change` |
| 查看备份 | `searke snapshot list -f cluster.yaml` |
| 下载备份到本地 | `searke snapshot pull -f cluster.yaml <快照文件名> -o ./backups` |
| 从备份恢复 | `searke snapshot restore -f cluster.yaml <快照文件名> --yes` |
| 在所有节点执行命令 | `searke exec -f cluster.yaml -- uptime` |
| 在指定节点执行命令 | `searke exec -f cluster.yaml --nodes master-1,worker-1 -- df -h` |
| 上传文件到所有节点 | `searke scp -f cluster.yaml ./a.sh /usr/local/bin/a.sh` |
| 同步各节点 /etc/hosts | `searke hosts sync -f cluster.yaml` |
| 查看证书到期时间 | `searke cert check -f cluster.yaml` |
| 证书增加域名或 IP | `searke cert san -f cluster.yaml k8s.example.com 10.0.0.100 --yes` |
| 续期证书 | `searke cert rotate -f cluster.yaml --yes` |
| 卸载整个集群 | `searke uninstall -f cluster.yaml --yes` |

`add` 和 `delete` 也可以写成 `--servers` 和 `--agents`。`delete` 删除的节点可以写节点名，也可以写 IP。

`add` 的行为：

- 只连接本次新增的节点和引导 server，不会去连其他已有节点
- `--password` 只用于本次新增的节点，每台节点可以用各自的密码一台台添加；用密码连上的节点会自动授权一把 searke 生成的集群密钥（保存为 `<集群目录>/.searke/id_ed25519` 并写入 `spec.ssh.identityFile`），之后的 `apply`/`exec`/`uninstall` 等命令直接走密钥，不需要再传密码。显式给了 `--ssh-key` 时推送的是那把 key 的公钥
- 新增的工作节点以**机器的主机名**作为节点名（主机名不合法或与已有节点重名时退回 `agent-NN`）；新增的控制平面节点仍使用 `server-NN`，避免名字排序改变引导节点
- `--password-stdin` 从 stdin 读密码，避免密码出现在进程列表和 shell 历史

### 升级 RKE2

1. 把 `cluster.yaml` 里的 `spec.rke2.bundle` 改成新版本的安装包
2. 执行：

   ```bash
   searke upgrade -f cluster.yaml
   ```

searke 会先备份 etcd，然后先升级 server、再升级 agent，一次只升级一台：驱逐 Pod，安装新版本，等节点恢复后再继续下一台。每次只能升级一个小版本（例如 1.36 → 1.37），不支持降级。中途失败后再执行一次即可继续。

### 有风险的操作

`delete`、`snapshot restore`、`cert san/rotate`、`uninstall` 都需要加 `--yes` 才会执行。其中 `delete` 支持 `--dry-run`，可以先预览。删除节点时，控制平面节点数量必须保持奇数，第一台 server 不能删除。

---

## 6. 完全离线：自己制作安装包

适用于需要其他 RKE2 版本或其他网络插件，或者完全没有网络的环境。

```bash
# 1) 在能上网的机器上，声明要哪个版本
cat > bundle.yaml <<EOF
apiVersion: searke.io/v1alpha1
kind: RKE2Bundle
spec:
  version: v1.36.5+rke2r1      # 必须写完整版本号
  cni: canal                   # canal | calico | cilium | flannel | none
  platforms: [linux/amd64, linux/arm64]
EOF

# 2) 下载官方文件并校验 SHA256（双架构约 1.5GB；支持断点续传，可用 HTTPS_PROXY）
searke bundle fetch -f bundle.yaml -o rke2-bundle

# 3) 打包成镜像
searke image build rke2-bundle -o rke2-oci --compression gzip

# 4a) 导出为一个文件，拷贝到内网
searke image save rke2-oci -o rke2.oci.tar
#     在内网的 cluster.yaml 里写：bundle: ./rke2.oci.tar

# 4b) 或者推送到自己的镜像仓库
searke login registry.example.com -u admin --password-stdin
searke image push registry.example.com/infra/rke2:v1.36.5-rke2r1 --from rke2-oci
```

`spec.rke2.bundle` 支持以下三种写法：

- 镜像地址：`registry.cn-hangzhou.aliyuncs.com/searke/rke2:v1.36.5-rke2r1`
- 导出的文件：`./rke2.oci.tar`
- 打包目录：`./rke2-oci`

内网镜像仓库只支持 HTTP 时，在 `spec.rke2` 下加上 `insecure: true`。

没有镜像仓库、但想让其他机器拉取时，可以临时开一个只读仓库：

```bash
searke image serve rke2-oci --address 0.0.0.0:5000
```

这个仓库没有认证，只能在可信的内网中使用。

---

## 7. cluster.yaml 说明

| 字段 | 说明 |
| --- | --- |
| `metadata.name` | 集群名称 |
| `spec.rke2.bundle` | 安装包：镜像地址、`.oci.tar` 文件或目录 |
| `spec.rke2.cni` | 可选，必须与安装包一致，默认 canal |
| `spec.rke2.version` | 可选，写了就必须与安装包版本一致，用来防止用错安装包 |
| `spec.rke2.insecure` | 可选，镜像仓库走 HTTP |
| `spec.ssh.user` / `identityFile` / `port` / `sudo` | SSH 登录方式，端口默认 22 |
| `spec.endpoints.registrationAddress` | 节点加入集群时连接的地址（`IP:9345`）。有负载均衡或 VIP 时填它，没有时填第一台 server |
| `spec.endpoints.apiAddress` | 可选，kubectl 访问地址（`IP:6443`），会自动加入证书 |
| `spec.servers` / `spec.agents` | 节点列表：`name`、`address`，以及可选的 `labels` |
| `spec.rke2Config` | 原样写入 RKE2 的 `config.yaml`，例如 `cluster-cidr`、`service-cidr`、`tls-san`。`server`、`token`、`node-name`、`cni` 由 searke 管理，不能填 |

注意事项：

- **按名字排序后的第一台 server 是"引导节点"**。修改节点名时，不要让其他 server 的名字排到它前面（`apply` 检测到引导节点变化时会拒绝执行）
- 节点地址是运行 searke 的这台机器本身时（`127.0.0.1` 或本机 IP），会直接在本机执行，不走 SSH，可以和远程节点混用
- 安装进度保存在 cluster.yaml 同目录下的 `.searke/` 中，请不要删除

---

## 8. 常见问题

**SSH 报 `knownhosts: key is unknown`**
现在不会出现了——首次连接会自动信任并写入 `~/.ssh/known_hosts`。

**SSH 报 `knownhosts: key mismatch`**
节点重装后主机密钥变了，需要先删除旧记录：`ssh-keygen -R <IP>`，再重跑命令。节点没重装时报这个错，也可能是 known_hosts 里固定的是另一种类型的密钥（比如 ed25519 记录而协商出 ecdsa）——用 `ssh-keygen -F <IP>` 查看已记录的类型。

**`passwordless sudo required`**
登录用户需要配置免密 sudo，或者直接使用 root 用户。

**`required ports already in use`**
节点上已有其他程序占用 6443、9345、10250 或 8472 端口，需要先停掉。

**`node already joined a different cluster`**
节点上已经装过别的 RKE2 集群，searke 不会自动覆盖。请先在该节点上卸载。

**`downgrades are not supported` / `skips N minor versions`**
不支持降级。每次只能升一个小版本，需要跨多个版本时请逐级升级。

**kubelet 崩溃循环、`run cgroup v1` 报错**
节点还在用 cgroup v1（麒麟 V10、CentOS 7、Alinux 2 等），而 Kubernetes 1.35 起 kubelet 默认拒绝在 cgroup v1 上启动（1.34 及以前只告警）。searke 会自动兼容：目标版本 ≥ v1.35 时，给 cgroup v1 节点的 kubelet 加上 `fail-cgroupv1=false`，`apply`/`upgrade` 时打印告警。如果你在 `rke2Config.kubelet-arg` 里显式写了 `fail-cgroupv1=true`，searke 会在安装前报错。cgroup v1 已被上游弃用，长期还是建议切到 v2：`grubby --update-kernel=ALL --args="systemd.unified_cgroup_hierarchy=1"` 后重启。用旧版 searke 装出这个错误的节点，升级 searke 后重跑 `searke apply` 并不会改写已经写好的配置；需要先 `uninstall` 再重新 `run`/`apply`，或者手工在 `/etc/rancher/rke2/config.yaml` 的 `kubelet-arg` 里加 `fail-cgroupv1=false`，然后 `systemctl restart rke2-server`。

**下载很慢**
设置代理：`export HTTPS_PROXY=http://代理:端口`。使用 `sudo` 运行时，需要写成 `sudo HTTPS_PROXY=... searke ...`。

**执行到一半失败了**
修复问题后，重新执行同一条命令即可。`apply`、`upgrade`、`delete`、`run` 都会从断点继续。

---

## 9. 命令速查

```text
安装      run  validate  plan  apply  status
节点      add  delete  uninstall  exec  scp  hosts sync  hostname
升级备份  upgrade  snapshot save|list|pull|restore
证书      cert check|san|rotate   kubeconfig
安装包    bundle fetch|verify   image build|push|pull|save|load|inspect|serve
其他      login  logout  gen  version  completion
```

每个命令都可以用 `searke <命令> --help` 查看详细参数。

与 Sealos 的对应关系：`run`、`add`、`delete`、`exec`、`scp`、`hosts`、`hostname`、`login`、`gen`、`version` 用法相同；`reset` 对应 `uninstall`，`cert --alt-names` 对应 `cert san`，`build`、`push`、`pull`、`save`、`load` 对应 `image` 下的同名子命令。

---

## 10. 从源码构建

需要 Go 1.27 及以上：

```bash
git clone https://github.com/jinteng123/searke.git && cd searke
go build -o searke ./cmd/searke
# 交叉编译 Linux 版本
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o searke-linux-amd64 ./cmd/searke
go test ./...
```

Docker 镜像：

```bash
docker build -t searke .
docker run --rm -v ~/.ssh:/home/searke/.ssh:ro -v "$PWD":/work -w /work searke status -f cluster.yaml
```

在容器中运行时，cluster.yaml 引用的本地安装包需要按相同的绝对路径挂载进容器。`searke run` 不适合在容器中使用。

## 文档

- [开发计划与实现说明](docs/DEVELOPMENT_PLAN.md)：设计、验证记录、已知限制
- [Sealos 对齐计划](docs/SEALOS_PARITY_PLAN.md)
- 真机测试环境：[`testlab/`](testlab/)（Parallels Ubuntu 24.04 arm64，3 台 server）
