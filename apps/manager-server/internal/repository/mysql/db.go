package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

type Options struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	SkipMigrate     bool
}

func Open(dsn string) (*sql.DB, error) {
	return OpenWithOptions(Options{DSN: dsn})
}

func OpenWithOptions(options Options) (*sql.DB, error) {
	config, err := mysqldriver.ParseDSN(strings.TrimSpace(options.DSN))
	if err != nil {
		return nil, fmt.Errorf("parse mysql dsn: %w", err)
	}
	if config.DBName == "" {
		return nil, fmt.Errorf("mysql dsn database name is required")
	}
	config.ParseTime = true
	config.Collation = "utf8mb4_unicode_ci"
	if config.Params == nil {
		config.Params = map[string]string{}
	}
	// The repository intentionally keeps one SQL surface for SQLite and MySQL.
	// PIPES_AS_CONCAT preserves the SQLite string-concatenation expressions used
	// by identity/search projections, while the remaining modes keep writes
	// strict and deterministic.
	config.Params["sql_mode"] = "'STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION,PIPES_AS_CONCAT'"
	// Filter option aggregation can legitimately contain more than MySQL's
	// 1 KiB default GROUP_CONCAT result.  The compatibility translation emits a
	// JSON array through GROUP_CONCAT, so keep enough headroom for a large pool.
	config.Params["group_concat_max_len"] = "1048576"
	if config.Timeout <= 0 {
		config.Timeout = 10 * time.Second
	}
	if config.ReadTimeout <= 0 {
		config.ReadTimeout = 60 * time.Second
	}
	if config.WriteTimeout <= 0 {
		config.WriteTimeout = 60 * time.Second
	}
	connector, err := mysqldriver.NewConnector(config)
	if err != nil {
		return nil, fmt.Errorf("create mysql connector: %w", err)
	}
	db := sql.OpenDB(&compatConnector{inner: connector})
	maxOpen := options.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 32
	}
	maxIdle := options.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = 8
	}
	lifetime := options.ConnMaxLifetime
	if lifetime <= 0 {
		lifetime = 30 * time.Minute
	}
	idleTime := options.ConnMaxIdleTime
	if idleTime <= 0 {
		idleTime = 5 * time.Minute
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(lifetime)
	db.SetConnMaxIdleTime(idleTime)

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := db.PingContext(pingCtx); err != nil {
		pingCancel()
		_ = db.Close()
		return nil, fmt.Errorf("connect mysql: %w", err)
	}
	pingCancel()

	// Empty MySQL installations build the full compatibility schema and its
	// indexes on first startup.  Sharing the short connectivity deadline with
	// that work made a healthy, newly initialized database fail close to the
	// final indexes on slower production disks.  Keep the connection probe
	// bounded separately and allow schema reconciliation a realistic window.
	if options.SkipMigrate {
		return db, nil
	}
	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer migrateCancel()
	if err := Migrate(migrateCtx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate mysql: %w", err)
	}
	return db, nil
}
