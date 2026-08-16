# CPA 整套部署日期时间版本号标准

本文约束 CPA 整套部署中的三个运行组件：

- Core：`cli-proxy-api`
- Manager：`cpa-manager-plus`
- Agent：`cpamp-agent`

## 1. 版本格式

内部构建与服务器部署统一采用：

```text
YYYYMMDD-HHMMSS
```

- `YYYYMMDD`：源码 commit 的完整日期，例如 `20260816`。
- `HHMMSS`：源码 commit 的时分秒，例如 `143205`。
- 完整示例：`20260816-143205`。
- 默认时区固定为 `Asia/Shanghai`，也可通过 `CPA_VERSION_TIMEZONE` 显式覆盖。
- 时间取源码 commit 时间，不取镜像构建时间，保证同一源码快照重复构建时版本号稳定。

禁止使用仅包含功能名称、`latest`、`dev`、不带日期的时间或旧的 `MMDD-SHA4` 格式作为生产镜像版本。

## 2. 生成命令

Core 仓库：

```bash
cd /Users/frode.luo/project/CLIProxyAPI-Pro
VERSION="$(./scripts/compact-version.sh)"
```

Manager/Agent 仓库：

```bash
cd /Users/frode.luo/project/CPA-Manager-Pro
VERSION="$(./bin/compact-version.sh)"
```

Manager 和 Agent 由同一仓库、同一 Dockerfile、同一源码快照构建，因此正常情况下使用同一个版本号；组件名称负责区分二者。

管理面板必须使用同一个 Manager 日期时间版本号。前端构建必须显式传入
`VERSION="$VERSION"`；Vite 的本地兜底只读取 commit 时间，禁止使用 `git describe`
或任何上游标签。Manager Server 会校验外置/内嵌 `management.html` 是否包含自己的
版本号；不匹配的外置面板会回退到内嵌面板，内嵌面板仍不匹配则健康发布失败。

如需核对 UTC 表示，可执行：

```bash
CPA_VERSION_TIMEZONE=UTC ./bin/compact-version.sh
```

生产发布仍统一使用默认的 `Asia/Shanghai`。

## 3. 构建注入

每次构建必须同时注入以下三个值：

```bash
VERSION="$(./scripts/compact-version.sh)" # Manager 仓库使用 ./bin/compact-version.sh
COMMIT="$(git rev-parse HEAD)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
```

Core 示例：

```bash
docker build \
  --build-arg VERSION="$VERSION" \
  --build-arg COMMIT="$COMMIT" \
  --build-arg BUILD_DATE="$BUILD_DATE" \
  -t "local/cli-proxy-api:$VERSION" \
  -f Dockerfile .
```

Manager/Agent 示例：

```bash
docker build \
  --build-arg VERSION="$VERSION" \
  --build-arg COMMIT="$COMMIT" \
  --build-arg BUILD_DATE="$BUILD_DATE" \
  -t "local/cpa-manager-plus:$VERSION" \
  -f Dockerfile.manager-server .
```

## 4. 版本展示与查询

- Core：`GET /healthz` 返回 `version`、`commit`、`buildDate`，并返回 `X-CPA-VERSION`、`X-CPA-COMMIT`、`X-CPA-BUILD-DATE` Header。
- Manager：`GET /health` 和鉴权后的 `GET /status` 返回 `version`、`commit`、`buildDate`。
- Agent：`GET /health` 和鉴权后的 `GET /agent/info` 返回 `version`。
- Manager Dashboard 的系统概览同时展示 Manager、Core、Agent 三个版本。
- Docker 镜像写入 OCI `version`、`revision`、`created` 标签。

## 5. 发布前校验

发布前必须记录并核对：

1. 三个组件的目标版本号和完整 commit。
2. Docker 镜像标签与镜像 OCI version 标签一致。
3. Compose 中引用的镜像标签与实际运行容器镜像一致。
4. Core、Manager、Agent 启动后接口返回的版本与目标版本一致。
5. Manager 和 Agent 使用同一镜像时，二者版本必须一致。
6. 任一版本、镜像或 Compose 配置存在漂移时，停止后续槽位切换并先消除漂移。

版本记录示例：

```text
Core:    20260816-143205  commit=COMMIT...
Manager: 20260816-143412  commit=COMMIT...
Agent:   20260816-143412  commit=COMMIT...
```

本标准只定义版本生成、注入、展示和校验方式；生产切换仍按零停机发布手册逐个 Gateway/服务执行。
