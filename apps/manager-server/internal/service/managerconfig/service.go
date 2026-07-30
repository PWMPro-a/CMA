package managerconfig

import (
	"context"
	"errors"
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
)

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
			Enabled:                 BoolPtr(false),
			BaseURL:                 "https://sogouedu.cc",
			Product:                 "oauth_30d",
			TargetAvailableAccounts: 100,
			ReplenishBatchSize:      10,
			CheckIntervalSeconds:    60,
			PollIntervalSeconds:     3,
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
	if value := strings.TrimSpace(submitted.BaseURL); value != "" {
		next.BaseURL = strings.TrimRight(value, "/")
	}
	if value := strings.TrimSpace(submitted.Username); value != "" {
		next.Username = value
	}
	if value := strings.TrimSpace(submitted.Password); value != "" {
		next.Password = value
	}
	if value := strings.ToLower(strings.TrimSpace(submitted.Product)); value != "" {
		next.Product = value
	}
	next.TargetAvailableAccounts = BoundedPositiveOrDefault(submitted.TargetAvailableAccounts, next.TargetAvailableAccounts, 100, 10000)
	next.ReplenishBatchSize = BoundedPositiveOrDefault(submitted.ReplenishBatchSize, next.ReplenishBatchSize, 10, 100)
	next.CheckIntervalSeconds = BoundedPositiveOrDefault(submitted.CheckIntervalSeconds, next.CheckIntervalSeconds, 60, 3600)
	next.PollIntervalSeconds = BoundedPositiveOrDefault(submitted.PollIntervalSeconds, next.PollIntervalSeconds, 3, 60)
	next.DefaultWebsockets = submitted.DefaultWebsockets
	next.PasswordConfigured = next.Password != ""
	return next
}

func ValidateSupplyConfig(cfg store.ManagerSupplyConfig) error {
	if !SupplyEnabled(cfg) {
		return nil
	}
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Username) == "" || strings.TrimSpace(cfg.Password) == "" {
		return errors.New("supply baseUrl, username and password are required when automatic replenishment is enabled")
	}
	parsed, err := url.Parse(cfg.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("supply baseUrl must be a valid HTTP or HTTPS URL")
	}
	switch cfg.Product {
	case "oauth_30d", "oauth_7d":
	default:
		return errors.New("supply product must be oauth_30d or oauth_7d")
	}
	return nil
}

func SanitizeManagerConfig(cfg store.ManagerConfig) store.ManagerConfig {
	cfg.Supply.PasswordConfigured = strings.TrimSpace(cfg.Supply.Password) != ""
	cfg.Supply.Password = ""
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
