package managerconfig

import (
	"testing"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func TestNormalizeSupplyConfigKeepsPasswordWhenIdentityUnchanged(t *testing.T) {
	current := store.ManagerSupplyConfig{
		BaseURL:  "https://sogouedu.cc",
		Username: "customer-a",
		Password: "saved-password",
		Product:  "oauth_7d",
	}
	next := NormalizeSupplyConfig(store.ManagerSupplyConfig{
		BaseURL:  "https://sogouedu.cc/",
		Username: "customer-a",
		Product:  "oauth_30d",
	}, current)
	if next.Password != "saved-password" {
		t.Fatalf("password = %q, want saved password", next.Password)
	}
	if !next.PasswordConfigured {
		t.Fatal("password should stay configured")
	}
}

func TestNormalizeSupplyConfigLowPriceReserve(t *testing.T) {
	enabled := true
	ceiling := int64(199)
	next := NormalizeSupplyConfig(store.ManagerSupplyConfig{
		TargetAvailableAccounts:                  50,
		ReplenishBatchSize:                       8,
		LowPriceReserveEnabled:                   &enabled,
		LowPriceReserveMaxUnitPriceFen:           &ceiling,
		LowPriceReserveTargetAccounts:            80,
		LowPriceReserveCheckIntervalMilliseconds: 750,
	}, store.ManagerSupplyConfig{})
	if next.LowPriceReserveEnabled == nil || !*next.LowPriceReserveEnabled ||
		next.LowPriceReserveMaxUnitPriceFen == nil || *next.LowPriceReserveMaxUnitPriceFen != 199 ||
		next.LowPriceReserveTargetAccounts != 80 ||
		next.LowPriceReserveCheckIntervalMilliseconds != 750 {
		t.Fatalf("low-price reserve = %#v", next)
	}
	cleared := int64(0)
	next = NormalizeSupplyConfig(store.ManagerSupplyConfig{
		LowPriceReserveMaxUnitPriceFen: &cleared,
	}, next)
	if next.LowPriceReserveMaxUnitPriceFen == nil || *next.LowPriceReserveMaxUnitPriceFen != 0 {
		t.Fatalf("cleared low-price ceiling = %#v", next.LowPriceReserveMaxUnitPriceFen)
	}
}

func TestNormalizeAndValidateNvtokensSupplierQuotaGate(t *testing.T) {
	enabled := true
	next := NormalizeSupplyConfig(store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{{
		ID: "nv", Type: SupplyPlatformNvtokens, Enabled: &enabled,
		BaseURL: "https://nvtokens.com", Token: "session", Product: "plus",
	}}}, store.ManagerSupplyConfig{})
	if len(next.Platforms) != 1 {
		t.Fatalf("platforms = %#v", next.Platforms)
	}
	platform := next.Platforms[0]
	if platform.SupplierQuotaGateEnabled == nil || *platform.SupplierQuotaGateEnabled || platform.SupplierQuotaMinimumM != 30 || platform.SupplierQuotaTrialQuantity != 1 {
		t.Fatalf("supplier quota defaults = %#v", platform)
	}

	invalid := store.ManagerSupplyPlatformConfig{
		ID: "nv", Type: SupplyPlatformNvtokens, Enabled: &enabled,
		BaseURL: "https://nvtokens.com", Token: "session", Product: "plus",
	}
	invalid.SupplierQuotaGateEnabled = &enabled
	invalid.SupplierQuotaMinimumM = 0.4
	invalid.SupplierQuotaTrialQuantity = 1
	normalizedInvalid := NormalizeSupplyConfig(store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{invalid}}, store.ManagerSupplyConfig{})
	platform = normalizedInvalid.Platforms[0]
	platform.SupplierQuotaTrialQuantity = 1
	if err := validateSupplyPlatform(platform); err == nil {
		t.Fatal("minimum below 0.5M should fail validation")
	}
	invalid.SupplierQuotaMinimumM = 30
	invalid.SupplierQuotaTrialQuantity = 6
	normalizedInvalid = NormalizeSupplyConfig(store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{invalid}}, store.ManagerSupplyConfig{})
	platform = normalizedInvalid.Platforms[0]
	if err := validateSupplyPlatform(platform); err == nil {
		t.Fatal("trial quantity above 5 should fail validation")
	}
	platform.SupplierQuotaTrialQuantity = 1
	platform.SupplierQuotaMinimumM = 3500
	if err := validateSupplyPlatform(platform); err != nil {
		t.Fatalf("valid high supplier quota gate: %v", err)
	}
}

func TestValidateSupplyConfigRequiresLowPriceReserveCeiling(t *testing.T) {
	enabled := true
	cfg := store.ManagerSupplyConfig{
		Enabled:                       &enabled,
		LowPriceReserveEnabled:        &enabled,
		LowPriceReserveTargetAccounts: 20,
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID: "legacy", Type: SupplyPlatformLegacy, Enabled: &enabled,
			BaseURL: "https://example.com", Token: "token", Product: "oauth_30d",
		}},
	}
	if err := ValidateSupplyConfig(cfg); err == nil {
		t.Fatal("enabled low-price reserve without a price ceiling should fail validation")
	}
	ceiling := int64(100)
	cfg.LowPriceReserveMaxUnitPriceFen = &ceiling
	cfg.LowPriceReserveCheckIntervalMilliseconds = 1000
	if err := ValidateSupplyConfig(cfg); err != nil {
		t.Fatalf("valid low-price reserve: %v", err)
	}
	cfg.LowPriceReserveCheckIntervalMilliseconds = 100
	if err := ValidateSupplyConfig(cfg); err == nil {
		t.Fatal("sub-250ms low-price reserve polling should fail validation")
	}
}

func TestNormalizeSupplyConfigClearsPasswordWhenSupplyIdentityChangesWithoutPassword(t *testing.T) {
	current := store.ManagerSupplyConfig{
		BaseURL:  "https://sogouedu.cc",
		Username: "customer-a",
		Password: "saved-password",
		Product:  "oauth_7d",
	}
	for _, submitted := range []store.ManagerSupplyConfig{
		{BaseURL: "https://other.example", Username: "customer-a", Product: "oauth_7d"},
		{BaseURL: "https://sogouedu.cc", Username: "customer-b", Product: "oauth_7d"},
	} {
		next := NormalizeSupplyConfig(submitted, current)
		if next.Password != "" {
			t.Fatalf("password = %q, want cleared for submitted=%#v", next.Password, submitted)
		}
		if next.PasswordConfigured {
			t.Fatalf("passwordConfigured = true, want false for submitted=%#v", submitted)
		}
	}
}

func TestNormalizeSupplyConfigReplacesPasswordWhenIdentityChangesWithPassword(t *testing.T) {
	current := store.ManagerSupplyConfig{
		BaseURL:  "https://sogouedu.cc",
		Username: "customer-a",
		Password: "saved-password",
		Product:  "oauth_7d",
	}
	next := NormalizeSupplyConfig(store.ManagerSupplyConfig{
		BaseURL:  "https://sogouedu.cc",
		Username: "customer-b",
		Password: "new-password",
		Product:  "oauth_7d",
	}, current)
	if next.Password != "new-password" {
		t.Fatalf("password = %q, want new password", next.Password)
	}
	if !next.PasswordConfigured {
		t.Fatal("password should be configured")
	}
}

func TestNormalizeSupplyConfigExplicitlyClearsLegacyUsernameAndPassword(t *testing.T) {
	current := store.ManagerSupplyConfig{
		BaseURL:  "https://sogouedu.cc",
		Username: "customer-a",
		Password: "saved-password",
		Product:  "oauth_7d",
	}
	next := NormalizeSupplyConfig(store.ManagerSupplyConfig{
		BaseURL:       "https://sogouedu.cc",
		ClearUsername: true,
		Product:       "oauth_7d",
	}, current)
	if next.Username != "" || next.Password != "" || next.PasswordConfigured {
		t.Fatalf("cleared legacy credentials = %#v", next)
	}
	if next.ClearUsername {
		t.Fatal("clearUsername is a request intent and must not be persisted")
	}
}

func TestNormalizeSupplyConfigClearsPlatformUsernameButKeepsTokenAuthentication(t *testing.T) {
	enabled := true
	current := store.ManagerSupplyConfig{
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID:       "bugteam",
			Type:     SupplyPlatformBugTeam,
			Enabled:  &enabled,
			BaseURL:  "https://bugteam.team",
			Username: "customer-a",
			Password: "saved-password",
			Token:    "saved-token",
			Product:  "team_1h",
		}},
	}
	next := NormalizeSupplyConfig(store.ManagerSupplyConfig{
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID:            "bugteam",
			Type:          SupplyPlatformBugTeam,
			Enabled:       &enabled,
			BaseURL:       "https://bugteam.team",
			ClearUsername: true,
			Product:       "team_1h",
		}},
	}, current)
	if len(next.Platforms) != 1 {
		t.Fatalf("platforms = %#v", next.Platforms)
	}
	platform := next.Platforms[0]
	if platform.Username != "" || platform.Password != "" || platform.PasswordConfigured {
		t.Fatalf("cleared platform credentials = %#v", platform)
	}
	if platform.Token != "saved-token" || !platform.TokenConfigured {
		t.Fatalf("token authentication should be preserved: %#v", platform)
	}
	if platform.ClearUsername {
		t.Fatal("platform clearUsername is a request intent and must not be persisted")
	}
}

func TestNormalizeSupplyConfigKeepsNvtokensPlatformType(t *testing.T) {
	enabled := true
	maxUnitPriceFen := int64(800)
	next := NormalizeSupplyConfig(store.ManagerSupplyConfig{
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID:                  "nvtokens-main",
			Name:                "nvtokens",
			Type:                SupplyPlatformNvtokens,
			Enabled:             &enabled,
			BaseURL:             "https://nvtokens.com/",
			Username:            "buyer",
			Password:            "secret",
			Product:             "oauth_30d",
			PurchaseAccountType: "access_refresh",
			MaxUnitPriceFen:     &maxUnitPriceFen,
		}},
	}, store.ManagerSupplyConfig{})
	if len(next.Platforms) != 1 {
		t.Fatalf("platforms = %#v", next.Platforms)
	}
	platform := next.Platforms[0]
	if platform.Type != SupplyPlatformNvtokens || platform.BaseURL != "https://nvtokens.com" || platform.Product != "plus" {
		t.Fatalf("nvtokens platform = %#v", platform)
	}
	if platform.PurchaseAccountType != SupplyPurchaseAccountHasRefreshToken ||
		platform.MaxUnitPriceFen == nil || *platform.MaxUnitPriceFen != 800 {
		t.Fatalf("nvtokens purchase filters = %#v", platform)
	}
	if err := ValidateSupplyConfig(store.ManagerSupplyConfig{Enabled: &enabled, Platforms: next.Platforms}); err != nil {
		t.Fatalf("validate nvtokens platform: %v", err)
	}
}

func TestNormalizeSupplyConfigPreservesNvtokensSessionRefreshSecret(t *testing.T) {
	enabled := true
	current := store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{{
		ID: "nvtokens-main", Type: SupplyPlatformNvtokens, Enabled: &enabled,
		BaseURL: "https://nvtokens.com", Username: "buyer", Password: "secret", Token: "session",
		Product: "plus", SessionRefreshEnabled: &enabled, ChallengeProvider: SupplyChallengeProviderCapSolver,
		ChallengeAPIBase: "https://api.capsolver.com", ChallengeAPIKey: "solver-secret", RefreshCooldownSeconds: 300,
	}}}
	next := NormalizeSupplyConfig(store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{{
		ID: "nvtokens-main", Type: SupplyPlatformNvtokens, Enabled: &enabled,
		BaseURL: "https://nvtokens.com", Username: "buyer", Product: "plus",
		SessionRefreshEnabled: &enabled, ChallengeProvider: SupplyChallengeProviderCapSolver,
		ChallengeAPIBase: "https://api.capsolver.com", RefreshCooldownSeconds: 120,
	}}}, current)
	platform := next.Platforms[0]
	if platform.ChallengeAPIKey != "solver-secret" || platform.RefreshCooldownSeconds != 120 || !nvtokensSessionRefreshEnabledForTest(platform) {
		t.Fatalf("normalized platform = %#v", platform)
	}
	if err := ValidateSupplyConfig(store.ManagerSupplyConfig{Enabled: &enabled, Platforms: next.Platforms}); err != nil {
		t.Fatalf("validate session refresh config: %v", err)
	}
}

func nvtokensSessionRefreshEnabledForTest(platform store.ManagerSupplyPlatformConfig) bool {
	return platform.SessionRefreshEnabled != nil && *platform.SessionRefreshEnabled
}

func TestValidateSupplyConfigAcceptsNvtokensNativeProducts(t *testing.T) {
	enabled := true
	for _, product := range []string{"plus", "pro", "team", "bugteam", "k12", "grokfree", "grokpro", "free"} {
		cfg := store.ManagerSupplyConfig{Enabled: &enabled, Platforms: []store.ManagerSupplyPlatformConfig{{
			ID: "nvtokens-main", Type: SupplyPlatformNvtokens, Enabled: &enabled,
			BaseURL: "https://nvtokens.com", Username: "buyer", Password: "secret", Product: product,
		}}}
		if err := ValidateSupplyConfig(cfg); err != nil {
			t.Fatalf("native product %s: %v", product, err)
		}
	}
}

func TestNormalizeSupplyConfigDefaultsPreservesAndClearsNvtokensPurchaseFilters(t *testing.T) {
	enabled := true
	basePlatform := store.ManagerSupplyPlatformConfig{
		ID:       "nvtokens-main",
		Type:     SupplyPlatformNvtokens,
		Enabled:  &enabled,
		BaseURL:  "https://nvtokens.com",
		Username: "buyer",
		Password: "secret",
		Product:  "oauth_30d",
	}
	defaulted := NormalizeSupplyConfig(store.ManagerSupplyConfig{
		Platforms: []store.ManagerSupplyPlatformConfig{basePlatform},
	}, store.ManagerSupplyConfig{})
	if got := defaulted.Platforms[0].PurchaseAccountType; got != SupplyPurchaseAccountAll {
		t.Fatalf("default purchase account type = %q, want %q", got, SupplyPurchaseAccountAll)
	}

	maxUnitPriceFen := int64(1350)
	currentPlatform := basePlatform
	currentPlatform.PurchaseAccountType = SupplyPurchaseAccountWithoutRefreshToken
	currentPlatform.MaxUnitPriceFen = &maxUnitPriceFen
	preserved := NormalizeSupplyConfig(store.ManagerSupplyConfig{
		Platforms: []store.ManagerSupplyPlatformConfig{basePlatform},
	}, store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{currentPlatform}})
	if preserved.Platforms[0].PurchaseAccountType != SupplyPurchaseAccountWithoutRefreshToken ||
		preserved.Platforms[0].MaxUnitPriceFen == nil || *preserved.Platforms[0].MaxUnitPriceFen != 1350 {
		t.Fatalf("preserved nvtokens purchase filters = %#v", preserved.Platforms[0])
	}

	zero := int64(0)
	clearPlatform := basePlatform
	clearPlatform.PurchaseAccountType = "invalid-value"
	clearPlatform.MaxUnitPriceFen = &zero
	cleared := NormalizeSupplyConfig(store.ManagerSupplyConfig{
		Platforms: []store.ManagerSupplyPlatformConfig{clearPlatform},
	}, preserved)
	if cleared.Platforms[0].PurchaseAccountType != SupplyPurchaseAccountAll || cleared.Platforms[0].MaxUnitPriceFen != nil {
		t.Fatalf("cleared nvtokens purchase filters = %#v", cleared.Platforms[0])
	}

	invalid := basePlatform
	invalid.PurchaseAccountType = "invalid-value"
	if err := ValidateSupplyConfig(store.ManagerSupplyConfig{Enabled: &enabled, Platforms: []store.ManagerSupplyPlatformConfig{invalid}}); err == nil {
		t.Fatal("invalid nvtokens purchase account type should fail validation")
	}
}

func TestNormalizeSupplyConfigDefaultsRecoveryControls(t *testing.T) {
	next := NormalizeSupplyConfig(store.ManagerSupplyConfig{}, store.ManagerSupplyConfig{})
	if next.RecoverySyncEnabled == nil || !*next.RecoverySyncEnabled {
		t.Fatalf("recovery sync should default enabled: %#v", next.RecoverySyncEnabled)
	}
	if next.RecoveryAutoClaim == nil || !*next.RecoveryAutoClaim {
		t.Fatalf("recovery auto claim should default enabled: %#v", next.RecoveryAutoClaim)
	}
	if next.RecoveryDisableOriginal == nil || !*next.RecoveryDisableOriginal {
		t.Fatalf("recovery original disable should default enabled: %#v", next.RecoveryDisableOriginal)
	}
	if next.RecoverySyncIntervalSeconds != 60 || next.RecoveryClaimBatchSize != 20 {
		t.Fatalf("recovery defaults = interval %d batch %d", next.RecoverySyncIntervalSeconds, next.RecoveryClaimBatchSize)
	}
	if next.RevenueMultiplier != 0.06 {
		t.Fatalf("revenue multiplier = %f, want 0.06", next.RevenueMultiplier)
	}
	if next.QuotaEstimationPolicies["team"].Mode != SupplyQuotaEstimationAuto ||
		next.QuotaEstimationPolicies["team"].FallbackM != 60 ||
		next.QuotaEstimationPolicies["plus"].FallbackM != 160 ||
		next.QuotaEstimationPolicies["pro"].FallbackM != 3000 ||
		next.QuotaEstimationPolicies["free"].FallbackM != 10 {
		t.Fatalf("quota estimation defaults = %#v", next.QuotaEstimationPolicies)
	}
	off := false
	next = NormalizeSupplyConfig(store.ManagerSupplyConfig{RecoveryDisableOriginal: &off}, next)
	if next.RecoveryDisableOriginal == nil || *next.RecoveryDisableOriginal {
		t.Fatalf("submitted recovery original disable=false should be preserved: %#v", next.RecoveryDisableOriginal)
	}
}

func TestNormalizeSupplyConfigAcceptsFixedAndFlooredQuotaPolicies(t *testing.T) {
	next := NormalizeSupplyConfig(store.ManagerSupplyConfig{
		QuotaEstimationPolicies: map[string]store.ManagerSupplyQuotaEstimationPolicy{
			" Team ": {Mode: SupplyQuotaEstimationFixed, FallbackM: 700, FixedM: 42},
			"pro":    {Mode: SupplyQuotaEstimationFixed, FallbackM: 3500, FixedM: 4200},
			"free":   {Mode: "invalid", FallbackM: 8, FixedM: 0.1},
		},
	}, store.ManagerSupplyConfig{})
	if got := next.QuotaEstimationPolicies["team"]; got.Mode != SupplyQuotaEstimationFixed || got.FallbackM != 700 || got.FixedM != 42 {
		t.Fatalf("normalized team quota policy = %#v", got)
	}
	if got := next.QuotaEstimationPolicies["pro"]; got.Mode != SupplyQuotaEstimationFixed || got.FallbackM != 3500 || got.FixedM != 4200 {
		t.Fatalf("normalized pro quota policy = %#v", got)
	}
	if got := next.QuotaEstimationPolicies["free"]; got.Mode != SupplyQuotaEstimationAuto || got.FallbackM != 8 || got.FixedM != 0.5 {
		t.Fatalf("normalized free quota policy = %#v", got)
	}
}

func TestNormalizeSupplyConfigBoundsParallelAutomaticOrders(t *testing.T) {
	defaulted := NormalizeSupplyConfig(store.ManagerSupplyConfig{}, store.ManagerSupplyConfig{})
	if defaulted.MaxConcurrentOrders != 3 {
		t.Fatalf("default max concurrent orders = %d, want 3", defaulted.MaxConcurrentOrders)
	}

	configured := NormalizeSupplyConfig(
		store.ManagerSupplyConfig{MaxConcurrentOrders: 3},
		defaulted,
	)
	if configured.MaxConcurrentOrders != 3 {
		t.Fatalf("configured max concurrent orders = %d, want 3", configured.MaxConcurrentOrders)
	}

	capped := NormalizeSupplyConfig(
		store.ManagerSupplyConfig{MaxConcurrentOrders: 99},
		configured,
	)
	if capped.MaxConcurrentOrders != 3 {
		t.Fatalf("capped max concurrent orders = %d, want 3", capped.MaxConcurrentOrders)
	}
}

func TestNormalizeSupplyConfigAcceptsPriorityFirstPlatformSelection(t *testing.T) {
	next := NormalizeSupplyConfig(store.ManagerSupplyConfig{
		PlatformSelectionStrategy: SupplyPlatformSelectionPriorityFirst,
	}, store.ManagerSupplyConfig{})

	if next.PlatformSelectionStrategy != SupplyPlatformSelectionPriorityFirst {
		t.Fatalf("platform selection strategy = %q, want %q", next.PlatformSelectionStrategy, SupplyPlatformSelectionPriorityFirst)
	}
}

func TestNormalizeSupplyConfigAppliesSupplyStrategyPresets(t *testing.T) {
	tests := []struct {
		strategy                               string
		critical, healthy, emergency, ttl      int
		maxRequests, maxUsefulSecondsBefore401 int
	}{
		{SupplyStrategyStrongSupply, 2, 10, 5, 60, 30, 120},
		{SupplyStrategyBalanced, 1, 5, 3, 30, 40, 150},
		{SupplyStrategyCostFirst, 2, 3, 1, 15, 50, 180},
	}
	for _, test := range tests {
		t.Run(test.strategy, func(t *testing.T) {
			next := NormalizeSupplyConfig(store.ManagerSupplyConfig{Strategy: test.strategy}, store.ManagerSupplyConfig{})
			if next.Strategy != test.strategy ||
				next.StartupAvailableAccounts != nil ||
				next.CriticalAvailableAccounts != test.critical ||
				next.HealthyAvailableAccounts != test.healthy ||
				next.DefaultEmergencyMinAccounts != test.emergency ||
				next.VirtualDemandTTLMinutes != test.ttl ||
				next.AccountMaxRequestsBefore401 != test.maxRequests ||
				next.AccountMaxUsefulSeconds401 != test.maxUsefulSecondsBefore401 {
				t.Fatalf("strategy preset = %#v", next)
			}
			if next.EmergencyBypassUsageRate == nil || !*next.EmergencyBypassUsageRate ||
				next.RecoveryTriggerOn401 == nil || !*next.RecoveryTriggerOn401 {
				t.Fatalf("strategy safety switches = %#v/%#v", next.EmergencyBypassUsageRate, next.RecoveryTriggerOn401)
			}
		})
	}

	defaulted := NormalizeSupplyConfig(store.ManagerSupplyConfig{}, store.ManagerSupplyConfig{})
	if defaulted.Strategy != SupplyStrategyStrongSupply {
		t.Fatalf("default strategy = %q", defaulted.Strategy)
	}
}

func TestNormalizeSupplyConfigPreservesCustomSupplyStrategy(t *testing.T) {
	off := false
	startupAccounts := 8
	next := NormalizeSupplyConfig(store.ManagerSupplyConfig{
		Strategy:                    SupplyStrategyCustom,
		CriticalAvailableAccounts:   4,
		HealthyAvailableAccounts:    3,
		StartupAvailableAccounts:    &startupAccounts,
		DefaultEmergencyMinAccounts: 7,
		VirtualDemandTTLMinutes:     45,
		AccountMaxRequestsBefore401: 35,
		AccountMaxUsefulSeconds401:  135,
		EmergencyBypassUsageRate:    &off,
		RecoveryTriggerOn401:        &off,
	}, store.ManagerSupplyConfig{})
	if next.Strategy != SupplyStrategyCustom || next.CriticalAvailableAccounts != 4 ||
		next.StartupAvailableAccounts == nil || *next.StartupAvailableAccounts != 8 ||
		next.HealthyAvailableAccounts != 4 || next.DefaultEmergencyMinAccounts != 7 ||
		next.VirtualDemandTTLMinutes != 45 || next.AccountMaxRequestsBefore401 != 35 ||
		next.AccountMaxUsefulSeconds401 != 135 {
		t.Fatalf("custom strategy = %#v", next)
	}
	if next.EmergencyBypassUsageRate == nil || *next.EmergencyBypassUsageRate ||
		next.RecoveryTriggerOn401 == nil || *next.RecoveryTriggerOn401 {
		t.Fatalf("custom switches = %#v/%#v", next.EmergencyBypassUsageRate, next.RecoveryTriggerOn401)
	}
}
