package managerconfig

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/config"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	collectorservice "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/collector"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpa"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

type Source string

const (
	SourceNone Source = ""
	SourceEnv  Source = "env"
	SourceDB   Source = "db"

	SupplyStrategyStrongSupply = "strong_supply"
	SupplyStrategyBalanced     = "balanced"
	SupplyStrategyCostFirst    = "cost_first"
	SupplyStrategyCustom       = "custom"
	SupplyQuotaEstimationAuto  = "auto"
	SupplyQuotaEstimationFixed = "fixed"
)

const (
	SupplyPlatformLegacy   = "legacy"
	SupplyPlatformBugTeam  = "bugteam"
	SupplyPlatformNvtokens = "nvtokens"

	SupplyPurchaseAccountAll                 = "all"
	SupplyPurchaseAccountHasRefreshToken     = "has_refresh_token"
	SupplyPurchaseAccountWithoutRefreshToken = "without_refresh_token"

	SupplyPlatformSelectionBestAvailable = "best_available"
	SupplyPlatformSelectionPriorityFirst = "priority_first"
)

type supplyStrategyPreset struct {
	Strategy                    string
	CriticalAvailableAccounts   int
	HealthyAvailableAccounts    int
	DefaultEmergencyMinAccounts int
	VirtualDemandTTLMinutes     int
	AccountMaxRequestsBefore401 int
	AccountMaxUsefulSeconds401  int
	EmergencyBypassUsageRate    bool
	RecoveryTriggerOn401        bool
}

type Response struct {
	Config   store.ManagerConfig `json:"config"`
	Source   string              `json:"source"`
	CPAUsage *cpa.UsageConfig    `json:"cpaUsage,omitempty"`
}

type Service struct {
	cfg       config.Config
	store     *store.Store
	collector *collectorservice.Service
}

func New(cfg config.Config, store *store.Store, collector *collectorservice.Service) *Service {
	return &Service{
		cfg:       cfg,
		store:     store,
		collector: collector,
	}
}

func (s *Service) Get(ctx context.Context) (Response, error) {
	cfg, source, _, err := s.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return Response{}, err
	}
	var cpaUsage *cpa.UsageConfig
	if cfg.CPAConnection.CPABaseURL != "" && cfg.CPAConnection.ManagementKey != "" {
		if usageCfg, err := cpa.FetchUsageConfig(
			ctx,
			cfg.CPAConnection.CPABaseURL,
			cfg.CPAConnection.ManagementKey,
		); err == nil {
			cpaUsage = &usageCfg
		}
	}
	return Response{
		Config:   SanitizeManagerConfig(cfg),
		Source:   string(source),
		CPAUsage: cpaUsage,
	}, nil
}

func (s *Service) Update(ctx context.Context, submitted store.ManagerConfig) (Response, error) {
	current, source, _, err := s.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return Response{}, err
	}
	if err := model.ValidateCodexInspectionConfig(submitted.CodexInspection); err != nil {
		return Response{}, err
	}
	next := s.MergeSubmittedManagerConfig(current, submitted)
	if err := ValidateSupplyConfig(next.Supply); err != nil {
		return Response{}, err
	}
	if source == SourceEnv && ManagerConfigConnectionDiffers(current, next) {
		return Response{}, errors.New("connection setup is managed by environment variables")
	}
	if next.CPAConnection.CPABaseURL != "" || next.CPAConnection.ManagementKey != "" {
		if next.CPAConnection.CPABaseURL == "" || next.CPAConnection.ManagementKey == "" {
			return Response{}, errors.New("cpaBaseUrl and managementKey are required")
		}
		if err := cpa.ValidateManagementAPI(
			ctx,
			next.CPAConnection.CPABaseURL,
			next.CPAConnection.ManagementKey,
		); err != nil {
			return Response{}, err
		}
		if ManagerCollectorEnabled(next) {
			if err := cpa.ValidateCollectorConfig(
				ctx,
				next.CPAConnection.CPABaseURL,
				next.CPAConnection.ManagementKey,
				next.Collector.PollIntervalMS,
			); err != nil {
				return Response{}, err
			}
			if err := cpa.SetUsageStatisticsEnabled(
				ctx,
				next.CPAConnection.CPABaseURL,
				next.CPAConnection.ManagementKey,
				true,
			); err != nil {
				return Response{}, err
			}
		}
	} else if ManagerCollectorEnabled(next) {
		return Response{}, errors.New("cpaBaseUrl and managementKey are required when request monitoring is enabled")
	}
	if next.CPAConnection.CPABaseURL == "" || next.CPAConnection.ManagementKey == "" {
		if err := s.store.SaveManagerConfig(ctx, next); err != nil {
			return Response{}, err
		}
		_ = s.collector.Stop(context.Background())
		return Response{
			Config: SanitizeManagerConfig(next),
			Source: string(SourceDB),
		}, nil
	}
	if err := s.store.SaveManagerConfig(ctx, next); err != nil {
		return Response{}, err
	}
	setup := SetupFromManagerConfig(next)
	if err := s.store.SaveSetup(ctx, setup); err != nil {
		return Response{}, err
	}
	if ManagerCollectorEnabled(next) {
		_ = s.collector.Start(context.Background(), next)
	} else {
		_ = s.collector.Stop(context.Background())
	}
	return Response{
		Config: SanitizeManagerConfig(next),
		Source: string(SourceDB),
	}, nil
}

func (s *Service) UpdateSupply(ctx context.Context, submitted store.ManagerSupplyConfig) (store.ManagerSupplyConfig, error) {
	current, _, _, err := s.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return store.ManagerSupplyConfig{}, err
	}
	next := current
	next.Supply = NormalizeSupplyConfig(submitted, current.Supply)
	if err := ValidateSupplyConfig(next.Supply); err != nil {
		return store.ManagerSupplyConfig{}, err
	}
	if err := s.store.SaveManagerConfig(ctx, next); err != nil {
		return store.ManagerSupplyConfig{}, err
	}
	result := next.Supply
	result.PasswordConfigured = strings.TrimSpace(result.Password) != ""
	result.Password = ""
	for index := range result.Platforms {
		result.Platforms[index].PasswordConfigured = strings.TrimSpace(result.Platforms[index].Password) != ""
		result.Platforms[index].TokenConfigured = strings.TrimSpace(result.Platforms[index].Token) != ""
		result.Platforms[index].Password = ""
		result.Platforms[index].Token = ""
	}
	return result, nil
}

func (s *Service) ResolveSetup(ctx context.Context) (store.Setup, bool, error) {
	setup, _, ok, err := s.ResolveSetupWithSource(ctx)
	return setup, ok, err
}

func (s *Service) ResolveSetupWithSource(ctx context.Context) (store.Setup, Source, bool, error) {
	if s.cfg.CPAUpstreamURL != "" && s.cfg.ManagementKey != "" {
		return store.Setup{
			CPAUpstreamURL: cpa.NormalizeBaseURL(s.cfg.CPAUpstreamURL),
			ManagementKey:  s.cfg.ManagementKey,
			Queue:          s.cfg.Queue,
			PopSide:        s.cfg.PopSide,
		}, SourceEnv, true, nil
	}
	if managerCfg, _, ok, err := s.ResolveManagerConfigWithSource(ctx); err != nil {
		return store.Setup{}, SourceNone, false, err
	} else if ok && managerCfg.CPAConnection.CPABaseURL != "" && managerCfg.CPAConnection.ManagementKey != "" {
		return SetupFromManagerConfig(managerCfg), SourceDB, true, nil
	}
	setup, ok, err := s.store.LoadSetup(ctx)
	if !ok || err != nil {
		return setup, SourceNone, ok, err
	}
	return setup, SourceDB, true, nil
}

func (s *Service) ResolveManagerConfigWithSource(ctx context.Context) (store.ManagerConfig, Source, bool, error) {
	cfg := s.DefaultManagerConfig()
	source := SourceNone
	found := false

	if saved, ok, err := s.store.LoadManagerConfig(ctx); err != nil {
		return cfg, source, false, err
	} else if ok {
		cfg = s.MergeSubmittedManagerConfig(cfg, saved)
		source = SourceDB
		found = true
	}

	if setup, ok, err := s.store.LoadSetup(ctx); err != nil {
		return cfg, source, false, err
	} else if ok && cfg.CPAConnection.CPABaseURL == "" && cfg.CPAConnection.ManagementKey == "" {
		cfg.CPAConnection.CPABaseURL = cpa.NormalizeBaseURL(setup.CPAUpstreamURL)
		cfg.CPAConnection.ManagementKey = setup.ManagementKey
		cfg.Collector.Queue = ValueOr(setup.Queue, cfg.Collector.Queue)
		cfg.Collector.PopSide = NormalizePopSide(setup.PopSide, cfg.Collector.PopSide)
		source = SourceDB
		found = true
	}

	if s.cfg.CPAUpstreamURL != "" && s.cfg.ManagementKey != "" {
		cfg.CPAConnection.CPABaseURL = cpa.NormalizeBaseURL(s.cfg.CPAUpstreamURL)
		cfg.CPAConnection.ManagementKey = s.cfg.ManagementKey
		cfg.Collector.CollectorMode = CollectorMode(s.cfg.CollectorMode)
		cfg.Collector.Queue = ValueOr(s.cfg.Queue, cfg.Collector.Queue)
		cfg.Collector.PopSide = NormalizePopSide(s.cfg.PopSide, cfg.Collector.PopSide)
		cfg.Collector.BatchSize = PositiveOrDefault(s.cfg.BatchSize, cfg.Collector.BatchSize, 100)
		cfg.Collector.PollIntervalMS = PositiveOrDefault(int(s.cfg.PollInterval/time.Millisecond), cfg.Collector.PollIntervalMS, 500)
		cfg.Collector.QueryLimit = PositiveOrDefault(s.cfg.QueryLimit, cfg.Collector.QueryLimit, 50000)
		cfg.Collector.TLSSkipVerify = s.cfg.TLSSkipVerify
		source = SourceEnv
		found = true
	}

	return cfg, source, found, nil
}

func (s *Service) DefaultManagerConfig() store.ManagerConfig {
	pollIntervalMS := int(s.cfg.PollInterval / time.Millisecond)
	return store.ManagerConfig{
		Collector: store.ManagerCollectorConfig{
			Enabled:        BoolPtr(true),
			CollectorMode:  CollectorMode(s.cfg.CollectorMode),
			Queue:          ValueOr(s.cfg.Queue, "usage"),
			PopSide:        NormalizePopSide(s.cfg.PopSide, "right"),
			BatchSize:      PositiveOrDefault(s.cfg.BatchSize, 100, 100),
			PollIntervalMS: PositiveOrDefault(pollIntervalMS, 500, 500),
			QueryLimit:     PositiveOrDefault(s.cfg.QueryLimit, 50000, 50000),
			TLSSkipVerify:  s.cfg.TLSSkipVerify,
		},
		CodexInspection: store.DefaultCodexInspectionConfig(),
		Supply: store.ManagerSupplyConfig{
			Enabled:                     BoolPtr(false),
			BaseURL:                     "https://sogouedu.cc",
			Product:                     "oauth_30d",
			TargetAvailableAccounts:     100,
			ReplenishBatchSize:          10,
			MaxConcurrentOrders:         3,
			CheckIntervalSeconds:        60,
			PollIntervalSeconds:         3,
			SmartEnabled:                BoolPtr(true),
			HealthyMinutesTarget:        120,
			WarningMinutes:              60,
			CriticalMinutes:             30,
			PrelockEnabled:              BoolPtr(true),
			PrelockMinQuantity:          1,
			PrelockMaxQuantity:          10,
			CriticalTakeConfirmRounds:   2,
			CreateCooldownSeconds:       120,
			ReleaseCooldownSeconds:      60,
			AuthFilesCacheTTLSeconds:    60,
			MinHoldSeconds:              30,
			NewAccountConfidence:        0.7,
			RevenueMultiplier:           0.06,
			Strategy:                    SupplyStrategyStrongSupply,
			CriticalAvailableAccounts:   2,
			HealthyAvailableAccounts:    10,
			DefaultEmergencyMinAccounts: 5,
			VirtualDemandTTLMinutes:     60,
			AccountMaxRequestsBefore401: 30,
			AccountMaxUsefulSeconds401:  120,
			EmergencyBypassUsageRate:    BoolPtr(true),
			RecoveryTriggerOn401:        BoolPtr(true),
			RecoverySyncEnabled:         BoolPtr(true),
			RecoveryAutoClaim:           BoolPtr(true),
			RecoverySyncIntervalSeconds: 60,
			RecoveryClaimBatchSize:      20,
			RecoveryDisableOriginal:     BoolPtr(true),
			QuotaEstimationPolicies:     defaultSupplyQuotaEstimationPolicies(),
			PlatformSelectionStrategy:   SupplyPlatformSelectionBestAvailable,
		},
	}
}

func (s *Service) MergeSubmittedManagerConfig(base store.ManagerConfig, submitted store.ManagerConfig) store.ManagerConfig {
	next := base

	if submitted.CPAConnection.CPABaseURL != "" || submitted.CPAConnection.ManagementKey != "" {
		next.CPAConnection.CPABaseURL = cpa.NormalizeBaseURL(submitted.CPAConnection.CPABaseURL)
		next.CPAConnection.ManagementKey = strings.TrimSpace(submitted.CPAConnection.ManagementKey)
	}

	if submitted.Collector.Enabled != nil {
		next.Collector.Enabled = BoolPtr(*submitted.Collector.Enabled)
	}
	next.Collector.CollectorMode = CollectorMode(ValueOr(submitted.Collector.CollectorMode, next.Collector.CollectorMode))
	next.Collector.Queue = ValueOr(strings.TrimSpace(submitted.Collector.Queue), next.Collector.Queue)
	next.Collector.PopSide = NormalizePopSide(submitted.Collector.PopSide, next.Collector.PopSide)
	next.Collector.BatchSize = PositiveOrDefault(submitted.Collector.BatchSize, next.Collector.BatchSize, 100)
	next.Collector.PollIntervalMS = PositiveOrDefault(submitted.Collector.PollIntervalMS, next.Collector.PollIntervalMS, 500)
	next.Collector.QueryLimit = PositiveOrDefault(submitted.Collector.QueryLimit, next.Collector.QueryLimit, 50000)
	next.Collector.TLSSkipVerify = submitted.Collector.TLSSkipVerify

	next.CodexInspection = store.NormalizeCodexInspectionConfig(submitted.CodexInspection, next.CodexInspection)
	next.Supply = NormalizeSupplyConfig(submitted.Supply, next.Supply)

	next.ExternalUsageService.Enabled = false
	next.ExternalUsageService.ServiceBase = ""

	return next
}

func NormalizeSupplyConfig(submitted store.ManagerSupplyConfig, current store.ManagerSupplyConfig) store.ManagerSupplyConfig {
	next := current
	if submitted.Enabled != nil {
		next.Enabled = BoolPtr(*submitted.Enabled)
	}
	credentialIdentityChanged := false
	if value := strings.TrimSpace(submitted.BaseURL); value != "" {
		normalized := strings.TrimRight(value, "/")
		if normalized != strings.TrimRight(strings.TrimSpace(current.BaseURL), "/") {
			credentialIdentityChanged = true
		}
		next.BaseURL = normalized
	}
	if submitted.ClearUsername {
		credentialIdentityChanged = true
		next.Username = ""
	} else if value := strings.TrimSpace(submitted.Username); value != "" {
		if value != strings.TrimSpace(current.Username) {
			credentialIdentityChanged = true
		}
		next.Username = value
	}
	passwordSubmitted := strings.TrimSpace(submitted.Password) != ""
	if passwordSubmitted {
		next.Password = strings.TrimSpace(submitted.Password)
	} else if credentialIdentityChanged {
		next.Password = ""
	}
	next.ClearUsername = false
	if value := strings.ToLower(strings.TrimSpace(submitted.Product)); value != "" {
		next.Product = value
	}
	next.Strategy = NormalizeSupplyStrategy(ValueOr(submitted.Strategy, next.Strategy))
	next.TargetAvailableAccounts = BoundedPositiveOrDefault(submitted.TargetAvailableAccounts, next.TargetAvailableAccounts, 100, 10000)
	next.ReplenishBatchSize = BoundedPositiveOrDefault(submitted.ReplenishBatchSize, next.ReplenishBatchSize, 10, 100)
	next.MaxConcurrentOrders = BoundedPositiveOrDefault(submitted.MaxConcurrentOrders, next.MaxConcurrentOrders, 3, 3)
	next.CheckIntervalSeconds = BoundedPositiveOrDefault(submitted.CheckIntervalSeconds, next.CheckIntervalSeconds, 60, 3600)
	next.PollIntervalSeconds = BoundedPositiveOrDefault(submitted.PollIntervalSeconds, next.PollIntervalSeconds, 3, 60)
	next.DefaultWebsockets = submitted.DefaultWebsockets
	if submitted.SmartEnabled != nil {
		next.SmartEnabled = BoolPtr(*submitted.SmartEnabled)
	} else if next.SmartEnabled == nil {
		next.SmartEnabled = BoolPtr(true)
	}
	next.HealthyMinutesTarget = BoundedPositiveOrDefault(submitted.HealthyMinutesTarget, next.HealthyMinutesTarget, 120, 1440)
	next.WarningMinutes = BoundedPositiveOrDefault(submitted.WarningMinutes, next.WarningMinutes, 60, next.HealthyMinutesTarget)
	if next.WarningMinutes >= next.HealthyMinutesTarget {
		next.WarningMinutes = max(1, next.HealthyMinutesTarget/2)
	}
	next.CriticalMinutes = BoundedPositiveOrDefault(submitted.CriticalMinutes, next.CriticalMinutes, 30, next.WarningMinutes)
	if next.CriticalMinutes >= next.WarningMinutes {
		next.CriticalMinutes = max(1, next.WarningMinutes/2)
	}
	if submitted.PrelockEnabled != nil {
		next.PrelockEnabled = BoolPtr(*submitted.PrelockEnabled)
	} else if next.PrelockEnabled == nil {
		next.PrelockEnabled = BoolPtr(true)
	}
	next.PrelockMinQuantity = BoundedPositiveOrDefault(submitted.PrelockMinQuantity, next.PrelockMinQuantity, 1, 100)
	next.PrelockMaxQuantity = BoundedPositiveOrDefault(submitted.PrelockMaxQuantity, next.PrelockMaxQuantity, 10, 100)
	if next.PrelockMaxQuantity < next.PrelockMinQuantity {
		next.PrelockMaxQuantity = next.PrelockMinQuantity
	}
	next.CriticalTakeConfirmRounds = BoundedPositiveOrDefault(submitted.CriticalTakeConfirmRounds, next.CriticalTakeConfirmRounds, 2, 5)
	next.CreateCooldownSeconds = BoundedPositiveOrDefault(submitted.CreateCooldownSeconds, next.CreateCooldownSeconds, 120, 3600)
	next.ReleaseCooldownSeconds = BoundedPositiveOrDefault(submitted.ReleaseCooldownSeconds, next.ReleaseCooldownSeconds, 60, 3600)
	next.AuthFilesCacheTTLSeconds = BoundedPositiveOrDefault(submitted.AuthFilesCacheTTLSeconds, next.AuthFilesCacheTTLSeconds, 60, 600)
	if next.AuthFilesCacheTTLSeconds < 10 {
		next.AuthFilesCacheTTLSeconds = 10
	}
	next.MinHoldSeconds = BoundedPositiveOrDefault(submitted.MinHoldSeconds, next.MinHoldSeconds, 30, 3600)
	next.NewAccountConfidence = BoundedFloatOrDefault(submitted.NewAccountConfidence, next.NewAccountConfidence, 0.7, 0.1, 1)
	next.MinBalanceReserveFen = BoundedOptionalInt64(submitted.MinBalanceReserveFen, next.MinBalanceReserveFen, 100_000_000)
	next.RevenueMultiplier = BoundedFloatOrDefault(submitted.RevenueMultiplier, next.RevenueMultiplier, 0.06, 0.000001, 100)
	if submitted.StartupAvailableAccounts != nil {
		value := clampInt(*submitted.StartupAvailableAccounts, 1, 1000)
		next.StartupAvailableAccounts = &value
	}
	next = NormalizeSupplyStrategyConfig(next, submitted)
	if next.StartupAvailableAccounts != nil {
		value := clampInt(*next.StartupAvailableAccounts, 1, 1000)
		if value < next.CriticalAvailableAccounts {
			value = next.CriticalAvailableAccounts
		}
		next.StartupAvailableAccounts = &value
	}
	if submitted.RecoverySyncEnabled != nil {
		next.RecoverySyncEnabled = BoolPtr(*submitted.RecoverySyncEnabled)
	} else if next.RecoverySyncEnabled == nil {
		next.RecoverySyncEnabled = BoolPtr(true)
	}
	if submitted.RecoveryAutoClaim != nil {
		next.RecoveryAutoClaim = BoolPtr(*submitted.RecoveryAutoClaim)
	} else if next.RecoveryAutoClaim == nil {
		next.RecoveryAutoClaim = BoolPtr(true)
	}
	next.RecoverySyncIntervalSeconds = BoundedPositiveOrDefault(submitted.RecoverySyncIntervalSeconds, next.RecoverySyncIntervalSeconds, 60, 3600)
	next.RecoveryClaimBatchSize = BoundedPositiveOrDefault(submitted.RecoveryClaimBatchSize, next.RecoveryClaimBatchSize, 20, 100)
	if submitted.RecoveryDisableOriginal != nil {
		next.RecoveryDisableOriginal = BoolPtr(*submitted.RecoveryDisableOriginal)
	} else if next.RecoveryDisableOriginal == nil {
		next.RecoveryDisableOriginal = BoolPtr(true)
	}
	next.QuotaEstimationPolicies = normalizeSupplyQuotaEstimationPolicies(
		submitted.QuotaEstimationPolicies,
		next.QuotaEstimationPolicies,
	)
	if submitted.Platforms != nil {
		next.Platforms = normalizeSupplyPlatforms(submitted.Platforms, current)
		if primary, ok := primarySupplyPlatform(next.Platforms); ok {
			next.BaseURL = primary.BaseURL
			next.Username = primary.Username
			next.Password = primary.Password
			next.Product = primary.Product
		}
	}
	selection := strings.ToLower(strings.TrimSpace(submitted.PlatformSelectionStrategy))
	if selection == "" {
		selection = strings.ToLower(strings.TrimSpace(next.PlatformSelectionStrategy))
	}
	if selection != SupplyPlatformSelectionBestAvailable && selection != SupplyPlatformSelectionPriorityFirst {
		selection = SupplyPlatformSelectionBestAvailable
	}
	next.PlatformSelectionStrategy = selection
	next.PasswordConfigured = next.Password != ""
	return next
}

func normalizeSupplyPlatforms(submitted []store.ManagerSupplyPlatformConfig, current store.ManagerSupplyConfig) []store.ManagerSupplyPlatformConfig {
	currentPlatforms := SupplyPlatforms(current)
	currentByID := make(map[string]store.ManagerSupplyPlatformConfig, len(currentPlatforms))
	for _, platform := range currentPlatforms {
		currentByID[strings.ToLower(strings.TrimSpace(platform.ID))] = platform
	}
	result := make([]store.ManagerSupplyPlatformConfig, 0, len(submitted))
	seen := make(map[string]struct{}, len(submitted))
	for index, raw := range submitted {
		platformType := normalizeSupplyPlatformType(raw.Type)
		id := normalizeSupplyPlatformID(raw.ID)
		if id == "" {
			id = fmt.Sprintf("%s-%d", platformType, index+1)
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		previous := currentByID[id]
		platform := previous
		platform.ID = id
		platform.Type = platformType
		if raw.Enabled != nil {
			platform.Enabled = BoolPtr(*raw.Enabled)
		} else if platform.Enabled == nil {
			platform.Enabled = BoolPtr(true)
		}
		if value := strings.TrimSpace(raw.Name); value != "" {
			platform.Name = value
		} else if strings.TrimSpace(platform.Name) == "" {
			platform.Name = defaultSupplyPlatformName(platformType)
		}
		identityChanged := false
		baseURLChanged := false
		if value := strings.TrimRight(strings.TrimSpace(raw.BaseURL), "/"); value != "" {
			baseURLChanged = !strings.EqualFold(value, strings.TrimRight(strings.TrimSpace(previous.BaseURL), "/"))
			identityChanged = baseURLChanged
			platform.BaseURL = value
		} else if strings.TrimSpace(platform.BaseURL) == "" && platformType == SupplyPlatformBugTeam {
			platform.BaseURL = "https://bugteam.team"
		}
		if raw.ClearUsername {
			identityChanged = true
			platform.Username = ""
		} else if value := strings.TrimSpace(raw.Username); value != "" {
			identityChanged = identityChanged || value != strings.TrimSpace(previous.Username)
			platform.Username = value
		}
		if value := strings.TrimSpace(raw.Password); value != "" {
			platform.Password = value
		} else if raw.ClearUsername {
			platform.Password = ""
		} else if identityChanged {
			platform.Password = credentialDonorPassword(currentPlatforms, platform.Username)
		}
		if value := strings.TrimSpace(raw.Token); value != "" {
			platform.Token = value
		} else if identityChanged {
			if raw.ClearUsername && !baseURLChanged {
				platform.Token = previous.Token
			} else {
				platform.Token = ""
			}
		}
		platform.ClearUsername = false
		if value := strings.ToLower(strings.TrimSpace(raw.Product)); value != "" {
			platform.Product = value
		} else if strings.TrimSpace(platform.Product) == "" {
			if platformType == SupplyPlatformBugTeam {
				platform.Product = "team_1h"
			} else {
				platform.Product = "oauth_30d"
			}
		}
		if platformType == SupplyPlatformNvtokens {
			purchaseAccountType := strings.TrimSpace(raw.PurchaseAccountType)
			if purchaseAccountType == "" {
				purchaseAccountType = platform.PurchaseAccountType
			}
			platform.PurchaseAccountType = normalizeSupplyPurchaseAccountType(purchaseAccountType)
			platform.MaxUnitPriceFen = normalizeSupplyMaxUnitPriceFen(raw.MaxUnitPriceFen, platform.MaxUnitPriceFen)
		} else {
			platform.PurchaseAccountType = ""
			platform.MaxUnitPriceFen = nil
		}
		if raw.Priority > 0 {
			platform.Priority = clampInt(raw.Priority, 1, 1000)
		} else if platform.Priority <= 0 {
			platform.Priority = index + 1
		}
		platform.EmergencyOnly = raw.EmergencyOnly
		if raw.QuotaEstimationPolicies != nil {
			platform.QuotaEstimationPolicies = normalizeSupplyQuotaEstimationPolicyOverrides(
				raw.QuotaEstimationPolicies,
				previous.QuotaEstimationPolicies,
			)
		}
		platform.PasswordConfigured = platform.Password != ""
		platform.TokenConfigured = platform.Token != ""
		result = append(result, platform)
	}
	return result
}

func credentialDonorPassword(platforms []store.ManagerSupplyPlatformConfig, username string) string {
	username = strings.TrimSpace(username)
	if username == "" {
		return ""
	}
	for _, platform := range platforms {
		if strings.EqualFold(strings.TrimSpace(platform.Username), username) && strings.TrimSpace(platform.Password) != "" {
			return platform.Password
		}
	}
	return ""
}

func normalizeSupplyPlatformType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case SupplyPlatformBugTeam:
		return SupplyPlatformBugTeam
	case SupplyPlatformNvtokens:
		return SupplyPlatformNvtokens
	default:
		return SupplyPlatformLegacy
	}
}

func normalizeSupplyPurchaseAccountType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case SupplyPurchaseAccountHasRefreshToken, "access_refresh", "refresh_token":
		return SupplyPurchaseAccountHasRefreshToken
	case SupplyPurchaseAccountWithoutRefreshToken, "access_token", "id_token", "session_token", "unknown":
		return SupplyPurchaseAccountWithoutRefreshToken
	default:
		return SupplyPurchaseAccountAll
	}
}

func supportedSupplyPurchaseAccountType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", SupplyPurchaseAccountAll,
		SupplyPurchaseAccountHasRefreshToken, "access_refresh", "refresh_token",
		SupplyPurchaseAccountWithoutRefreshToken, "access_token", "id_token", "session_token", "unknown":
		return true
	default:
		return false
	}
}

func normalizeSupplyMaxUnitPriceFen(submitted *int64, current *int64) *int64 {
	if submitted == nil {
		if current == nil || *current <= 0 {
			return nil
		}
		value := min(*current, int64(100_000_000))
		return &value
	}
	if *submitted <= 0 {
		return nil
	}
	value := min(*submitted, int64(100_000_000))
	return &value
}

func normalizeSupplyPlatformID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			builder.WriteRune(r)
		case builder.Len() > 0:
			builder.WriteByte('-')
		}
		if builder.Len() >= 64 {
			break
		}
	}
	return strings.Trim(builder.String(), "-_")
}

func defaultSupplyPlatformName(platformType string) string {
	if platformType == SupplyPlatformBugTeam {
		return "BugTeam"
	}
	if platformType == SupplyPlatformNvtokens {
		return "nvtokens"
	}
	return "Legacy supplier"
}

func primarySupplyPlatform(platforms []store.ManagerSupplyPlatformConfig) (store.ManagerSupplyPlatformConfig, bool) {
	for _, platform := range platforms {
		if SupplyPlatformEnabled(platform) {
			return platform, true
		}
	}
	if len(platforms) > 0 {
		return platforms[0], true
	}
	return store.ManagerSupplyPlatformConfig{}, false
}

// SupplyPlatforms returns configured platforms or a synthesized legacy
// platform for configurations saved by earlier Manager versions.
func SupplyPlatforms(cfg store.ManagerSupplyConfig) []store.ManagerSupplyPlatformConfig {
	if cfg.Platforms != nil {
		return append([]store.ManagerSupplyPlatformConfig(nil), cfg.Platforms...)
	}
	if strings.TrimSpace(cfg.BaseURL) == "" && strings.TrimSpace(cfg.Username) == "" && strings.TrimSpace(cfg.Password) == "" {
		return nil
	}
	enabled := true
	return []store.ManagerSupplyPlatformConfig{{
		ID:                 SupplyPlatformLegacy,
		Name:               defaultSupplyPlatformName(SupplyPlatformLegacy),
		Type:               SupplyPlatformLegacy,
		Enabled:            &enabled,
		BaseURL:            strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		Username:           strings.TrimSpace(cfg.Username),
		Password:           cfg.Password,
		PasswordConfigured: strings.TrimSpace(cfg.Password) != "",
		Product:            strings.ToLower(strings.TrimSpace(cfg.Product)),
		Priority:           1,
	}}
}

func SupplyPlatformEnabled(cfg store.ManagerSupplyPlatformConfig) bool {
	return cfg.Enabled == nil || *cfg.Enabled
}

func defaultSupplyQuotaEstimationPolicies() map[string]store.ManagerSupplyQuotaEstimationPolicy {
	return map[string]store.ManagerSupplyQuotaEstimationPolicy{
		"team": {Mode: SupplyQuotaEstimationAuto, FallbackM: 60, FixedM: 60},
		"free": {Mode: SupplyQuotaEstimationAuto, FallbackM: 10, FixedM: 10},
	}
}

func normalizeSupplyQuotaEstimationPolicies(
	submitted map[string]store.ManagerSupplyQuotaEstimationPolicy,
	current map[string]store.ManagerSupplyQuotaEstimationPolicy,
) map[string]store.ManagerSupplyQuotaEstimationPolicy {
	result := defaultSupplyQuotaEstimationPolicies()
	for planType, policy := range current {
		planType = strings.ToLower(strings.TrimSpace(planType))
		if planType == "" {
			continue
		}
		result[planType] = normalizeSupplyQuotaEstimationPolicy(planType, policy, result[planType])
	}
	for planType, policy := range submitted {
		planType = strings.ToLower(strings.TrimSpace(planType))
		if planType == "" || len(planType) > 64 {
			continue
		}
		result[planType] = normalizeSupplyQuotaEstimationPolicy(planType, policy, result[planType])
	}
	return result
}

func normalizeSupplyQuotaEstimationPolicyOverrides(
	submitted map[string]store.ManagerSupplyQuotaEstimationPolicy,
	current map[string]store.ManagerSupplyQuotaEstimationPolicy,
) map[string]store.ManagerSupplyQuotaEstimationPolicy {
	result := make(map[string]store.ManagerSupplyQuotaEstimationPolicy, len(current)+len(submitted))
	for planType, policy := range current {
		planType = strings.ToLower(strings.TrimSpace(planType))
		if planType == "" || len(planType) > 64 {
			continue
		}
		fallback := defaultSupplyQuotaEstimationPolicies()[planType]
		result[planType] = normalizeSupplyQuotaEstimationPolicy(planType, policy, fallback)
	}
	for planType, policy := range submitted {
		planType = strings.ToLower(strings.TrimSpace(planType))
		if planType == "" || len(planType) > 64 {
			continue
		}
		fallback := result[planType]
		if fallback.FallbackM <= 0 {
			fallback = defaultSupplyQuotaEstimationPolicies()[planType]
		}
		result[planType] = normalizeSupplyQuotaEstimationPolicy(planType, policy, fallback)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func normalizeSupplyQuotaEstimationPolicy(
	planType string,
	policy store.ManagerSupplyQuotaEstimationPolicy,
	current store.ManagerSupplyQuotaEstimationPolicy,
) store.ManagerSupplyQuotaEstimationPolicy {
	mode := strings.ToLower(strings.TrimSpace(policy.Mode))
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(current.Mode))
	}
	if mode != SupplyQuotaEstimationFixed {
		mode = SupplyQuotaEstimationAuto
	}
	defaultFallback := 10.0
	if planType == "team" {
		defaultFallback = 60
	}
	fallbackM := BoundedFloatOrDefault(policy.FallbackM, current.FallbackM, defaultFallback, 0.5, 500)
	fixedM := BoundedFloatOrDefault(policy.FixedM, current.FixedM, fallbackM, 0.5, 500)
	return store.ManagerSupplyQuotaEstimationPolicy{Mode: mode, FallbackM: fallbackM, FixedM: fixedM}
}

func NormalizeSupplyStrategy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case SupplyStrategyBalanced:
		return SupplyStrategyBalanced
	case SupplyStrategyCostFirst:
		return SupplyStrategyCostFirst
	case SupplyStrategyCustom:
		return SupplyStrategyCustom
	case SupplyStrategyStrongSupply, "":
		return SupplyStrategyStrongSupply
	default:
		return SupplyStrategyStrongSupply
	}
}

func NormalizeSupplyStrategyConfig(current store.ManagerSupplyConfig, submitted store.ManagerSupplyConfig) store.ManagerSupplyConfig {
	current.Strategy = NormalizeSupplyStrategy(current.Strategy)
	if current.Strategy != SupplyStrategyCustom {
		preset := SupplyStrategyPreset(current.Strategy)
		current.CriticalAvailableAccounts = preset.CriticalAvailableAccounts
		current.HealthyAvailableAccounts = preset.HealthyAvailableAccounts
		current.DefaultEmergencyMinAccounts = preset.DefaultEmergencyMinAccounts
		current.VirtualDemandTTLMinutes = preset.VirtualDemandTTLMinutes
		current.AccountMaxRequestsBefore401 = preset.AccountMaxRequestsBefore401
		current.AccountMaxUsefulSeconds401 = preset.AccountMaxUsefulSeconds401
		current.EmergencyBypassUsageRate = BoolPtr(preset.EmergencyBypassUsageRate)
		current.RecoveryTriggerOn401 = BoolPtr(preset.RecoveryTriggerOn401)
		return current
	}
	current.CriticalAvailableAccounts = clampInt(NonNegativeOrDefault(submitted.CriticalAvailableAccounts, current.CriticalAvailableAccounts, 2), 0, 1000)
	current.HealthyAvailableAccounts = clampInt(NonNegativeOrDefault(submitted.HealthyAvailableAccounts, current.HealthyAvailableAccounts, 10), 0, 10000)
	current.DefaultEmergencyMinAccounts = BoundedPositiveOrDefault(submitted.DefaultEmergencyMinAccounts, current.DefaultEmergencyMinAccounts, 5, 100)
	current.VirtualDemandTTLMinutes = BoundedPositiveOrDefault(submitted.VirtualDemandTTLMinutes, current.VirtualDemandTTLMinutes, 60, 180)
	current.AccountMaxRequestsBefore401 = BoundedPositiveOrDefault(submitted.AccountMaxRequestsBefore401, current.AccountMaxRequestsBefore401, 30, 100000)
	current.AccountMaxUsefulSeconds401 = BoundedPositiveOrDefault(submitted.AccountMaxUsefulSeconds401, current.AccountMaxUsefulSeconds401, 120, 3600)
	if submitted.EmergencyBypassUsageRate != nil {
		current.EmergencyBypassUsageRate = BoolPtr(*submitted.EmergencyBypassUsageRate)
	} else if current.EmergencyBypassUsageRate == nil {
		current.EmergencyBypassUsageRate = BoolPtr(true)
	}
	if submitted.RecoveryTriggerOn401 != nil {
		current.RecoveryTriggerOn401 = BoolPtr(*submitted.RecoveryTriggerOn401)
	} else if current.RecoveryTriggerOn401 == nil {
		current.RecoveryTriggerOn401 = BoolPtr(true)
	}
	if current.HealthyAvailableAccounts < current.CriticalAvailableAccounts {
		current.HealthyAvailableAccounts = current.CriticalAvailableAccounts
	}
	return current
}

func SupplyStrategyPreset(strategy string) supplyStrategyPreset {
	switch NormalizeSupplyStrategy(strategy) {
	case SupplyStrategyBalanced:
		return supplyStrategyPreset{
			Strategy:                    SupplyStrategyBalanced,
			CriticalAvailableAccounts:   1,
			HealthyAvailableAccounts:    5,
			DefaultEmergencyMinAccounts: 3,
			VirtualDemandTTLMinutes:     30,
			AccountMaxRequestsBefore401: 40,
			AccountMaxUsefulSeconds401:  150,
			EmergencyBypassUsageRate:    true,
			RecoveryTriggerOn401:        true,
		}
	case SupplyStrategyCostFirst:
		return supplyStrategyPreset{
			Strategy:                    SupplyStrategyCostFirst,
			CriticalAvailableAccounts:   2,
			HealthyAvailableAccounts:    3,
			DefaultEmergencyMinAccounts: 1,
			VirtualDemandTTLMinutes:     15,
			AccountMaxRequestsBefore401: 50,
			AccountMaxUsefulSeconds401:  180,
			EmergencyBypassUsageRate:    true,
			RecoveryTriggerOn401:        true,
		}
	default:
		return supplyStrategyPreset{
			Strategy:                    SupplyStrategyStrongSupply,
			CriticalAvailableAccounts:   2,
			HealthyAvailableAccounts:    10,
			DefaultEmergencyMinAccounts: 5,
			VirtualDemandTTLMinutes:     60,
			AccountMaxRequestsBefore401: 30,
			AccountMaxUsefulSeconds401:  120,
			EmergencyBypassUsageRate:    true,
			RecoveryTriggerOn401:        true,
		}
	}
}

func ValidateSupplyConfig(cfg store.ManagerSupplyConfig) error {
	if !SupplyEnabled(cfg) {
		return nil
	}
	platforms := SupplyPlatforms(cfg)
	enabled := 0
	for _, platform := range platforms {
		if !SupplyPlatformEnabled(platform) {
			continue
		}
		enabled++
		if err := validateSupplyPlatform(platform); err != nil {
			return fmt.Errorf("supply platform %s: %w", firstNonEmpty(platform.Name, platform.ID), err)
		}
	}
	if enabled == 0 {
		return errors.New("at least one enabled supply platform is required")
	}
	return nil
}

func validateSupplyPlatform(platform store.ManagerSupplyPlatformConfig) error {
	parsed, err := url.Parse(strings.TrimSpace(platform.BaseURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("baseUrl must be a valid HTTP or HTTPS URL")
	}
	if strings.TrimSpace(platform.Token) == "" &&
		(strings.TrimSpace(platform.Username) == "" || strings.TrimSpace(platform.Password) == "") {
		return errors.New("token or username and password are required")
	}
	switch normalizeSupplyPlatformType(platform.Type) {
	case SupplyPlatformBugTeam:
		if !strings.EqualFold(strings.TrimSpace(platform.Product), "team_1h") {
			return errors.New("product must be team_1h")
		}
	case SupplyPlatformNvtokens:
		if !supportedSupplyPurchaseAccountType(platform.PurchaseAccountType) {
			return errors.New("purchaseAccountType must be all, has_refresh_token or without_refresh_token")
		}
		if platform.MaxUnitPriceFen != nil && *platform.MaxUnitPriceFen < 0 {
			return errors.New("maxUnitPriceFen must be zero or greater")
		}
		switch strings.ToLower(strings.TrimSpace(platform.Product)) {
		case "oauth_30d", "oauth_7d", "team_1h":
		default:
			return errors.New("product must be oauth_30d, oauth_7d or team_1h")
		}
	default:
		switch strings.ToLower(strings.TrimSpace(platform.Product)) {
		case "oauth_30d", "oauth_7d", "team_1h":
		default:
			return errors.New("product must be oauth_30d, oauth_7d or team_1h")
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "unknown"
}

func SanitizeManagerConfig(cfg store.ManagerConfig) store.ManagerConfig {
	cfg.Supply.PasswordConfigured = strings.TrimSpace(cfg.Supply.Password) != ""
	cfg.Supply.Password = ""
	for index := range cfg.Supply.Platforms {
		cfg.Supply.Platforms[index].PasswordConfigured = strings.TrimSpace(cfg.Supply.Platforms[index].Password) != ""
		cfg.Supply.Platforms[index].TokenConfigured = strings.TrimSpace(cfg.Supply.Platforms[index].Token) != ""
		cfg.Supply.Platforms[index].Password = ""
		cfg.Supply.Platforms[index].Token = ""
	}
	return cfg
}

func SupplyEnabled(cfg store.ManagerSupplyConfig) bool {
	return cfg.Enabled != nil && *cfg.Enabled
}

func SetupFromManagerConfig(cfg store.ManagerConfig) store.Setup {
	return store.Setup{
		CPAUpstreamURL: cfg.CPAConnection.CPABaseURL,
		ManagementKey:  cfg.CPAConnection.ManagementKey,
		Queue:          cfg.Collector.Queue,
		PopSide:        cfg.Collector.PopSide,
	}
}

func ManagerConfigConnectionDiffers(left store.ManagerConfig, right store.ManagerConfig) bool {
	return cpa.NormalizeBaseURL(left.CPAConnection.CPABaseURL) != cpa.NormalizeBaseURL(right.CPAConnection.CPABaseURL) ||
		left.CPAConnection.ManagementKey != right.CPAConnection.ManagementKey ||
		ManagerCollectorEnabled(left) != ManagerCollectorEnabled(right) ||
		left.Collector.CollectorMode != right.Collector.CollectorMode ||
		left.Collector.Queue != right.Collector.Queue ||
		left.Collector.PopSide != right.Collector.PopSide ||
		left.Collector.BatchSize != right.Collector.BatchSize ||
		left.Collector.PollIntervalMS != right.Collector.PollIntervalMS ||
		left.Collector.TLSSkipVerify != right.Collector.TLSSkipVerify
}

func ManagerConfigCPABindingDiffers(left store.ManagerConfig, right store.ManagerConfig) bool {
	leftBase := cpa.NormalizeBaseURL(left.CPAConnection.CPABaseURL)
	rightBase := cpa.NormalizeBaseURL(right.CPAConnection.CPABaseURL)
	if leftBase == "" || left.CPAConnection.ManagementKey == "" {
		return false
	}
	return leftBase != rightBase
}

func PositiveOrDefault(value int, fallback int, hardDefault int) int {
	if value > 0 {
		return value
	}
	if fallback > 0 {
		return fallback
	}
	return hardDefault
}

func BoundedPositiveOrDefault(value int, fallback int, hardDefault int, maximum int) int {
	result := PositiveOrDefault(value, fallback, hardDefault)
	if result > maximum {
		return maximum
	}
	return result
}

func BoundedFloatOrDefault(value float64, fallback float64, hardDefault float64, minimum float64, maximum float64) float64 {
	result := hardDefault
	if fallback > 0 {
		result = fallback
	}
	if value > 0 {
		result = value
	}
	if result < minimum {
		return minimum
	}
	if result > maximum {
		return maximum
	}
	return result
}

func BoundedOptionalInt64(value int64, fallback int64, maximum int64) int64 {
	result := fallback
	if value > 0 || fallback <= 0 {
		result = value
	}
	if result < 0 {
		return 0
	}
	if result > maximum {
		return maximum
	}
	return result
}

func NonNegativeOrDefault(value int, fallback int, hardDefault int) int {
	result := hardDefault
	if fallback >= 0 {
		result = fallback
	}
	if value >= 0 {
		result = value
	}
	return result
}

func clampInt(value int, minimum int, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func ValueOr(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func NormalizePopSide(value string, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "left", "right":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		if strings.ToLower(strings.TrimSpace(fallback)) == "left" {
			return "left"
		}
		return "right"
	}
}

func CollectorMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "http", "resp", "subscribe":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "auto"
	}
}

func BoolPtr(value bool) *bool {
	return &value
}

func ManagerCollectorEnabled(cfg store.ManagerConfig) bool {
	return cfg.Collector.Enabled == nil || *cfg.Collector.Enabled
}

func AuthHeaderMatches(header string, managementKey string) bool {
	header = strings.TrimSpace(header)
	if header == "" || managementKey == "" {
		return false
	}
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	return strings.TrimSpace(header[len(prefix):]) == managementKey
}
