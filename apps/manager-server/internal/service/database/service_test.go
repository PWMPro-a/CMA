package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/config"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func TestSQLiteMigrationCopiesAndVerifiesCanonicalTables(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sqlite")
	targetPath := filepath.Join(dir, "target.sqlite")
	st, err := store.Open(sourcePath)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer st.Close()
	if err := st.SaveSetup(context.Background(), model.Setup{
		CPAUpstreamURL: "http://cpa.test:8317", ManagementKey: "test-key",
	}); err != nil {
		t.Fatalf("save setup: %v", err)
	}
	cfg := config.Config{DataDir: dir, DBDriver: DriverSQLite, DBPath: sourcePath, ConfigPath: filepath.Join(dir, "config.json"), DBConfigSource: "file"}
	service := New(cfg, st)

	plan, err := service.Plan(context.Background(), ConnectionConfig{Driver: DriverSQLite, Path: targetPath})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.TargetEmpty || plan.SourceTables == 0 {
		t.Fatalf("plan = %#v", plan)
	}
	job, err := service.StartMigration(ConnectionConfig{Driver: DriverSQLite, Path: targetPath}, true)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	job = waitForMigration(t, service, job.ID)
	if job.Status != "completed" || !job.Verified || job.CompletedTables != job.TotalTables {
		t.Fatalf("job = %#v", job)
	}

	target, err := store.Open(targetPath)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	defer target.Close()
	setup, ok, err := target.LoadSetup(context.Background())
	if err != nil || !ok || setup.ManagementKey != "test-key" {
		t.Fatalf("target setup = %#v ok=%v err=%v", setup, ok, err)
	}
}

func TestPrepareSwitchWritesDSNFileWithoutEmbeddingPasswordInConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte("{\n  \"httpAddr\": \"127.0.0.1:18317\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "source.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	service := New(config.Config{DataDir: dir, DBPath: filepath.Join(dir, "source.sqlite"), ConfigPath: configPath, DBConfigSource: "file"}, st)
	target := ConnectionConfig{Driver: DriverMySQL, DSN: "cpamp:secret@tcp(mysql:3306)/cpamp"}
	job := MigrationJob{ID: "verified-migration", Status: "completed", Verified: true, Target: publicConnection(target)}
	service.jobs[job.ID] = &migrationRuntime{job: job, target: target, fingerprint: connectionFingerprint(target)}

	result, err := service.PrepareSwitch(job.ID, target)
	if err != nil {
		t.Fatal(err)
	}
	if !result.AppliedToConfig || !result.RestartRequired {
		t.Fatalf("result = %#v", result)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") || !strings.Contains(string(data), "dbDsnFile") {
		t.Fatalf("config leaked or omitted dsn file: %s", data)
	}
	dsnData, err := os.ReadFile(filepath.Join(dir, "database.dsn"))
	if err != nil || strings.TrimSpace(string(dsnData)) != target.DSN {
		t.Fatalf("dsn file = %q err=%v", dsnData, err)
	}
	info, err := os.Stat(filepath.Join(dir, "database.dsn"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("dsn mode = %v err=%v", info, err)
	}
}

func TestMaskMySQLDSN(t *testing.T) {
	masked := maskMySQLDSN("cpamp:secret@tcp(mysql:3306)/cpamp?charset=utf8mb4")
	if strings.Contains(masked, "secret") || !strings.Contains(masked, "******") {
		t.Fatalf("masked dsn = %q", masked)
	}
}

func TestMySQLConnectionFingerprintIdentifiesDatabaseNotCredentials(t *testing.T) {
	left := connectionFingerprint(ConnectionConfig{Driver: DriverMySQL, DSN: "first:one@tcp(mysql:3306)/cpamp"})
	right := connectionFingerprint(ConnectionConfig{Driver: DriverMySQL, DSN: "second:two@tcp(mysql:3306)/cpamp?timeout=5s"})
	if left != right {
		t.Fatalf("same mysql database fingerprints differ: %s != %s", left, right)
	}
}

func TestTargetIsEmptyRejectsNonSeedSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target.sqlite")
	target, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if _, err := target.SQLDB().Exec(`insert into settings(key, value, updated_at_ms) values('custom', '{}', 1)`); err != nil {
		t.Fatal(err)
	}
	tables, err := listTables(context.Background(), target.SQLDB(), DriverSQLite)
	if err != nil {
		t.Fatal(err)
	}
	empty, occupied, err := targetIsEmpty(context.Background(), target.SQLDB(), DriverSQLite, tables)
	if err != nil {
		t.Fatal(err)
	}
	if empty || len(occupied) != 1 || occupied[0] != "settings" {
		t.Fatalf("empty=%v occupied=%v", empty, occupied)
	}
}

func waitForMigration(t *testing.T, service *Service, id string) MigrationJob {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := service.GetMigration(id)
		if !ok {
			t.Fatal("migration disappeared")
		}
		if job.Status == "completed" || job.Status == "failed" || job.Status == "cancelled" {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("migration timed out")
	return MigrationJob{}
}
