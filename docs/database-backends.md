# Database Backends

CPA Manager Plus Manager Server supports two storage backends through one repository surface:

| Backend            | Driver value | Connection setting                                  | Intended use                                               |
| ------------------ | ------------ | --------------------------------------------------- | ---------------------------------------------------------- |
| SQLite             | `sqlite`     | `USAGE_DB_PATH` / `dbPath`                          | Single-node installs and simple backups                    |
| MySQL 5.7.8+ / 8.x | `mysql`      | `USAGE_DB_DSN` or `USAGE_DB_DSN_FILE` / `dbDsnFile` | Larger datasets and independently managed database storage |

The active backend is selected only during Manager startup. Workers and services share one store handle, so the running process is never switched to another database in place.

## Configuration precedence

Database settings use this precedence:

1. `USAGE_DB_DRIVER`, `USAGE_DB_DSN`, `USAGE_DB_DSN_FILE`, `USAGE_DB_PATH`;
2. `CPA_MANAGER_CONFIG` JSON fields `dbDriver`, `dbDsnFile`, `dbDsn`, `dbPath`;
3. the default SQLite file at `<dataDir>/usage.sqlite`.

Example SQLite configuration:

```json
{
  "dataDir": "/data",
  "dbDriver": "sqlite",
  "dbPath": "/data/usage.sqlite"
}
```

Example MySQL configuration with a restricted secret file:

```json
{
  "dataDir": "/data",
  "dbDriver": "mysql",
  "dbDsnFile": "/data/database.dsn"
}
```

`/data/database.dsn`:

```text
cpamp:PASSWORD@tcp(cpamp-mysql:3306)/cpamp?parseTime=true
```

Use mode `0600` for DSN and generated database-switch files. The management API never returns the clear-text password; it exposes only a masked DSN.

## Runtime status

`GET /status` now returns a backend-neutral `database` object:

- driver, health, server/database version, latency;
- database name and host where applicable;
- connection-pool usage;
- table count, estimated rows, and storage size;
- SQLite file/WAL/SHM and checkpoint details when SQLite is active.

The System page renders the same status for both SQLite and MySQL.

## Schema compatibility

SQLite remains the canonical schema definition. MySQL startup derives and reconciles compatible tables, columns, indexes, generated uniqueness helpers, and best-effort foreign keys from that definition. Business repositories keep one SQL surface, with the MySQL compatibility connector translating the SQLite forms used by existing queries.

When adding storage code:

1. use parameterized statements;
2. verify the query through both SQLite and MySQL tests;
3. do not add a backend option to the panel until schema creation, reads, writes, and migration are implemented for it;
4. preserve the same `data.key` when moving encrypted settings or supply credentials to another backend.

## Management API

All endpoints require panel administrator authorization.

| Method | Endpoint                                         | Purpose                                                             |
| ------ | ------------------------------------------------ | ------------------------------------------------------------------- |
| `GET`  | `/v0/management/database`                        | Active status, configuration source, supported backends, latest job |
| `POST` | `/v0/management/database/test`                   | Read-only connection probe                                          |
| `POST` | `/v0/management/database/migrations/plan`        | Empty-target and schema plan                                        |
| `POST` | `/v0/management/database/migrations`             | Start a snapshot copy                                               |
| `GET`  | `/v0/management/database/migrations/{id}`        | Read table and row progress                                         |
| `POST` | `/v0/management/database/migrations/{id}/cancel` | Cancel the active copy                                              |
| `POST` | `/v0/management/database/switch`                 | Save or generate next-start configuration                           |

Migration job journals are stored under `<dataDir>/database-migrations/` with mode `0600`. They contain masked connection data only.
