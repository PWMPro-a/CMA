package database

import "time"

const (
	DriverSQLite = "sqlite"
	DriverMySQL  = "mysql"
)

type ConnectionConfig struct {
	Driver string `json:"driver"`
	Path   string `json:"path,omitempty"`
	DSN    string `json:"dsn,omitempty"`
}

type PublicConnectionConfig struct {
	Driver    string `json:"driver"`
	Path      string `json:"path,omitempty"`
	DSNMasked string `json:"dsnMasked,omitempty"`
}

type ConnectionStats struct {
	OpenConnections    int `json:"openConnections"`
	InUseConnections   int `json:"inUseConnections"`
	IdleConnections    int `json:"idleConnections"`
	MaxOpenConnections int `json:"maxOpenConnections"`
}

type DatabaseStatus struct {
	Driver                string          `json:"driver"`
	Healthy               bool            `json:"healthy"`
	DatabaseName          string          `json:"databaseName,omitempty"`
	Host                  string          `json:"host,omitempty"`
	Version               string          `json:"version,omitempty"`
	LatencyMS             int64           `json:"latencyMs"`
	Tables                int64           `json:"tables"`
	EstimatedRows         int64           `json:"estimatedRows,omitempty"`
	SizeBytes             int64           `json:"sizeBytes,omitempty"`
	Connections           ConnectionStats `json:"connections"`
	DatabaseBytes         int64           `json:"databaseBytes,omitempty"`
	WALBytes              int64           `json:"walBytes,omitempty"`
	SHMBytes              int64           `json:"shmBytes,omitempty"`
	TotalBytes            int64           `json:"totalBytes,omitempty"`
	JournalSizeLimitBytes int64           `json:"journalSizeLimitBytes,omitempty"`
	Checkpoint            any             `json:"checkpoint,omitempty"`
	Error                 string          `json:"error,omitempty"`
}

type ConfigurationState struct {
	Source          string `json:"source"`
	ConfigPath      string `json:"configPath,omitempty"`
	EnvironmentLock bool   `json:"environmentLock"`
	RestartRequired bool   `json:"restartRequired"`
}

type ManagementStatus struct {
	Current          DatabaseStatus         `json:"current"`
	Connection       PublicConnectionConfig `json:"connection"`
	Configuration    ConfigurationState     `json:"configuration"`
	SupportedDrivers []string               `json:"supportedDrivers"`
	LatestMigration  *MigrationJob          `json:"latestMigration,omitempty"`
}

type ProbeResult struct {
	Connection   PublicConnectionConfig `json:"connection"`
	Healthy      bool                   `json:"healthy"`
	Reachable    bool                   `json:"reachable"`
	Exists       bool                   `json:"exists"`
	SchemaReady  bool                   `json:"schemaReady"`
	DatabaseName string                 `json:"databaseName,omitempty"`
	Host         string                 `json:"host,omitempty"`
	Version      string                 `json:"version,omitempty"`
	LatencyMS    int64                  `json:"latencyMs"`
	Tables       int64                  `json:"tables"`
	Error        string                 `json:"error,omitempty"`
}

type MigrationPlan struct {
	Source               PublicConnectionConfig `json:"source"`
	Target               PublicConnectionConfig `json:"target"`
	SourceTables         int                    `json:"sourceTables"`
	TargetTables         int                    `json:"targetTables"`
	EstimatedSourceRows  int64                  `json:"estimatedSourceRows,omitempty"`
	TargetEmpty          bool                   `json:"targetEmpty"`
	TargetSchemaReady    bool                   `json:"targetSchemaReady"`
	RequiresEmptyTarget  bool                   `json:"requiresEmptyTarget"`
	RequiresRestart      bool                   `json:"requiresRestart"`
	OnlineWritesPossible bool                   `json:"onlineWritesPossible"`
	Warnings             []string               `json:"warnings,omitempty"`
}

type MigrationTable struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	TotalRows  int64  `json:"totalRows"`
	CopiedRows int64  `json:"copiedRows"`
	StartedAt  int64  `json:"startedAtMs,omitempty"`
	FinishedAt int64  `json:"finishedAtMs,omitempty"`
	Error      string `json:"error,omitempty"`
}

type MigrationJob struct {
	ID                 string                 `json:"id"`
	Status             string                 `json:"status"`
	Source             PublicConnectionConfig `json:"source"`
	Target             PublicConnectionConfig `json:"target"`
	CreatedAtMS        int64                  `json:"createdAtMs"`
	StartedAtMS        int64                  `json:"startedAtMs,omitempty"`
	FinishedAtMS       int64                  `json:"finishedAtMs,omitempty"`
	CurrentTable       string                 `json:"currentTable,omitempty"`
	TotalTables        int                    `json:"totalTables"`
	CompletedTables    int                    `json:"completedTables"`
	TotalRows          int64                  `json:"totalRows"`
	CopiedRows         int64                  `json:"copiedRows"`
	Verified           bool                   `json:"verified"`
	RestartRequired    bool                   `json:"restartRequired"`
	ConsistentSnapshot bool                   `json:"consistentSnapshot"`
	Error              string                 `json:"error,omitempty"`
	Tables             []MigrationTable       `json:"tables,omitempty"`
}

type SwitchResult struct {
	MigrationID         string                 `json:"migrationId"`
	Connection          PublicConnectionConfig `json:"connection"`
	AppliedToConfig     bool                   `json:"appliedToConfig"`
	RestartRequired     bool                   `json:"restartRequired"`
	ConfigurationSource string                 `json:"configurationSource"`
	ConfigPath          string                 `json:"configPath,omitempty"`
	PendingFile         string                 `json:"pendingFile,omitempty"`
	Environment         map[string]string      `json:"environment,omitempty"`
	Message             string                 `json:"message"`
}

func nowMS() int64 { return time.Now().UnixMilli() }
