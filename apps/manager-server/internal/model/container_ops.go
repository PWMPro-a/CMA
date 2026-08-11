package model

type ContainerOpsInfo struct {
	Enabled            bool                         `json:"enabled"`
	Mode               string                       `json:"mode"`
	Agent              ContainerOpsAgentInfo        `json:"agent"`
	StandardResources  ContainerOpsStandardResource `json:"standardResources"`
	NewAPI             ContainerOpsNewAPIInfo       `json:"newApi"`
	ManagedStack       string                       `json:"managedStack"`
	SupportedScopes    []string                     `json:"supportedScopes"`
	UnsupportedScopes  []string                     `json:"unsupportedScopes"`
	DestructiveActions bool                         `json:"destructiveActions"`
	Lifecycle          ContainerOpsLifecycleState   `json:"lifecycle"`
}

type ContainerOpsLifecycleState struct {
	OperationID string `json:"operationId,omitempty"`
	Operation   string `json:"operation,omitempty"`
	Phase       string `json:"phase,omitempty"`
	Status      string `json:"status"`
	Active      bool   `json:"active"`
	StartedAt   int64  `json:"startedAt,omitempty"`
	FinishedAt  int64  `json:"finishedAt,omitempty"`
	Message     string `json:"message,omitempty"`
}

type ContainerOpsAuditEntry struct {
	ID           int64  `json:"id"`
	OperationID  string `json:"operationId"`
	Operation    string `json:"operation"`
	Phase        string `json:"phase,omitempty"`
	Status       string `json:"status"`
	BackupID     string `json:"backupId,omitempty"`
	AgentBaseURL string `json:"agentBaseUrl,omitempty"`
	Message      string `json:"message,omitempty"`
	Error        string `json:"error,omitempty"`
	RequestJSON  string `json:"-"`
	Request      any    `json:"request,omitempty"`
	ResultJSON   string `json:"-"`
	Result       any    `json:"result,omitempty"`
	StartedAtMS  int64  `json:"startedAtMs"`
	FinishedAtMS int64  `json:"finishedAtMs,omitempty"`
	DurationMS   int64  `json:"durationMs,omitempty"`
	CreatedAtMS  int64  `json:"createdAtMs"`
	UpdatedAtMS  int64  `json:"updatedAtMs"`
}

type ContainerOpsUpgradeTask struct {
	ID               int64  `json:"id"`
	TaskID           string `json:"taskId"`
	OperationID      string `json:"operationId,omitempty"`
	Status           string `json:"status"`
	Phase            string `json:"phase,omitempty"`
	CPAImage         string `json:"cpaImage,omitempty"`
	CPAMPImage       string `json:"cpampImage,omitempty"`
	RollbackBackupID string `json:"rollbackBackupId,omitempty"`
	AgentBaseURL     string `json:"agentBaseUrl,omitempty"`
	Message          string `json:"message,omitempty"`
	Error            string `json:"error,omitempty"`
	NextAction       string `json:"nextAction,omitempty"`
	RequestJSON      string `json:"-"`
	Request          any    `json:"request,omitempty"`
	ResultJSON       string `json:"-"`
	Result           any    `json:"result,omitempty"`
	StartedAtMS      int64  `json:"startedAtMs"`
	FinishedAtMS     int64  `json:"finishedAtMs,omitempty"`
	CreatedAtMS      int64  `json:"createdAtMs"`
	UpdatedAtMS      int64  `json:"updatedAtMs"`
}

type ContainerOpsAgentInfo struct {
	Configured bool   `json:"configured"`
	Reachable  bool   `json:"reachable"`
	BaseURL    string `json:"baseUrl,omitempty"`
	Service    string `json:"service,omitempty"`
	Version    string `json:"version,omitempty"`
	Mode       string `json:"mode,omitempty"`
	DockerHost string `json:"dockerHost,omitempty"`
	ReadOnly   bool   `json:"readOnly,omitempty"`
	Error      string `json:"error,omitempty"`
}

type ContainerOpsStandardResource struct {
	ComposeProject string `json:"composeProject"`
	Network        string `json:"network"`
	CPAService     string `json:"cpaService"`
	CPAMPService   string `json:"cpampService"`
	AgentService   string `json:"agentService"`
	StackRoot      string `json:"stackRoot"`
	BackupRoot     string `json:"backupRoot"`
}

type ContainerOpsNewAPIInfo struct {
	RecommendedBaseURL string `json:"recommendedBaseUrl"`
}

type ContainerOpsEgressIPInventory struct {
	Agent            ContainerOpsAgentInfo       `json:"agent,omitempty"`
	DefaultInterface string                      `json:"defaultInterface,omitempty"`
	Route            string                      `json:"route,omitempty"`
	NativeOutboundIP string                      `json:"nativeOutboundIp,omitempty"`
	Addresses        []ContainerOpsEgressAddress `json:"addresses"`
	Checks           []ContainerOpsEgressCheck   `json:"checks,omitempty"`
}

type ContainerOpsEgressAddress struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
	CIDR      string `json:"cidr"`
	Scope     string `json:"scope,omitempty"`
}

type ContainerOpsSourceIPRequest struct {
	SourceIP  string `json:"sourceIp"`
	Interface string `json:"interface,omitempty"`
	VerifyURL string `json:"verifyUrl,omitempty"`
}

type ContainerOpsSourceIPResult struct {
	Agent            ContainerOpsAgentInfo       `json:"agent,omitempty"`
	SourceIP         string                      `json:"sourceIp"`
	Interface        string                      `json:"interface,omitempty"`
	Status           string                      `json:"status"`
	Mounted          bool                        `json:"mounted"`
	AlreadyPresent   bool                        `json:"alreadyPresent"`
	Removed          bool                        `json:"removed,omitempty"`
	OutboundIP       string                      `json:"outboundIp,omitempty"`
	NativeOutboundIP string                      `json:"nativeOutboundIp,omitempty"`
	Checks           []ContainerOpsEgressCheck   `json:"checks"`
	Actions          []ContainerOpsEgressAction  `json:"actions,omitempty"`
	Lifecycle        *ContainerOpsLifecycleState `json:"lifecycle,omitempty"`
}

type ContainerOpsEgressCheck struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Resource string `json:"resource,omitempty"`
	Blocking bool   `json:"blocking"`
}

type ContainerOpsEgressAction struct {
	Order   int    `json:"order"`
	Code    string `json:"code"`
	Target  string `json:"target,omitempty"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Output  string `json:"output,omitempty"`
}

type ContainerOpsDiscovery struct {
	Agent             ContainerOpsAgentInfo      `json:"agent"`
	Docker            ContainerOpsDockerOverview `json:"docker"`
	NewAPI            ContainerOpsNewAPIInfo     `json:"newApi"`
	RecommendedAction string                     `json:"recommendedAction"`
}

type ContainerOpsImportPlan struct {
	Agent       ContainerOpsAgentInfo         `json:"agent"`
	Summary     ContainerOpsImportSummary     `json:"summary"`
	Manifest    ContainerOpsStackManifest     `json:"manifest"`
	Compose     ContainerOpsComposeDraft      `json:"compose"`
	Candidates  []ContainerOpsImportCandidate `json:"candidates"`
	Risks       []ContainerOpsImportRisk      `json:"risks"`
	NextActions []string                      `json:"nextActions"`
	NewAPI      ContainerOpsNewAPIInfo        `json:"newApi"`
	ReadOnly    bool                          `json:"readOnly"`
}

type ContainerOpsImportSummary struct {
	Ready             bool `json:"ready"`
	CPAFound          bool `json:"cpaFound"`
	CPAMPFound        bool `json:"cpampFound"`
	AgentFound        bool `json:"agentFound"`
	NewAPIFound       bool `json:"newApiFound"`
	RiskCount         int  `json:"riskCount"`
	BlockingRiskCount int  `json:"blockingRiskCount"`
}

type ContainerOpsImportCandidate struct {
	Role             string                    `json:"role"`
	ContainerID      string                    `json:"containerId,omitempty"`
	Name             string                    `json:"name"`
	Image            string                    `json:"image"`
	State            string                    `json:"state"`
	Managed          bool                      `json:"managed"`
	TargetService    string                    `json:"targetService"`
	IncludeInCompose bool                      `json:"includeInCompose"`
	Networks         []string                  `json:"networks,omitempty"`
	Ports            []ContainerOpsDockerPort  `json:"ports,omitempty"`
	Mounts           []ContainerOpsDockerMount `json:"mounts,omitempty"`
	Reasons          []string                  `json:"reasons,omitempty"`
}

type ContainerOpsStackManifest struct {
	Stack          string                        `json:"stack"`
	ComposeProject string                        `json:"composeProject"`
	Network        string                        `json:"network"`
	StackRoot      string                        `json:"stackRoot"`
	BackupRoot     string                        `json:"backupRoot"`
	NewAPIBaseURL  string                        `json:"newApiBaseUrl"`
	Services       []ContainerOpsManifestService `json:"services"`
	Volumes        []ContainerOpsManifestVolume  `json:"volumes,omitempty"`
}

type ContainerOpsManifestService struct {
	Role             string   `json:"role"`
	Service          string   `json:"service"`
	SourceContainer  string   `json:"sourceContainer,omitempty"`
	Image            string   `json:"image,omitempty"`
	State            string   `json:"state,omitempty"`
	Managed          bool     `json:"managed"`
	IncludeInCompose bool     `json:"includeInCompose"`
	InternalBaseURL  string   `json:"internalBaseUrl,omitempty"`
	Networks         []string `json:"networks,omitempty"`
	Mounts           []string `json:"mounts,omitempty"`
	Ports            []string `json:"ports,omitempty"`
}

type ContainerOpsManifestVolume struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination"`
	External    bool   `json:"external"`
}

type ContainerOpsComposeDraft struct {
	FileName    string   `json:"fileName"`
	ProjectName string   `json:"projectName"`
	NetworkName string   `json:"networkName"`
	Services    []string `json:"services"`
	Content     string   `json:"content"`
}

type ContainerOpsImportRisk struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Resource string `json:"resource,omitempty"`
	Blocking bool   `json:"blocking"`
}

type ContainerOpsDeployRequest struct {
	Apply  bool   `json:"apply"`
	Action string `json:"action,omitempty"`
}

type ContainerOpsDeployPlan struct {
	Agent       ContainerOpsAgentInfo       `json:"agent,omitempty"`
	Status      string                      `json:"status"`
	Manifest    ContainerOpsStackManifest   `json:"manifest"`
	Compose     ContainerOpsComposeDraft    `json:"compose"`
	Checks      []ContainerOpsDeployCheck   `json:"checks"`
	Steps       []ContainerOpsDeployStep    `json:"steps"`
	Files       []ContainerOpsDeployFile    `json:"files,omitempty"`
	ImagePulls  []ContainerOpsImagePull     `json:"imagePulls,omitempty"`
	Actions     []ContainerOpsDeployAction  `json:"actions,omitempty"`
	Applied     bool                        `json:"applied"`
	Destructive bool                        `json:"destructive"`
	ReadOnly    bool                        `json:"readOnly"`
	Overview    *ContainerOpsDockerOverview `json:"overview,omitempty"`
	Lifecycle   *ContainerOpsLifecycleState `json:"lifecycle,omitempty"`
}

type ContainerOpsDeployCheck struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Resource string `json:"resource,omitempty"`
	Blocking bool   `json:"blocking"`
}

type ContainerOpsDeployStep struct {
	Order       int    `json:"order"`
	Code        string `json:"code"`
	Title       string `json:"title"`
	Target      string `json:"target,omitempty"`
	Destructive bool   `json:"destructive"`
}

type ContainerOpsDeployFile struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}

type ContainerOpsImagePull struct {
	Image   string `json:"image"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type ContainerOpsDeployAction struct {
	Order   int    `json:"order"`
	Code    string `json:"code"`
	Target  string `json:"target,omitempty"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type ContainerOpsDeployRenderRequest struct {
	Manifest ContainerOpsStackManifest `json:"manifest"`
	Compose  ContainerOpsComposeDraft  `json:"compose"`
}

type ContainerOpsBackupResult struct {
	Agent      ContainerOpsAgentInfo       `json:"agent,omitempty"`
	BackupID   string                      `json:"backupId"`
	Status     string                      `json:"status"`
	BackupRoot string                      `json:"backupRoot"`
	CreatedAt  int64                       `json:"createdAt"`
	Archives   []ContainerOpsBackupArchive `json:"archives"`
	Warnings   []ContainerOpsBackupWarning `json:"warnings,omitempty"`
	ReadOnly   bool                        `json:"readOnly"`
	Lifecycle  *ContainerOpsLifecycleState `json:"lifecycle,omitempty"`
}

type ContainerOpsBackupArchive struct {
	Role      string `json:"role"`
	Service   string `json:"service"`
	Container string `json:"container"`
	Path      string `json:"path"`
	FileName  string `json:"fileName"`
	Size      int64  `json:"size"`
}

type ContainerOpsBackupWarning struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Resource string `json:"resource,omitempty"`
}

type ContainerOpsRestoreRequest struct {
	BackupID string `json:"backupId"`
	Apply    bool   `json:"apply"`
}

type ContainerOpsRollbackRequest struct {
	BackupID string `json:"backupId"`
}

type ContainerOpsRestorePlan struct {
	Agent          ContainerOpsAgentInfo       `json:"agent,omitempty"`
	BackupID       string                      `json:"backupId"`
	Status         string                      `json:"status"`
	BackupRoot     string                      `json:"backupRoot"`
	CreatedAt      int64                       `json:"createdAt"`
	Archives       []ContainerOpsBackupArchive `json:"archives"`
	Checks         []ContainerOpsRestoreCheck  `json:"checks"`
	Steps          []ContainerOpsRestoreStep   `json:"steps"`
	Actions        []ContainerOpsRestoreAction `json:"actions,omitempty"`
	RollbackBackup *ContainerOpsBackupResult   `json:"rollbackBackup,omitempty"`
	Applied        bool                        `json:"applied"`
	Destructive    bool                        `json:"destructive"`
	ReadOnly       bool                        `json:"readOnly"`
	Overview       *ContainerOpsDockerOverview `json:"overview,omitempty"`
	Lifecycle      *ContainerOpsLifecycleState `json:"lifecycle,omitempty"`
}

type ContainerOpsRestoreCheck struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Resource string `json:"resource,omitempty"`
	Blocking bool   `json:"blocking"`
}

type ContainerOpsRestoreStep struct {
	Order       int    `json:"order"`
	Code        string `json:"code"`
	Title       string `json:"title"`
	Target      string `json:"target,omitempty"`
	Destructive bool   `json:"destructive"`
}

type ContainerOpsRestoreAction struct {
	Order   int    `json:"order"`
	Code    string `json:"code"`
	Target  string `json:"target,omitempty"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type ContainerOpsNetworkStandardizeRequest struct {
	BackupID string `json:"backupId"`
	Apply    bool   `json:"apply"`
}

type ContainerOpsNetworkStandardizeResult struct {
	Agent       ContainerOpsAgentInfo       `json:"agent,omitempty"`
	BackupID    string                      `json:"backupId"`
	Status      string                      `json:"status"`
	Network     string                      `json:"network"`
	Checks      []ContainerOpsNetworkCheck  `json:"checks"`
	Actions     []ContainerOpsNetworkAction `json:"actions"`
	Applied     bool                        `json:"applied"`
	Destructive bool                        `json:"destructive"`
	Overview    *ContainerOpsDockerOverview `json:"overview,omitempty"`
	Lifecycle   *ContainerOpsLifecycleState `json:"lifecycle,omitempty"`
}

type ContainerOpsUpgradeRequest struct {
	CPAImage   string `json:"cpaImage,omitempty"`
	CPAMPImage string `json:"cpampImage,omitempty"`
	Apply      bool   `json:"apply"`
}

type ContainerOpsUpgradeTaskStartRequest struct {
	TaskID string `json:"taskId"`
}

type ContainerOpsUpgradeJobStartRequest struct {
	TaskID           string `json:"taskId"`
	CPAImage         string `json:"cpaImage,omitempty"`
	CPAMPImage       string `json:"cpampImage,omitempty"`
	RollbackBackupID string `json:"rollbackBackupId,omitempty"`
}

type ContainerOpsUpgradeJob struct {
	JobID            string                      `json:"jobId"`
	TaskID           string                      `json:"taskId,omitempty"`
	Status           string                      `json:"status"`
	Phase            string                      `json:"phase,omitempty"`
	CPAImage         string                      `json:"cpaImage,omitempty"`
	CPAMPImage       string                      `json:"cpampImage,omitempty"`
	RollbackBackupID string                      `json:"rollbackBackupId,omitempty"`
	Message          string                      `json:"message,omitempty"`
	Error            string                      `json:"error,omitempty"`
	NextAction       string                      `json:"nextAction,omitempty"`
	Checks           []ContainerOpsUpgradeCheck  `json:"checks,omitempty"`
	Actions          []ContainerOpsUpgradeAction `json:"actions,omitempty"`
	Plan             *ContainerOpsUpgradePlan    `json:"plan,omitempty"`
	StartedAtMS      int64                       `json:"startedAtMs"`
	FinishedAtMS     int64                       `json:"finishedAtMs,omitempty"`
	CreatedAtMS      int64                       `json:"createdAtMs"`
	UpdatedAtMS      int64                       `json:"updatedAtMs"`
}

type ContainerOpsUpgradePlan struct {
	Agent          ContainerOpsAgentInfo       `json:"agent,omitempty"`
	Status         string                      `json:"status"`
	CPAImage       string                      `json:"cpaImage"`
	CPAMPImage     string                      `json:"cpampImage"`
	Checks         []ContainerOpsUpgradeCheck  `json:"checks"`
	Steps          []ContainerOpsUpgradeStep   `json:"steps"`
	Actions        []ContainerOpsUpgradeAction `json:"actions,omitempty"`
	ImagePulls     []ContainerOpsImagePull     `json:"imagePulls,omitempty"`
	RollbackBackup *ContainerOpsBackupResult   `json:"rollbackBackup,omitempty"`
	Task           *ContainerOpsUpgradeTask    `json:"task,omitempty"`
	Applied        bool                        `json:"applied"`
	Destructive    bool                        `json:"destructive"`
	ReadOnly       bool                        `json:"readOnly"`
	Overview       *ContainerOpsDockerOverview `json:"overview,omitempty"`
	Lifecycle      *ContainerOpsLifecycleState `json:"lifecycle,omitempty"`
}

type ContainerOpsUpgradeCheck struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Resource string `json:"resource,omitempty"`
	Blocking bool   `json:"blocking"`
}

type ContainerOpsUpgradeStep struct {
	Order       int    `json:"order"`
	Code        string `json:"code"`
	Title       string `json:"title"`
	Target      string `json:"target,omitempty"`
	Destructive bool   `json:"destructive"`
}

type ContainerOpsUpgradeAction struct {
	Order   int    `json:"order"`
	Code    string `json:"code"`
	Target  string `json:"target,omitempty"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type ContainerOpsNetworkCheck struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Resource string `json:"resource,omitempty"`
	Blocking bool   `json:"blocking"`
}

type ContainerOpsNetworkAction struct {
	Order   int    `json:"order"`
	Code    string `json:"code"`
	Target  string `json:"target,omitempty"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type ContainerOpsDockerOverview struct {
	Summary    ContainerOpsDockerSummary     `json:"summary"`
	Containers []ContainerOpsDockerContainer `json:"containers"`
	Networks   []ContainerOpsDockerNetwork   `json:"networks"`
	Images     []ContainerOpsDockerImage     `json:"images"`
}

type ContainerOpsDockerSummary struct {
	ContainerCount int `json:"containerCount"`
	RunningCount   int `json:"runningCount"`
	NetworkCount   int `json:"networkCount"`
	ImageCount     int `json:"imageCount"`
	CPACount       int `json:"cpaCount"`
	CPAMPCount     int `json:"cpampCount"`
	NewAPICount    int `json:"newApiCount"`
	ManagedCount   int `json:"managedCount"`
}

type ContainerOpsDockerContainer struct {
	ID       string                         `json:"id"`
	Name     string                         `json:"name"`
	Image    string                         `json:"image"`
	ImageID  string                         `json:"imageId,omitempty"`
	State    string                         `json:"state"`
	Status   string                         `json:"status"`
	Role     string                         `json:"role,omitempty"`
	Managed  bool                           `json:"managed"`
	Labels   map[string]string              `json:"labels,omitempty"`
	Ports    []ContainerOpsDockerPort       `json:"ports,omitempty"`
	Mounts   []ContainerOpsDockerMount      `json:"mounts,omitempty"`
	Networks []ContainerOpsDockerAttachment `json:"networks,omitempty"`
}

type ContainerOpsDockerPort struct {
	PrivatePort int    `json:"privatePort"`
	PublicPort  int    `json:"publicPort,omitempty"`
	Type        string `json:"type,omitempty"`
	IP          string `json:"ip,omitempty"`
}

type ContainerOpsDockerMount struct {
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination"`
	Mode        string `json:"mode,omitempty"`
	RW          bool   `json:"rw"`
}

type ContainerOpsDockerAttachment struct {
	Name      string `json:"name"`
	NetworkID string `json:"networkId,omitempty"`
	IPAddress string `json:"ipAddress,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
}

type ContainerOpsDockerNetwork struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Scope      string            `json:"scope"`
	Internal   bool              `json:"internal"`
	Attachable bool              `json:"attachable"`
	Managed    bool              `json:"managed"`
	Labels     map[string]string `json:"labels,omitempty"`
	Containers int               `json:"containers"`
}

type ContainerOpsDockerImage struct {
	ID       string            `json:"id"`
	RepoTags []string          `json:"repoTags"`
	Size     int64             `json:"size"`
	Created  int64             `json:"created"`
	Labels   map[string]string `json:"labels,omitempty"`
}
