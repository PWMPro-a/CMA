# CPAM 上游升级合并手册

本文仅适用于 **CPA-Manager-Pro（CPAM）**。`CLIProxyAPI-Pro`（cpa-cli）是独立仓库、独立版本线和独立合并任务，不在本流程中连带升级。

## 1. v1.12.0-beta2 合并记录

| 项目 | 提交 |
| --- | --- |
| CPAM 本地基线 | `dc2f3cb63b96b2f3b7242136c4e59bd0ec25c1da` |
| 上游标签 | `v1.12.0-beta2` |
| 上游提交 | `fe63d9d465ed83800546241f92dfa8ce3b615181` |
| 双父合并提交 | `54adccd7` |
| 合并分支 | `codex/merge-upstream-v1.12.0-beta2` |

本次先创建标准双父 merge commit，再回放合并前未提交的 CPAM 本地改进。这样后续可以明确区分“上游版本合并”和“本地功能演进”，也能继续使用 Git ancestry 判断某个上游标签是否已经纳入。

## 2. 必须持续保留的 CPAM 能力

后续升级时，不应只按文件选择 `ours` 或 `theirs`。以下能力跨越多个文件，必须按业务语义复核：

| 本地能力 | 主要后端位置 | 主要前端位置 | 合并关注点 |
| --- | --- | --- | --- |
| Supply 自动补货、修复认领、财务统计 | `internal/service/supply`、Supply repository/controller | `features/supply`、`services/api/supply.ts` | 保留订单幂等、在途数量扣减、启动预热、安全限单、401 修复自动导入、账号稳定文件名 |
| Container Ops / cpamp-agent | `internal/service/containerops`、`internal/containeropsagent` | `features/containerOps`、`services/api/containerOps.ts` | 保留 Agent URL/Token、生命周期审计、部署/导入计划和路由注册 |
| Agent Identity 恢复 | auth-file controller/service/types | Auth Files hooks、卡片和恢复页 | 保留注册、重建、批量重试和状态轮询；状态提交要经过当前文件 revision/generation 防陈旧写回 |
| Source IP / 一键出口 IP | Container Ops egress handler/service/agent | Provider 编辑器、Accounts 配置抽屉、Egress Wizard | 上游主入口已迁移到 `/accounts`，不能只更新旧 `/auth-files` 页面；写 Auth File 时必须携带 credential identity snapshot |
| Supply 账号取消冻结 | `normalizeSupplyAccountObject` | 无 | 继续显式写入 `selection_error_freeze_seconds: 0`，避免 CPA 隐式 30 秒冻结 |
| Account Usage 无成本读取 | monitoring service/controller | Accounts/Auth Files 使用统计 | 保留 `include_cost=false`；同时兼容上游必填 `row_key`，不要回退到按数组或显示名猜测身份 |

## 3. v1.12.0-beta2 的关键语义决策

### 3.1 Accounts 工作区替代旧页面

上游将 Auth Files、Quota、健康检查和 OAuth 规则整合进 `/accounts`：

- `/auth-files`、`/quota`、`/codex-inspection` 保留为兼容重定向。
- 本地 `/supply`、`/container-ops` 和 Agent Identity 恢复入口继续保留。
- Source IP 编辑必须同时接入 `AccountConfigurationTab`。只修改旧 `AuthFilesPage` 会导致默认后台看不到该功能。

### 3.2 Auth File 修改采用身份快照校验

beta2 引入 credential-scoped mutation、文件 revision 和内容 SHA-256 校验。后续本地功能写入 Auth File 时遵循：

1. 先重新获取当前 Auth Files 列表。
2. 使用 `getAuthFilePatchTarget` 生成 `runtimeId/authIndex/provider/accountId/accountSnapshot` 快照。
3. 单凭文件名不能修改共享文件中的某个账号。
4. 通过 `patchFieldsWithPluginSourceFallback` 修改单凭据。
5. 需要重写共享源文件时，使用 `patchFieldsForAuthIndexes(name, targets, sourceIdentities, fields)`。
6. 保留上游 `X-CPAMP-Auth-File-*` identity/content-hash headers，不回退到下载后无条件覆盖上传。

本次 Source IP Wizard 已按上述规则适配，避免对共享 JSON 中其他账号产生旁路修改。

### 3.3 Auth Files 异步状态防陈旧提交

`useAuthFilesData` 同时保留：

- 上游 connection fingerprint、operation generation、request ID、files revision。
- 本地 Agent Identity registration polling。
- 本地静默刷新 `loadFiles({ silent: true })`。

所有改变文件列表的异步回调应走 `commitFiles`，不能直接 `setFiles`，否则 revision 不递增，旧请求可能覆盖新连接或新凭据。

### 3.4 Usage / Quota 数据契约

- `MonitoringAccountHistoryTarget.row_key` 按 beta2 设为必填。
- 本地账号用量请求继续发送 `include_cost: false`，避免卡片轮询触发模型价格计算。
- Account History 返回值保留 beta2 的 `row_key`、latest request、recent requests 和精确时间字段。
- Quota Snapshot、Usage Pricing Snapshot 和本地 Header Snapshot 同时存在时，按 credential identity 合并，不能按文件名覆盖。

### 3.5 SQLite 与 worker 启动顺序

- 使用 beta2 的 `_txlock=immediate` 和 mutation coordinator。
- 保留本地 `busy_timeout(30000)`、Supply 表迁移和启动快照预热。
- Usage rollup 在 cache accounting/backfill 后启动，避免启动阶段与 Supply/Recovery 写入争抢 SQLite。
- 不重复初始化 Codex inspection worker。

## 4. 翻译文件合并规则

四份 locale JSON 经常因为键顺序产生整文件冲突。合并时应做递归三方合并：

1. 以新上游文件为输出骨架，保留其键顺序。
2. 计算本地版本相对旧基线新增或修改的键。
3. 将这些键递归应用到新上游对象。
4. 同一路径双方都修改且值不同才算真实冲突。
5. 最后运行 Prettier 和 JSON 解析检查。

不要使用旧基线作为输出骨架，否则新增的 `accounts` 等大对象会被移动到文件末尾，产生数万行无意义 diff。

## 5. 推荐升级流程

```bash
# 1. 记录状态并做可恢复备份
git status --short
git bundle create /tmp/cpam-before-upgrade.bundle --all

# 2. 将未提交工作完整保存（包含 untracked 文件）
git stash push -u -m "checkpoint: local work before upstream merge"

# 3. 创建升级分支并合并标签
git switch -c codex/merge-upstream-VERSION
git merge --no-commit --no-ff VERSION

# 4. 按本手册做语义合并并验证
npm run manager-server:test
npm run type-check
npm test
npm run lint
npm run build

# 5. 创建双父合并提交后，再回放本地工作
git commit -m "merge: integrate upstream VERSION into CPAM"
git stash apply STASH_SHA

# 6. 处理回放冲突，再次执行完整验证并提交本地改进
```

`stash apply` 后重点检查新上游是否更换了主页面、DTO 或 mutation API。代码能自动合并不等于功能仍能从默认入口使用。

## 6. 验证门禁

每次 CPAM 上游升级至少通过：

```bash
npm run manager-server:test
npm run type-check
npm test
npm run lint
npm run build
git diff --check
```

另外执行以下结构检查：

```bash
# 不应残留冲突标记
grep -RIn --exclude-dir=.git -E '^(<<<<<<<|=======|>>>>>>>)' apps

# merge commit 应有两个父提交
git show -s --format='%P' MERGE_COMMIT

# 上游标签应成为当前分支祖先
git merge-base --is-ancestor VERSION HEAD
```

需要专项覆盖的场景：

- 共享 Auth File 中只修改目标 `authIndex`。
- Source IP 在 `/accounts` 配置抽屉可见、可保存、可清空。
- Agent Identity 重试期间切换连接，不回写旧连接状态。
- Supply 启动时先完成账号/容量预热，不因零请求立即重复下单。
- 在途订单、已下单未提货和正在提货数量会从补货缺口中扣除。
- 货源紧张/稀缺时保留基础量、约一半、约五分之一的分段抢号；一轮失败后按配置检查间隔重开，不引入固定 60/90 秒或分钟级本地退避。货源充足时仍采用消费速率驱动的小批量渐进采购。
- SQLite busy/locked 时 Recovery 导入可退避重试，不丢失已认领凭据。

## 7. 提交与发布边界

- CPAM 与 cpa-cli 分别提交、分别推送、分别发布。
- 合并提交只表达上游 ancestry；本地功能适配使用后续普通提交。
- 发布前按生产运行手册确认服务器目录、Compose 文件和 env 文件，不根据历史服务器猜测路径。
- 本地合并完成不自动等同于生产发布；发布必须在对应指令中单独执行。
