# CPAMP Container Ops v1

本文定义下一版 CPAMP 的容器管理能力。目标不是替代 Portainer，而是把 CPA 相关服务做成可部署、可迁移、可备份、可恢复、可升级和可回滚的一套单机 Docker 运维栈。

Core、Manager、Agent 的构建版本、镜像标签和发布校验统一遵循 [CPA 整套部署日期时间版本号标准](compact-deployment-versioning.zh-CN.md)。

## 产品定位

- **核心目标**：管理 CPA 栈生命周期，包括 CPA、CPAMP、认证文件、配置、用量数据库和 Docker 内网。
- **NewAPI 边界**：只做拓扑识别、内网连通测试和渠道 Base URL 建议，不备份或修改 NewAPI 数据。
- **第一版形态**：单机 Docker；不支持多机调度、Kubernetes 或通用容器平台能力。
- **默认体验**：首次进入只读观测模式，先展示容器、网络、端口、数据目录和风险，再由用户确认是否接管。

## 技术架构

```text
浏览器
  -> CPAMP Manager Server
      -> /v0/management/container-ops/*
      -> cpamp-agent（Docker 内网）
          -> Docker Engine API
          -> /opt/cpamp/stacks/cpa
          -> /opt/cpamp/backups
```

CPAMP 主服务不直接挂载 Docker socket。所有 Docker 操作通过 `cpamp-agent` 完成。Agent 只监听 Docker 内网，不暴露公网端口，并使用共享 token 与 Manager Server 鉴权。

## 标准资源

- Compose project：`cpamp-cpa`
- Docker network：`cpamp-cpa_default`
- CPA 服务名：`cli-proxy-api`
- CPAMP 服务名：`cpa-manager-plus`
- Agent 服务名：`cpamp-agent`
- Stack 根目录：`/opt/cpamp/stacks/cpa`
- 备份目录：`/opt/cpamp/backups`
- NewAPI 推荐内网地址：`http://cli-proxy-api:8317/v1`

## Agent 安全模型

- Agent API 必须带共享 token。
- Agent 默认只能操作带 `com.cpamp.managed=true` 标签的容器、网络和卷。
- 禁止提供通用 Docker 控制台能力，例如任意 `exec`、任意 bind mount、任意容器删除。
- 所有写操作必须生成审计日志。
- 破坏性操作前，Manager Server 必须生成回滚备份。

## API 契约

容器管理接口挂在 Manager Server 的管理命名空间下，需要管理员密钥：

| 接口 | 用途 |
|---|---|
| `GET /v0/management/container-ops/info` | 返回功能模式、Agent 配置状态、标准资源名和推荐 NewAPI Base URL |
| `GET /v0/management/container-ops/agent` | 检查 Agent 是否已配置和是否可达 |
| `GET /v0/management/container-ops/discover` | 发现 CPA/CPAMP/NewAPI 相关容器、网络和数据目录 |
| `POST /v0/management/container-ops/import` | 接管现有部署并生成 manifest/Compose 草案 |
| `POST /v0/management/container-ops/deploy` | 生成标准 CPA 栈部署预案；`apply=true` 可由 Agent 写入部署文件、拉取标准镜像或受控启动标准服务 |
| `POST /v0/management/container-ops/backup` | 创建 CPA 栈备份，归档 CPA `/app/data` 和可发现的 CPAMP `/data` |
| `POST /v0/management/container-ops/restore` | 基于备份 ID 生成恢复预检；`apply=true` 由 Agent 创建回滚备份后受控执行恢复 |
| `POST /v0/management/container-ops/network-standardize` | 基于备份 ID 预检或执行标准 CPA 网络创建与容器连接 |
| `GET /v0/management/container-ops/audits` | 返回最近 CPA 生命周期写操作审计记录 |
| `GET /v0/management/container-ops/upgrade-tasks` | 返回最近升级准备/后续异步升级任务状态 |
| `POST /v0/management/container-ops/upgrade-tasks/start` | 启动已准备任务的异步升级 runner；Manager 会创建并轮询 Agent 持久化升级 job，当前只执行 CPA 容器重建，CPAMP/Agent 重建延后 |
| `POST /v0/management/container-ops/upgrade` | 生成升级预检；`apply=true` 由 Agent 创建升级回滚备份并拉取标准镜像，容器重建延后到异步升级阶段 |
| `POST /v0/management/container-ops/rollback` | 基于回滚备份 ID 执行受控回滚 |

当前实现先落地 `info`、`agent`、`discover`、`import` 预案、`deploy` 预案、`backup` 备份、`restore` 预检/执行、`rollback` 独立回滚、`network-standardize` 受控网络标准化、`upgrade` 安全准备接口、`upgrade-tasks` 升级任务历史和升级任务启动控制面。`discover` 通过 Agent 调用 Docker Engine API 汇总容器、网络和镜像；`import` 只生成候选容器、manifest、Compose 草案、风险清单和下一步动作，不修改 Docker 资源；`deploy` 面向干净新机生成标准 CPA/CPAMP/Agent Compose 草案和部署步骤，如果发现非标准或非托管的 CPA/CPAMP/Agent 容器或标准网络归属冲突，会阻断执行建议，已存在的标准 CPAMP 托管服务可由部署状态机复用；`deploy apply=true` 默认通过 Agent 将 `compose.yml`、`stack.manifest.json` 和 `.env.example` 写入 `CPAMP_STACK_ROOT`，`action=pull_images` 时只允许拉取 manifest 中标准 CPA/CPAMP/Agent 服务对应的镜像，`action=start_services` 要求 `compose.yml`、`stack.manifest.json` 和包含非占位密钥的 `.env` 均已就绪，然后只创建/复用标准网络、数据卷和 CPA/CPAMP/Agent 容器并按 CPA -> Agent -> CPAMP 顺序启动，最后只做运行状态健康检查；`backup` 通过 Docker archive API 只读导出 CPA `/app/data` 和可发现的 CPAMP `/data`，写入 Agent 的 `CPAMP_BACKUP_ROOT` 目录；`restore` 根据备份 ID 读取 `manifest.json`、校验归档文件、检查当前 CPA/CPAMP 目标容器，默认只返回预检和步骤，`apply=true` 时先创建 `rollback-cpa-*` 回滚备份，再按 CPAMP -> CPA 停止、CPA/CPAMP archive 恢复、CPA -> CPAMP 启动和运行状态健康检查执行受控恢复；`rollback` 要求传入已存在的回滚备份 ID，执行前再创建 `pre-rollback-cpa-*` 安全备份，然后复用同一条 CPA/CPAMP 受控归档应用流程；`network-standardize` 要求传入备份 ID，只允许创建标准 bridge 网络 `cpamp-cpa_default` 并将已识别的 CPA/CPAMP/Agent/NewAPI 容器连接到该网络，不提供任意容器、任意网络或删除能力；`upgrade` 默认只做预检，要求标准 `cpamp-cpa_default` 网络和 CPAMP 托管的 CPA/CPAMP 目标容器存在，且目标镜像只能来自 `seakee/cli-proxy-api` 和 `seakee/cpa-manager-plus` 仓库，`apply=true` 时先创建 `upgrade-cpa-*` 回滚备份，再拉取允许的升级镜像，并把同步容器重建标记为已跳过；`upgrade-tasks/start` 会把已准备任务推进到 `running`，创建 Agent 持久化升级 job，并轮询 job 直到 `completed/blocked/failed`。当前 Agent job 只重建标准 CPA 容器：先校验回滚备份，再停止旧 CPA、改名保留旧容器、创建并启动新版 `cli-proxy-api`，健康检查失败时尝试把旧容器改回并启动；CPAMP/Agent 自身重建仍延后。

Manager Server 镜像同时包含 `cpa-manager-plus` 和 `cpamp-agent` 两个二进制，Compose 草案中的 Agent 服务通过同一镜像执行 `cpamp-agent` 命令启动。

## Manager 生命周期锁

Manager Server 在进程内维护单实例 CPA 生命周期状态，`GET /v0/management/container-ops/info` 会返回当前 `lifecycle`。备份、恢复执行、独立回滚、网络标准化执行、升级准备，以及部署文件写入、镜像拉取、服务启动这些写入阶段会进入 `in_progress`，同一时间只允许一个 CPA 生命周期写操作执行。

如果已有写操作仍在执行，新的写操作会被 Manager 拒绝并返回 `409 Conflict`；只读发现、接管预案、部署预案、恢复预检和网络预检不占用该锁。当前锁是 Manager 进程本地状态，不是跨多个 Manager 实例的分布式锁；生产部署应保持单 Manager 写入口，或在后续版本把状态持久化/分布式化。

## 审计历史

Manager Server 会把每次生命周期写操作持久化到 SQLite 的 `container_ops_audits` 表。记录内容包括 operation id、操作类型、阶段、状态、备份 ID、Agent 地址、开始/结束时间、耗时、错误信息，以及经过摘要化的请求和结果。当前审计覆盖：备份、恢复执行、独立回滚、网络标准化执行、部署文件写入、部署镜像拉取和部署服务启动。

审计接口 `GET /v0/management/container-ops/audits?limit=20` 只读返回最近记录；前端容器运维页展示最近审计，方便确认谁在什么时候执行了哪类写操作、是否成功、关联哪个备份。审计记录由 Manager 写入，不依赖 Agent 直接访问数据库；如果 Manager 无法创建审计记录，写操作不会继续执行，避免出现无审计的受控变更。升级准备的审计 operation 为 `upgrade_prepare`，结果摘要会记录目标镜像、回滚备份 ID、镜像拉取数量和阻断检查数量。

## 升级任务历史

Manager Server 会把 `upgrade apply=true` 创建为持久化升级任务，写入 SQLite 的 `container_ops_upgrade_tasks` 表，并复用 lifecycle operation id 作为 `taskId`。任务记录包含目标 CPA/CPAMP 镜像、回滚备份 ID、当前状态、阶段、下一步动作、Agent 地址、开始/结束时间、错误信息和摘要化结果。

当前阶段的升级任务覆盖安全准备和异步 runner 控制面：`preparing -> prepared/blocked/failed`，以及 `prepared -> running -> completed/blocked/failed`。准备成功时 `nextAction` 为 `start_async_recreate`，前端可调用 `POST /v0/management/container-ops/upgrade-tasks/start` 启动 runner；runner 会创建 `upgrade_async` 生命周期审计，调用 Agent `POST /upgrades/cpa/jobs` 创建持久化升级 job，再轮询 `GET /upgrades/cpa/jobs/{jobId}`。Agent 会把 job JSON 写入 `CPAMP_BACKUP_ROOT/upgrade-jobs`，重启后加载已完成 job；如果发现 `queued/running` 这类非终态 job，会标记为 `failed` 并设置 `nextAction=restart_upgrade_task`，避免 Manager 继续等待已经丢失执行上下文的 job。当前 Agent job 只执行 CPA-only 重建，任务结果摘要会记录 `agentJobId`。接口 `GET /v0/management/container-ops/upgrade-tasks?limit=20` 只读返回最近任务，前端容器运维页展示任务历史。当前版本仍不重建 CPAMP/Agent 自身。

## 状态机

- 部署：`precheck -> render compose -> pull image -> create resources -> start -> healthcheck -> commit`
- 备份：`precheck -> stop CPAMP/CPA -> archive data -> restart -> verify archive`
- 恢复：`precheck -> rollback backup -> stop -> restore -> start -> healthcheck -> commit/rollback`
- 升级：`precheck -> rollback backup -> pull image -> recreate -> healthcheck -> commit/rollback`

## 受控写操作

当前已开放的写操作都必须由 Agent 执行，且不属于破坏性操作：

- 部署文件渲染：`deploy apply=true` 只允许在 `CPAMP_STACK_ROOT` 下写入固定文件名的 `compose.yml`、`stack.manifest.json` 和 `.env.example`，不拉镜像、不启动容器、不覆盖任意路径。
- 部署镜像拉取：`deploy apply=true action=pull_images` 只允许拉取标准 CPA/CPAMP/Agent manifest 中的 `seakee/cli-proxy-api` 和 `seakee/cpa-manager-plus` 镜像，不创建网络、卷或容器。
- 部署服务启动：`deploy apply=true action=start_services` 必须先存在部署文件和 `.env`，且 `.env` 中 `CPA_MANAGER_ADMIN_KEY`、`CPA_MANAGEMENT_KEY`、`CPAMP_AGENT_TOKEN` 不得为空或占位；Agent 只允许创建标准 `cpamp-cpa_default` 网络、标准数据卷和标准 CPA/CPAMP/Agent 容器，不提供任意镜像、任意容器名或任意挂载入口。
- 恢复预检：`restore` 默认只读取备份 manifest、校验 archive 文件、检查 CPA/CPAMP 目标容器，并返回恢复步骤，不执行 Docker 写操作。
- 恢复执行：`restore apply=true` 必须先通过恢复预检；Agent 会先创建新的 `rollback-cpa-*` 备份，回滚备份失败则不继续；随后只允许停止/恢复/启动已识别的 CPA 与可选 CPAMP 目标容器，不恢复 Agent，不修改 NewAPI 数据，不接受任意 archive 路径或任意容器名。
- 独立回滚：`rollback` 要求显式传入备份 ID，Agent 会先创建 `pre-rollback-cpa-*` 安全备份，再只对 CPA 与可选 CPAMP 目标容器执行受控归档应用、启动和运行状态健康检查。
- 网络标准化前置条件：必须提供已存在的备份 ID，且备份 manifest 中包含 CPA 数据归档。
- 网络标准化允许动作：创建 `cpamp-cpa_default` bridge 网络；把已识别角色的 CPA、CPAMP、Agent、NewAPI 容器连接到该网络。
- 升级预检：`upgrade` 默认只检查标准网络、标准 CPA/CPAMP 目标容器和镜像白名单，不执行 Docker 写操作。
- 升级准备：`upgrade apply=true` 必须先通过预检；Agent 会先创建 `upgrade-cpa-*` 回滚备份，再只拉取标准 CPA/CPAMP 升级镜像。
- 升级重建限制：当前版本不在同步 HTTP 请求中重建容器；异步 runner 只允许在存在 `upgrade-cpa-*` 回滚备份的前提下重建标准 CPAMP 托管的 `cli-proxy-api`。旧 CPA 容器会被改名保留，新 CPA 健康检查失败时会尝试恢复旧容器。`cpa-manager-plus` 和 `cpamp-agent` 自身重建仍延后，避免在没有完整自升级恢复机制前重建 Manager/Agent。
- 禁止动作：删除容器/网络/卷，给任意未识别容器执行操作，修改 NewAPI 数据；除恢复/回滚/CPA-only 升级状态机外，不提供任意停止、改名或重建容器能力。
- 限制说明：Docker 既有容器标签不能安全原地修改；既有网络缺少 CPAMP 标签时只提示风险，不在此步骤强制 relabel。

## 备份包结构

```text
manifest.json
cpa-<container>.tar       # Docker archive: CPA /app/data
cpamp-<container>.tar     # Docker archive: CPAMP /data，可选
```

恢复预检必须校验 CPA 数据归档存在；如果包含 CPAMP 归档，必须进一步扫描 tar 内容并确认 `usage.sqlite` 和 `data.key` 同时存在，缺失任一项都不能自动恢复 CPAMP 加密配置。

## 验收标准

- 空服务器可部署 CPAMP + Agent，并能一键部署 CPA 栈。
- 导入现有手工部署容器时，未确认前不修改 Docker 资源。
- NewAPI gateway 容器能解析并访问 `http://cli-proxy-api:8317/v1/models`。
- 备份恢复后，CPA 配置、auth 文件、CPAMP 历史用量和加密管理密钥均可用。
- 升级失败时自动回滚；回滚失败时保留现场并提示人工处理。
