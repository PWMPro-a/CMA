package sqlite

import (
	"database/sql"
	"fmt"
)

func Migrate(db *sql.DB) error {
	statements := []string{
		`pragma journal_mode = WAL`,
		`pragma synchronous = FULL`,
		`pragma busy_timeout = 5000`,
		`pragma foreign_keys = ON`,
		`create table if not exists usage_events (
			id integer primary key autoincrement,
			request_id text,
			event_hash text not null unique,
			timestamp_ms integer not null,
			timestamp text not null,
			provider text,
			executor_type text,
			model text not null,
			endpoint text,
			method text,
			path text,
			auth_type text,
			auth_index text,
			source text,
			source_hash text,
			api_key_hash text,
			account_snapshot text,
			auth_label_snapshot text,
			auth_file_snapshot text,
			auth_provider_snapshot text,
			auth_project_id_snapshot text,
			auth_snapshot_at_ms integer,
			requested_model text,
			resolved_model text,
			reasoning_effort text,
			service_tier text,
			input_tokens integer not null default 0,
			output_tokens integer not null default 0,
			reasoning_tokens integer not null default 0,
			cached_tokens integer not null default 0,
			cache_tokens integer not null default 0,
			cache_read_tokens integer not null default 0,
			cache_creation_tokens integer not null default 0,
			total_tokens integer not null default 0,
			latency_ms integer,
			ttft_ms integer,
			failed integer not null default 0,
			fail_status_code integer,
			fail_summary text,
			fail_body text,
			raw_json text,
			created_at_ms integer not null
		)`,
		`create index if not exists idx_usage_events_timestamp on usage_events(timestamp_ms)`,
		`create index if not exists idx_usage_events_request_id on usage_events(request_id)`,
		`create index if not exists idx_usage_events_model on usage_events(model)`,
		`create index if not exists idx_usage_events_auth_index on usage_events(auth_index)`,
		`create index if not exists idx_usage_events_endpoint on usage_events(endpoint)`,
		`create table if not exists dead_letter_events (
			id integer primary key autoincrement,
			payload text not null,
			error text not null,
			created_at_ms integer not null
		)`,
		`create table if not exists settings (
			key text primary key,
			value text not null,
			updated_at_ms integer not null
		)`,
		`create table if not exists model_prices (
			model text primary key,
			prompt_per_1m real not null,
			completion_per_1m real not null,
			cache_per_1m real not null,
			cache_read_per_1m real not null default 0,
			cache_creation_per_1m real not null default 0,
			source text,
			source_model_id text,
			raw_json text,
			updated_at_ms integer not null,
			synced_at_ms integer
		)`,
		`create table if not exists api_key_aliases (
			api_key_hash text primary key,
			alias text not null,
			updated_at_ms integer not null
		)`,
		`create table if not exists codex_inspection_runs (
			id integer primary key autoincrement,
			trigger_type text not null,
			trigger_key text,
			status text not null,
			started_at_ms integer not null,
			finished_at_ms integer,
			total_files integer not null default 0,
			probe_set_count integer not null default 0,
			sampled_count integer not null default 0,
			disabled_count integer not null default 0,
			enabled_count integer not null default 0,
			delete_count integer not null default 0,
			disable_count integer not null default 0,
			enable_count integer not null default 0,
			reauth_count integer not null default 0,
			keep_count integer not null default 0,
			error text,
			settings_json text not null,
			created_at_ms integer not null,
			updated_at_ms integer not null
		)`,
		`create index if not exists idx_codex_inspection_runs_started_at on codex_inspection_runs(started_at_ms)`,
		`create index if not exists idx_codex_inspection_runs_status on codex_inspection_runs(status)`,
		`create index if not exists idx_codex_inspection_runs_trigger on codex_inspection_runs(trigger_type, trigger_key)`,
		`create table if not exists codex_inspection_results (
			id integer primary key autoincrement,
			run_id integer not null,
			account_key text not null,
			file_name text not null,
			display_account text not null,
			auth_index text,
			account_id text,
			provider text,
			disabled integer not null default 0,
			status text,
			state text,
			action text not null,
			action_reason text,
			action_status text,
			executed_action text,
			action_error text,
			status_code integer,
			used_percent real,
			is_quota integer not null default 0,
			error text,
			created_at_ms integer not null,
			foreign key(run_id) references codex_inspection_runs(id) on delete cascade,
			unique(run_id, account_key)
		)`,
		`create index if not exists idx_codex_inspection_results_run on codex_inspection_results(run_id)`,
		`create table if not exists codex_inspection_logs (
			id integer primary key autoincrement,
			run_id integer not null,
			level text not null,
			message text not null,
			detail_json text,
			created_at_ms integer not null,
			foreign key(run_id) references codex_inspection_runs(id) on delete cascade
		)`,
		`create index if not exists idx_codex_inspection_logs_run on codex_inspection_logs(run_id, created_at_ms)`,
		`create table if not exists container_ops_audits (
			id integer primary key autoincrement,
			operation_id text not null unique,
			operation text not null,
			phase text,
			status text not null,
			backup_id text,
			agent_base_url text,
			message text,
			error text,
			request_json text,
			result_json text,
			started_at_ms integer not null,
			finished_at_ms integer,
			duration_ms integer,
			created_at_ms integer not null,
			updated_at_ms integer not null
		)`,
		`create index if not exists idx_container_ops_audits_started_at on container_ops_audits(started_at_ms)`,
		`create index if not exists idx_container_ops_audits_operation on container_ops_audits(operation, status)`,
		`create table if not exists container_ops_upgrade_tasks (
			id integer primary key autoincrement,
			task_id text not null unique,
			operation_id text,
			status text not null,
			phase text,
			cpa_image text,
			cpamp_image text,
			rollback_backup_id text,
			agent_base_url text,
			message text,
			error text,
			next_action text,
			request_json text,
			result_json text,
			started_at_ms integer not null,
			finished_at_ms integer,
			created_at_ms integer not null,
			updated_at_ms integer not null
		)`,
		`create index if not exists idx_container_ops_upgrade_tasks_created_at on container_ops_upgrade_tasks(created_at_ms)`,
		`create index if not exists idx_container_ops_upgrade_tasks_status on container_ops_upgrade_tasks(status, phase)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	if err := ensureUsageEventSnapshotColumns(db); err != nil {
		return err
	}
	if err := ensureCodexInspectionRunColumns(db); err != nil {
		return err
	}
	if err := ensureCodexInspectionResultColumns(db); err != nil {
		return err
	}
	return ensureModelPriceColumns(db)
}

func ensureCodexInspectionRunColumns(db *sql.DB) error {
	rows, err := db.Query(`pragma table_info(codex_inspection_runs)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	existing := map[string]struct{}{}
	for rows.Next() {
		var cid int
		var name string
		var columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		existing[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	columns := []struct {
		name       string
		definition string
	}{
		{name: "reauth_count", definition: "integer not null default 0"},
	}
	for _, column := range columns {
		if _, ok := existing[column.name]; ok {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf(
			`alter table codex_inspection_runs add column %s %s`,
			column.name,
			column.definition,
		)); err != nil {
			return err
		}
	}
	return nil
}

func ensureCodexInspectionResultColumns(db *sql.DB) error {
	rows, err := db.Query(`pragma table_info(codex_inspection_results)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	existing := map[string]struct{}{}
	for rows.Next() {
		var cid int
		var name string
		var columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		existing[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	columns := []struct {
		name       string
		definition string
	}{
		{name: "action_status", definition: "text"},
		{name: "executed_action", definition: "text"},
		{name: "action_error", definition: "text"},
	}
	for _, column := range columns {
		if _, ok := existing[column.name]; ok {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf(
			`alter table codex_inspection_results add column %s %s`,
			column.name,
			column.definition,
		)); err != nil {
			return err
		}
	}
	return nil
}

func ensureUsageEventSnapshotColumns(db *sql.DB) error {
	rows, err := db.Query(`pragma table_info(usage_events)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	existing := map[string]struct{}{}
	for rows.Next() {
		var cid int
		var name string
		var columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		existing[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	columns := []struct {
		name       string
		definition string
	}{
		{name: "account_snapshot", definition: "text"},
		{name: "auth_label_snapshot", definition: "text"},
		{name: "auth_file_snapshot", definition: "text"},
		{name: "auth_provider_snapshot", definition: "text"},
		{name: "auth_project_id_snapshot", definition: "text"},
		{name: "auth_snapshot_at_ms", definition: "integer"},
		{name: "executor_type", definition: "text"},
		{name: "requested_model", definition: "text"},
		{name: "resolved_model", definition: "text"},
		{name: "reasoning_effort", definition: "text"},
		{name: "service_tier", definition: "text"},
		{name: "cache_read_tokens", definition: "integer not null default 0"},
		{name: "cache_creation_tokens", definition: "integer not null default 0"},
		{name: "ttft_ms", definition: "integer"},
		{name: "fail_status_code", definition: "integer"},
		{name: "fail_summary", definition: "text"},
		{name: "fail_body", definition: "text"},
	}
	for _, column := range columns {
		if _, ok := existing[column.name]; ok {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf(
			`alter table usage_events add column %s %s`,
			column.name,
			column.definition,
		)); err != nil {
			return err
		}
	}
	return nil
}

func ensureModelPriceColumns(db *sql.DB) error {
	rows, err := db.Query(`pragma table_info(model_prices)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	existing := map[string]struct{}{}
	for rows.Next() {
		var cid int
		var name string
		var columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		existing[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	columns := []struct {
		name       string
		definition string
	}{
		{name: "cache_read_per_1m", definition: "real not null default 0"},
		{name: "cache_creation_per_1m", definition: "real not null default 0"},
	}
	for _, column := range columns {
		if _, ok := existing[column.name]; ok {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf(
			`alter table model_prices add column %s %s`,
			column.name,
			column.definition,
		)); err != nil {
			return err
		}
	}
	return nil
}
