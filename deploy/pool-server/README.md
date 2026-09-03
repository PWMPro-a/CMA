# CPA Pool Server 发布包

这是面向客户服务器的 CPA + CPA Manager Plus 完整部署模板。发布包只使用公开的
CPA release 镜像和 CPAMP GHCR 镜像，不依赖本地 `local/*` 镜像，也不会把商城客户端
密钥提交到仓库。

## 快速安装

在服务器上把本目录放到独立目录（例如 `/opt/cpa-pool`），然后执行：

```bash
chmod +x bootstrap.sh preflight.sh
# 将商城为本实例签发的 client secret 放入本地文件（单行）
mkdir -p -m 700 secrets
install -m 600 /path/to/storefront-issued-secret secrets/cpa-license-client-secret
./bootstrap.sh
```

`CPA_LICENSE_PROVIDER=shop666` 或 `CPA_LICENSE_API_BASE_URL` 使用
`p.666ttt.net` 时，client secret 是启动前置条件。`bootstrap.sh` 不会生成随机值；
如果文件缺失、为空、不可读或权限不是 600，脚本会在启动前停止并只提示文件路径，
不会输出 secret 内容。先注入商城签发且与 client ID 匹配的值，再重新执行脚本。

脚本会：

1. 从 `.env.example` 创建 `.env`，将相对路径转换为绝对路径；
2. 生成并保存 CPAMP Admin Key、CPA Management Key、Agent Token 和演示 API Key；
3. 创建 CPA 数据、授权租约、Manager 数据和备份目录；
4. 首次安装时从 `config.yaml.template` 生成 `data/cpa/config.yaml`；已有配置和授权状态默认保留；
5. 执行 preflight，校验端口、镜像、公钥、secret 文件和 Compose 插值；
6. 使用显式 `--env-file` 拉取固定版本镜像并启动服务，等待 CPA、Agent、CPAMP 健康检查。

面板地址：`http://<服务器地址>:18317/management.html`。密钥会写入权限为 600 的本地
`.env` 和 `secrets/` 文件（Compose 优先从 `secrets/` 读取）；脚本不会在输出中显示完整值。

## 常用命令

```bash
# 只生成配置，不启动服务
./bootstrap.sh --render

# 只执行文件和配置预检
./preflight.sh --skip-docker

# 拉取镜像但不启动
./bootstrap.sh --pull

# 查看或停止当前 Compose 项目
./bootstrap.sh --status
./bootstrap.sh --down

# 有意修改模板配置时，先备份再重生成
./bootstrap.sh --force-config
```

所有 Compose 操作都等价于：

```bash
docker compose --env-file /path/to/.env -f /path/to/compose.yml <command>
```

请不要用未指定 env 文件的 `docker compose up`，否则路径和授权配置可能来自当前 shell，
从而启动错误的实例。

## 端口、路径和镜像

`.env` 中可调整以下值：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `CPA_IMAGE` | `ghcr.io/abc124774961/cli-proxy-api-cpa:v7.2.148-cpa.3` | 已验证的公开 CPA 发布版本；建议固定 tag 或 digest |
| `CPAMP_IMAGE` | `ghcr.io/seakee/cpa-manager-plus:v1.12.8` | 已验证的 Manager/Agent 版本；升级时显式修改并先做预检 |
| `CPA_PORT` | `8317` | CPA host-network 监听端口 |
| `CPAMP_PORT` | `18317` | 面板对外端口 |
| `CPAMP_AGENT_PORT` | `18417` | Agent host-network 端口 |
| `CPA_DATA_DIR` | `./data/cpa` | CPA 配置、auths、日志和 license 状态 |
| `CPAMP_DATA_DIR` | `./data/manager` | Manager SQLite 与 `data.key` |
| `CPAMP_STACK_ROOT` | `.` | Agent 管理的 Compose 根目录 |
| `CPAMP_BACKUP_ROOT` | `./backups` | 备份归档目录 |

CPA、Agent 使用 host network，因此三个监听端口必须互不相同。CPAMP 容器通过
`host.docker.internal:host-gateway` 访问 CPA 和 Agent。

容器名和网络名也能调整，以便在同一台机器隔离多个 Compose 项目：
`CPA_CONTAINER_NAME`、`CPAMP_CONTAINER_NAME`、`CPAMP_AGENT_CONTAINER_NAME`、
`CPAMP_NETWORK_NAME`。自定义名称适用于独立 Compose 运行；CPAMP 的容器操作升级、
网络标准化接口仍以标准名称 `cli-proxy-api`、`cpa-manager-plus`、`cpamp-agent` 和
`cpamp-cpa_default` 为目标，使用这些接口时请保留默认名称。

## 商城授权和宽限期

`CPA_LICENSE_PUBLIC_KEY` 是公开的 Ed25519 发布公钥，必须保留。商城签发的授权和宽限
租约写入 `CPA_LICENSE_STATE_DIR`，容器重建时会保留同一个目录及 installation identity。

`CPA_LICENSE_GRACE_PERIOD=6h` 只是刷新暂时失败时的本地回退值；实际有效宽限期由商城
签名租约控制，客户修改本地配置不会把租约变成无限期。商城客户端 ID 可按商城配置填写；
client secret 对 `shop666` 或 `p.666ttt.net` 场景是必填项：

- 推荐将客户端密钥放在 `CPA_LICENSE_CLIENT_SECRET_HOST_PATH` 指定的本地文件（权限 600）；
- 旧版只设置 `CPA_LICENSE_CLIENT_SECRET` 或 `CPA_LICENSE_CLIENT_SECRET_FILE` 的配置仍能
  被 bootstrap 识别，并迁移到本地 secret 文件；
- 外部商城场景缺少可读 secret 文件时，preflight/bootstrap 会阻断并提示注入匹配的商城
  secret；不会创建伪 secret 或空的可用凭据；
- 不要把真实 `.env`、`secrets/`、`data/` 或 `backups/` 提交到 Git；模板目录已通过
  `.gitignore` 忽略这些运行时文件。

如果使用完全本地的授权 provider 和非商城 API 地址，secret 文件仍会按 mode 600
创建，保持旧版 key-only 部署流程。

## 升级建议

1. 先执行 `./preflight.sh` 并备份 `data/cpa`、`data/manager`、`secrets/`；
2. 只修改 `CPA_IMAGE` 或 `CPAMP_IMAGE` 到已验证的固定 tag/digest；不要直接使用 `latest`；
3. 先 `./bootstrap.sh --pull`，确认拉取成功后再 `./bootstrap.sh --no-pull`；
4. 升级后检查 `docker compose --env-file .env -f compose.yml ps`、`/healthz` 和面板登录。

脚本默认不会覆盖已有 CPA 配置或授权租约。需要重建配置时，`--force-config` 会先把旧
配置保存到 `backups/config-<timestamp>.yaml`。
