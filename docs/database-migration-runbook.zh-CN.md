# SQLite / MySQL 可视化迁移与切换手册

## 功能范围

系统信息页新增“数据库迁移与切换”卡片，支持：

- 查看当前 SQLite 或 MySQL 的健康、容量、连接池和版本；
- 填写目标 SQLite 路径或 MySQL DSN；
- 只读测试目标连接；
- 检查目标库是否为空并生成迁移计划；
- 创建一致性快照，按表复制、显示进度并逐表核对行数；
- 取消正在运行的迁移；
- 迁移核验通过后，保存下次启动配置或生成环境变量切换清单。

当前版本支持 SQLite 与 MySQL 双向迁移。切换只在 Manager 下次启动时生效，不替换运行中各 service/worker 使用的 Store。

## 迁移规则

1. **源库只读**：迁移过程只对目标库建表、清理初始化种子行和写入数据。
2. **目标库必须为空**：检测到业务数据会终止任务，避免覆盖已有数据。
3. **一致性快照**：复制使用源库只读事务，同一任务看到一个时间点的数据。
4. **逐表核验**：每张表完成后比较源行数、已复制行数和目标行数；全部一致才标记 `verified=true`。
5. **一个活动任务**：同一 Manager 同时只运行一个数据库迁移任务。
6. **外键处理**：复制期间仅在目标迁移连接上临时关闭外键检查，完成或失败后恢复。
7. **敏感信息**：MySQL 密码不写入任务 JSON，也不通过状态接口返回。DSN 文件和迁移任务日志权限为 `0600`。

## 生产切换流程

### 1. 先准备目标库

MySQL 数据库和账号示例：

```sql
create database cpamp character set utf8mb4 collate utf8mb4_unicode_ci;
create user 'cpamp'@'%' identified by 'PASSWORD';
grant all privileges on cpamp.* to 'cpamp'@'%';
flush privileges;
```

目标数据库可以没有表；迁移任务会创建当前版本结构。若已经有业务行，计划阶段会显示“目标库已有业务数据”。

### 2. 在面板测试和生成计划

进入：

```text
系统信息 -> 数据库迁移与切换
```

依次执行：

1. 选择 MySQL 或 SQLite；
2. 填写 DSN/路径；
3. 点击“测试连接”；
4. 点击“生成迁移计划”；
5. 确认目标库为空；
6. 点击“开始迁移”。

迁移进度包含当前表、完成表数、复制行数和错误。刷新页面后，Manager 仍会返回本进程内最新任务；任务公开日志位于：

```text
<dataDir>/database-migrations/<migration-id>.json
```

### 3. 处理迁移期间新增写入

快照开始后，源库仍可继续提供线上服务，但快照时间点之后的新请求和任务不会出现在该目标库。

要求零数据遗漏时：

1. 先用测试目标库演练并测量耗时；
2. 安排最终切换窗口；
3. 暂停 Collector、采购、巡检等会写 Manager 数据库的入口；
4. 使用一个新的空目标库执行最终迁移；
5. 等待任务 `completed + verified`；
6. 保存切换配置并只重建 Manager；
7. 通过 `/health`、`/status` 和管理面板完成验收后再恢复写入。

### 4. 保存切换配置

迁移完成后点击“保存切换配置”。

#### 配置文件托管

若数据库没有被 `USAGE_DB_*` 环境变量锁定，Manager 会原子更新 `CPA_MANAGER_CONFIG`：

- SQLite：写入 `dbDriver=sqlite`、`dbPath`；
- MySQL：写入 `dbDriver=mysql`、`dbDsnFile=<dataDir>/database.dsn`；
- DSN 单独以 `0600` 保存，不写入普通 JSON。

重启 Manager 后生效。

#### 环境变量托管

若 Compose/Kubernetes 使用 `USAGE_DB_*`，面板不会覆盖运行环境，而会生成：

```text
<dataDir>/database-switch.pending.json
<dataDir>/database-switch-<migration-id>.dsn   # MySQL 时
```

按面板返回的变量更新部署：

```yaml
environment:
  USAGE_DB_DRIVER: mysql
  USAGE_DB_DSN_FILE: /data/database-switch-MIGRATION_ID.dsn
```

然后只重建 Manager。不要重启 MySQL、Agent 或 Gateway。

## 回滚

切换前保留源库和 `data.key`。若新库验收失败：

1. 停止新 Manager 的数据库写入；
2. 将 `USAGE_DB_DRIVER`、`USAGE_DB_PATH`/`USAGE_DB_DSN_FILE` 恢复到源库；
3. 只重建 Manager；
4. 检查 `/status` 中的 driver、数据库名、事件数和配置读取是否恢复。

在新库已经产生写入后直接回滚会丢失这部分新数据；此时应先安排反向迁移到新的空目标库。

## 验收清单

- `/status.database.healthy=true`；
- driver、databaseName、host 与目标一致；
- 连接池没有耗尽；
- 迁移任务 `verified=true`；
- 关键表行数与源库一致；
- 管理配置、供应商配置、最近请求和历史采购可读取；
- `data.key` 与源环境一致，受保护字段可解密；
- 日志没有 deadlock、lock wait timeout、database is locked 或持续 5xx。
