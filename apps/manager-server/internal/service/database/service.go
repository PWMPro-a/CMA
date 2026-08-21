package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/config"
	mysqlrepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/mysql"
	sqliterepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/sqlite"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

const migrationBatchSize = 100

type migrationRuntime struct {
	job         MigrationJob
	target      ConnectionConfig
	fingerprint string
	cancel      context.CancelFunc
}

type Service struct {
	cfg   config.Config
	store *store.Store

	mu       sync.RWMutex
	jobs     map[string]*migrationRuntime
	latestID string
	activeID string
}

func New(cfg config.Config, st *store.Store) *Service {
	return &Service{cfg: cfg, store: st, jobs: make(map[string]*migrationRuntime)}
}

func (s *Service) currentConnection() ConnectionConfig {
	return ConnectionConfig{Driver: s.store.Driver(), Path: s.cfg.DBPath, DSN: s.cfg.DBDSN}
}

func (s *Service) ManagementStatus(ctx context.Context) ManagementStatus {
	status := s.Status(ctx)
	configSource := strings.TrimSpace(s.cfg.DBConfigSource)
	if configSource == "" {
		configSource = "default"
	}
	var latest *MigrationJob
	s.mu.RLock()
	if runtime := s.jobs[s.latestID]; runtime != nil {
		copy := cloneJob(runtime.job)
		latest = &copy
	}
	s.mu.RUnlock()
	return ManagementStatus{
		Current:          status,
		Connection:       publicConnection(s.currentConnection()),
		Configuration:    ConfigurationState{Source: configSource, ConfigPath: s.cfg.ConfigPath, EnvironmentLock: s.cfg.DBConfigEnvLocked, RestartRequired: true},
		SupportedDrivers: []string{DriverSQLite, DriverMySQL},
		LatestMigration:  latest,
	}
}

func (s *Service) Status(ctx context.Context) DatabaseStatus {
	db := s.store.SQLDB()
	status := DatabaseStatus{Driver: s.store.Driver()}
	if db == nil {
		status.Error = "database handle is unavailable"
		return status
	}
	started := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := db.PingContext(probeCtx)
	cancel()
	status.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Healthy = true
	stats := db.Stats()
	status.Connections = ConnectionStats{
		OpenConnections: stats.OpenConnections, InUseConnections: stats.InUse,
		IdleConnections: stats.Idle, MaxOpenConnections: stats.MaxOpenConnections,
	}
	if status.Driver == DriverMySQL {
		s.populateMySQLStatus(ctx, db, &status)
	} else {
		s.populateSQLiteStatus(ctx, db, &status)
	}
	return status
}

func (s *Service) populateSQLiteStatus(ctx context.Context, db *sql.DB, status *DatabaseStatus) {
	status.DatabaseName = filepath.Base(s.cfg.DBPath)
	status.Host = "local"
	_ = db.QueryRowContext(ctx, `select sqlite_version()`).Scan(&status.Version)
	_ = db.QueryRowContext(ctx, `select count(*) from sqlite_schema where type='table' and name not like 'sqlite_%'`).Scan(&status.Tables)
	status.DatabaseBytes = fileSize(s.cfg.DBPath)
	status.WALBytes = fileSize(s.cfg.DBPath + "-wal")
	status.SHMBytes = fileSize(s.cfg.DBPath + "-shm")
	status.TotalBytes = status.DatabaseBytes + status.WALBytes + status.SHMBytes
	status.SizeBytes = status.TotalBytes
	var journalLimit int64
	if err := db.QueryRowContext(ctx, `pragma journal_size_limit`).Scan(&journalLimit); err == nil && journalLimit > 0 {
		status.JournalSizeLimitBytes = journalLimit
	}
}

func (s *Service) populateMySQLStatus(ctx context.Context, db *sql.DB, status *DatabaseStatus) {
	_ = db.QueryRowContext(ctx, `select database(), version()`).Scan(&status.DatabaseName, &status.Version)
	var tables, rows, size sql.NullInt64
	err := db.QueryRowContext(ctx, `select count(*), coalesce(sum(table_rows), 0), coalesce(sum(data_length + index_length), 0)
		from information_schema.tables where table_schema = database() and table_type = 'BASE TABLE'`).Scan(&tables, &rows, &size)
	if err == nil {
		status.Tables, status.EstimatedRows, status.SizeBytes = tables.Int64, rows.Int64, size.Int64
		status.TotalBytes = status.SizeBytes
	}
	if parsed, err := mysqldriver.ParseDSN(strings.TrimSpace(s.cfg.DBDSN)); err == nil {
		status.Host = parsed.Addr
		if status.DatabaseName == "" {
			status.DatabaseName = parsed.DBName
		}
	}
}

func (s *Service) Probe(ctx context.Context, input ConnectionConfig) (ProbeResult, error) {
	target, err := normalizeConnection(input)
	if err != nil {
		return ProbeResult{}, err
	}
	result := ProbeResult{Connection: publicConnection(target)}
	if target.Driver == DriverSQLite {
		if _, err := os.Stat(target.Path); err != nil {
			if os.IsNotExist(err) {
				parent := filepath.Dir(target.Path)
				if info, statErr := os.Stat(parent); statErr != nil || !info.IsDir() {
					return result, fmt.Errorf("sqlite parent directory is unavailable: %s", parent)
				}
				result.Healthy = true
				result.Exists = false
				result.Reachable = true
				return result, nil
			}
			return result, err
		}
		result.Exists = true
	}
	db, err := openConnection(target, false)
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	defer db.Close()
	started := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = db.PingContext(probeCtx)
	cancel()
	result.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	result.Healthy, result.Reachable, result.Exists = true, true, true
	if target.Driver == DriverMySQL {
		_ = db.QueryRowContext(ctx, `select database(), version()`).Scan(&result.DatabaseName, &result.Version)
		_ = db.QueryRowContext(ctx, `select count(*) from information_schema.tables where table_schema=database() and table_type='BASE TABLE'`).Scan(&result.Tables)
		if parsed, parseErr := mysqldriver.ParseDSN(target.DSN); parseErr == nil {
			result.Host = parsed.Addr
		}
	} else {
		result.DatabaseName = filepath.Base(target.Path)
		result.Host = "local"
		_ = db.QueryRowContext(ctx, `select sqlite_version()`).Scan(&result.Version)
		_ = db.QueryRowContext(ctx, `select count(*) from sqlite_schema where type='table' and name not like 'sqlite_%'`).Scan(&result.Tables)
	}
	result.SchemaReady = result.Tables > 0
	return result, nil
}

func (s *Service) Plan(ctx context.Context, input ConnectionConfig) (MigrationPlan, error) {
	target, err := normalizeConnection(input)
	if err != nil {
		return MigrationPlan{}, err
	}
	if connectionFingerprint(target) == connectionFingerprint(s.currentConnection()) {
		return MigrationPlan{}, errors.New("target database matches the active database")
	}
	sourceTables, err := listTables(ctx, s.store.SQLDB(), s.store.Driver())
	if err != nil {
		return MigrationPlan{}, err
	}
	plan := MigrationPlan{
		Source: publicConnection(s.currentConnection()), Target: publicConnection(target),
		SourceTables: len(sourceTables), RequiresEmptyTarget: true, RequiresRestart: true,
		OnlineWritesPossible: true,
		Warnings: []string{
			"online_snapshot_requires_final_write_pause",
			"empty_target_required",
		},
	}
	plan.EstimatedSourceRows = estimateRows(ctx, s.store.SQLDB(), s.store.Driver())
	probe, err := s.Probe(ctx, target)
	if err != nil {
		return MigrationPlan{}, err
	}
	plan.TargetSchemaReady = probe.SchemaReady
	if !probe.Exists && target.Driver == DriverSQLite {
		plan.TargetEmpty = true
		return plan, nil
	}
	if !probe.Healthy {
		return MigrationPlan{}, errors.New(probe.Error)
	}
	db, err := openConnection(target, false)
	if err != nil {
		return MigrationPlan{}, err
	}
	defer db.Close()
	targetTables, err := listTables(ctx, db, target.Driver)
	if err != nil {
		return MigrationPlan{}, err
	}
	plan.TargetTables = len(targetTables)
	empty, _, err := targetIsEmpty(ctx, db, target.Driver, targetTables)
	if err != nil {
		return MigrationPlan{}, err
	}
	plan.TargetEmpty = empty
	return plan, nil
}

func (s *Service) StartMigration(input ConnectionConfig, requireEmpty bool) (MigrationJob, error) {
	if !requireEmpty {
		return MigrationJob{}, errors.New("requireEmptyTarget confirmation is required")
	}
	target, err := normalizeConnection(input)
	if err != nil {
		return MigrationJob{}, err
	}
	if connectionFingerprint(target) == connectionFingerprint(s.currentConnection()) {
		return MigrationJob{}, errors.New("target database matches the active database")
	}
	s.mu.Lock()
	if s.activeID != "" {
		if active := s.jobs[s.activeID]; active != nil && (active.job.Status == "queued" || active.job.Status == "running" || active.job.Status == "cancelling") {
			s.mu.Unlock()
			return MigrationJob{}, fmt.Errorf("migration %s is already active", s.activeID)
		}
	}
	id := uuid.NewString()
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &migrationRuntime{
		job: MigrationJob{
			ID: id, Status: "queued", Source: publicConnection(s.currentConnection()), Target: publicConnection(target),
			CreatedAtMS: nowMS(), RestartRequired: true, ConsistentSnapshot: true,
		},
		target: target, fingerprint: connectionFingerprint(target), cancel: cancel,
	}
	s.jobs[id], s.latestID, s.activeID = runtime, id, id
	job := cloneJob(runtime.job)
	s.mu.Unlock()
	s.persistJob(job)
	go s.runMigration(ctx, id)
	return job, nil
}

func (s *Service) GetMigration(id string) (MigrationJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	runtime := s.jobs[strings.TrimSpace(id)]
	if runtime == nil {
		return MigrationJob{}, false
	}
	return cloneJob(runtime.job), true
}

func (s *Service) CancelMigration(id string) (MigrationJob, error) {
	s.mu.Lock()
	runtime := s.jobs[strings.TrimSpace(id)]
	if runtime == nil {
		s.mu.Unlock()
		return MigrationJob{}, errors.New("migration not found")
	}
	if runtime.job.Status != "queued" && runtime.job.Status != "running" {
		job := cloneJob(runtime.job)
		s.mu.Unlock()
		return job, nil
	}
	runtime.job.Status = "cancelling"
	runtime.cancel()
	job := cloneJob(runtime.job)
	s.mu.Unlock()
	s.persistJob(job)
	return job, nil
}

func (s *Service) runMigration(ctx context.Context, id string) {
	s.updateJob(id, func(job *MigrationJob) {
		job.Status = "running"
		job.StartedAtMS = nowMS()
	})
	runtime := s.runtime(id)
	if runtime == nil {
		return
	}
	err := s.copyDatabase(ctx, id, runtime.target)
	if err != nil {
		status := "failed"
		if errors.Is(err, context.Canceled) {
			status = "cancelled"
		}
		s.updateJob(id, func(job *MigrationJob) {
			job.Status = status
			job.Error = err.Error()
			job.FinishedAtMS = nowMS()
		})
	} else {
		s.updateJob(id, func(job *MigrationJob) {
			job.Status = "completed"
			job.Verified = true
			job.CurrentTable = ""
			job.FinishedAtMS = nowMS()
		})
	}
	s.mu.Lock()
	if s.activeID == id {
		s.activeID = ""
	}
	s.mu.Unlock()
}

func (s *Service) copyDatabase(ctx context.Context, id string, targetConfig ConnectionConfig) error {
	targetDB, err := openConnection(targetConfig, true)
	if err != nil {
		return err
	}
	defer targetDB.Close()
	sourceDB := s.store.SQLDB()
	sourceDriver := s.store.Driver()
	targetDriver := targetConfig.Driver
	sourceTables, err := listTables(ctx, sourceDB, sourceDriver)
	if err != nil {
		return err
	}
	targetTables, err := listTables(ctx, targetDB, targetDriver)
	if err != nil {
		return err
	}
	missing := difference(sourceTables, targetTables)
	if len(missing) > 0 {
		return fmt.Errorf("target schema is missing tables: %s", strings.Join(missing, ", "))
	}
	empty, occupied, err := targetIsEmpty(ctx, targetDB, targetDriver, targetTables)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("target database is not empty; occupied tables: %s", strings.Join(occupied, ", "))
	}

	// Both supported backends provide a stable read view once the first query in
	// this transaction executes: SQLite through its read transaction and MySQL
	// through the default REPEATABLE READ isolation. Avoid requesting an
	// isolation level from the SQLite driver because it is not portable there.
	sourceTx, err := sourceDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("start source snapshot: %w", err)
	}
	defer sourceTx.Rollback()
	targetConn, err := targetDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer targetConn.Close()
	if err := setForeignKeyChecks(ctx, targetConn, targetDriver, false); err != nil {
		return err
	}
	defer setForeignKeyChecks(context.Background(), targetConn, targetDriver, true)

	tables := intersect(sourceTables, targetTables)
	sort.Strings(tables)
	progress := make([]MigrationTable, len(tables))
	for index, name := range tables {
		progress[index] = MigrationTable{Name: name, Status: "pending"}
	}
	s.updateJob(id, func(job *MigrationJob) {
		job.Tables = progress
		job.TotalTables = len(progress)
	})

	for index := len(tables) - 1; index >= 0; index-- {
		if _, err := targetConn.ExecContext(ctx, "delete from "+quoteIdentifier(targetDriver, tables[index])); err != nil {
			return fmt.Errorf("clear target table %s: %w", tables[index], err)
		}
	}

	for tableIndex, table := range tables {
		if err := ctx.Err(); err != nil {
			return err
		}
		var total int64
		if err := sourceTx.QueryRowContext(ctx, "select count(*) from "+quoteIdentifier(sourceDriver, table)).Scan(&total); err != nil {
			return fmt.Errorf("count source table %s: %w", table, err)
		}
		s.updateJob(id, func(job *MigrationJob) {
			job.CurrentTable = table
			job.TotalRows += total
			job.Tables[tableIndex].Status = "running"
			job.Tables[tableIndex].TotalRows = total
			job.Tables[tableIndex].StartedAt = nowMS()
		})
		copied, err := copyTable(ctx, sourceTx, sourceDriver, targetConn, targetDriver, table, func(delta int64) {
			s.updateJobTransient(id, func(job *MigrationJob) {
				job.CopiedRows += delta
				job.Tables[tableIndex].CopiedRows += delta
			})
		})
		if err != nil {
			s.updateJob(id, func(job *MigrationJob) {
				job.Tables[tableIndex].Status = "failed"
				job.Tables[tableIndex].Error = err.Error()
			})
			return err
		}
		var targetCount int64
		if err := targetConn.QueryRowContext(ctx, "select count(*) from "+quoteIdentifier(targetDriver, table)).Scan(&targetCount); err != nil {
			return fmt.Errorf("verify target table %s: %w", table, err)
		}
		if copied != total || targetCount != total {
			return fmt.Errorf("verify table %s: source=%d copied=%d target=%d", table, total, copied, targetCount)
		}
		s.updateJob(id, func(job *MigrationJob) {
			job.Tables[tableIndex].Status = "completed"
			job.Tables[tableIndex].FinishedAt = nowMS()
			job.CompletedTables++
		})
	}
	return sourceTx.Commit()
}

func (s *Service) PrepareSwitch(migrationID string, input ConnectionConfig) (SwitchResult, error) {
	target, err := normalizeConnection(input)
	if err != nil {
		return SwitchResult{}, err
	}
	s.mu.RLock()
	runtime := s.jobs[strings.TrimSpace(migrationID)]
	if runtime == nil || runtime.job.Status != "completed" || !runtime.job.Verified {
		s.mu.RUnlock()
		return SwitchResult{}, errors.New("a completed and verified migration is required")
	}
	fingerprint := runtime.fingerprint
	s.mu.RUnlock()
	if fingerprint != connectionFingerprint(target) {
		return SwitchResult{}, errors.New("switch target does not match the verified migration")
	}
	result := SwitchResult{
		MigrationID: migrationID, Connection: publicConnection(target), RestartRequired: true,
		ConfigurationSource: s.cfg.DBConfigSource, ConfigPath: s.cfg.ConfigPath,
	}
	if s.cfg.DBConfigEnvLocked || strings.TrimSpace(s.cfg.ConfigPath) == "" {
		pendingFile, environment, err := s.writePendingSwitch(migrationID, target)
		if err != nil {
			return SwitchResult{}, err
		}
		result.PendingFile = pendingFile
		result.Environment = environment
		result.Message = "database environment variables are authoritative; apply the generated values and recreate only the Manager process"
		return result, nil
	}
	if err := s.writeConfigSwitch(target); err != nil {
		return SwitchResult{}, err
	}
	result.AppliedToConfig = true
	result.Message = "next-start database configuration saved; restart the Manager process to activate it"
	return result, nil
}

func (s *Service) writePendingSwitch(migrationID string, target ConnectionConfig) (string, map[string]string, error) {
	if err := os.MkdirAll(s.cfg.DataDir, 0o700); err != nil {
		return "", nil, err
	}
	environment := map[string]string{"USAGE_DB_DRIVER": target.Driver}
	if target.Driver == DriverMySQL {
		dsnPath := filepath.Join(s.cfg.DataDir, "database-switch-"+migrationID+".dsn")
		if err := writeAtomic(dsnPath, []byte(target.DSN+"\n"), 0o600); err != nil {
			return "", nil, err
		}
		environment["USAGE_DB_DSN_FILE"] = dsnPath
	} else {
		environment["USAGE_DB_PATH"] = target.Path
	}
	pending := map[string]any{
		"migrationId": migrationID, "createdAtMs": nowMS(), "target": publicConnection(target),
		"environment": environment, "restartRequired": true,
	}
	data, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(s.cfg.DataDir, "database-switch.pending.json")
	if err := writeAtomic(path, append(data, '\n'), 0o600); err != nil {
		return "", nil, err
	}
	return path, environment, nil
}

func (s *Service) writeConfigSwitch(target ConnectionConfig) error {
	configPath := strings.TrimSpace(s.cfg.ConfigPath)
	payload := map[string]any{}
	if data, err := os.ReadFile(configPath); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &payload); err != nil {
			return fmt.Errorf("parse config %s: %w", configPath, err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	payload["dbDriver"] = target.Driver
	if target.Driver == DriverMySQL {
		dsnPath := filepath.Join(s.cfg.DataDir, "database.dsn")
		if err := os.MkdirAll(filepath.Dir(dsnPath), 0o700); err != nil {
			return err
		}
		if err := writeAtomic(dsnPath, []byte(target.DSN+"\n"), 0o600); err != nil {
			return err
		}
		payload["dbDsnFile"] = dsnPath
		delete(payload, "dbDsn")
		delete(payload, "dbPath")
	} else {
		payload["dbPath"] = target.Path
		delete(payload, "dbDsn")
		delete(payload, "dbDsnFile")
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(configPath, append(data, '\n'), 0o600)
}

func (s *Service) runtime(id string) *migrationRuntime {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.jobs[id]
}

func (s *Service) updateJob(id string, update func(*MigrationJob)) {
	s.mu.Lock()
	runtime := s.jobs[id]
	if runtime == nil {
		s.mu.Unlock()
		return
	}
	update(&runtime.job)
	job := cloneJob(runtime.job)
	s.mu.Unlock()
	s.persistJob(job)
}

func (s *Service) updateJobTransient(id string, update func(*MigrationJob)) {
	s.mu.Lock()
	if runtime := s.jobs[id]; runtime != nil {
		update(&runtime.job)
	}
	s.mu.Unlock()
}

func (s *Service) persistJob(job MigrationJob) {
	if strings.TrimSpace(s.cfg.DataDir) == "" {
		return
	}
	dir := filepath.Join(s.cfg.DataDir, "database-migrations")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return
	}
	_ = writeAtomic(filepath.Join(dir, job.ID+".json"), append(data, '\n'), 0o600)
}

func cloneJob(job MigrationJob) MigrationJob {
	copy := job
	copy.Tables = append([]MigrationTable(nil), job.Tables...)
	return copy
}

func normalizeConnection(input ConnectionConfig) (ConnectionConfig, error) {
	driver := strings.ToLower(strings.TrimSpace(input.Driver))
	switch driver {
	case DriverSQLite:
		path := strings.TrimSpace(input.Path)
		if path == "" {
			return ConnectionConfig{}, errors.New("sqlite path is required")
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return ConnectionConfig{}, err
		}
		if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
			absolute = resolved
		}
		return ConnectionConfig{Driver: driver, Path: absolute}, nil
	case DriverMySQL:
		dsn := strings.TrimSpace(input.DSN)
		if dsn == "" {
			return ConnectionConfig{}, errors.New("mysql dsn is required")
		}
		parsed, err := mysqldriver.ParseDSN(dsn)
		if err != nil {
			return ConnectionConfig{}, fmt.Errorf("parse mysql dsn: %w", err)
		}
		if strings.TrimSpace(parsed.DBName) == "" {
			return ConnectionConfig{}, errors.New("mysql dsn database name is required")
		}
		return ConnectionConfig{Driver: driver, DSN: dsn}, nil
	default:
		return ConnectionConfig{}, fmt.Errorf("unsupported database driver %q", input.Driver)
	}
}

func publicConnection(input ConnectionConfig) PublicConnectionConfig {
	public := PublicConnectionConfig{Driver: strings.ToLower(strings.TrimSpace(input.Driver)), Path: input.Path}
	if public.Driver == DriverMySQL {
		public.Path = ""
		public.DSNMasked = maskMySQLDSN(input.DSN)
	}
	return public
}

func maskMySQLDSN(dsn string) string {
	parsed, err := mysqldriver.ParseDSN(strings.TrimSpace(dsn))
	if err != nil {
		return "***"
	}
	if parsed.Passwd != "" {
		parsed.Passwd = "******"
	}
	return parsed.FormatDSN()
}

func connectionFingerprint(input ConnectionConfig) string {
	normalized, err := normalizeConnection(input)
	if err != nil {
		normalized = input
	}
	identity := normalized.Path
	if normalized.Driver == DriverMySQL {
		if parsed, parseErr := mysqldriver.ParseDSN(normalized.DSN); parseErr == nil {
			identity = strings.ToLower(strings.TrimSpace(parsed.Net)) + "\x00" +
				strings.ToLower(strings.TrimSpace(parsed.Addr)) + "\x00" +
				strings.ToLower(strings.TrimSpace(parsed.DBName))
		} else {
			identity = normalized.DSN
		}
	}
	sum := sha256.Sum256([]byte(normalized.Driver + "\x00" + identity))
	return hex.EncodeToString(sum[:])
}

func openConnection(input ConnectionConfig, migrate bool) (*sql.DB, error) {
	if input.Driver == DriverMySQL {
		return mysqlrepo.OpenWithOptions(mysqlrepo.Options{DSN: input.DSN, SkipMigrate: !migrate})
	}
	return sqliterepo.OpenWithOptions(sqliterepo.Options{Path: input.Path, SkipMigrate: !migrate})
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cpamp-db-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
