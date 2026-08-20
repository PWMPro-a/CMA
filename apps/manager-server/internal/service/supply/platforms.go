package supply

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supplyclient"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

type PlatformOverview struct {
	ID                    string                  `json:"id"`
	Name                  string                  `json:"name,omitempty"`
	Type                  string                  `json:"type"`
	Product               string                  `json:"product"`
	Priority              int                     `json:"priority,omitempty"`
	EmergencyOnly         bool                    `json:"emergencyOnly,omitempty"`
	Selected              bool                    `json:"selected"`
	CheckedAtMS           int64                   `json:"checkedAtMs"`
	Inventory             *supplyclient.Inventory `json:"inventory,omitempty"`
	Balance               *supplyclient.Balance   `json:"balance,omitempty"`
	ExpectedQuotaM        float64                 `json:"expectedQuotaM,omitempty"`
	UsableQuotaM          float64                 `json:"usableQuotaM,omitempty"`
	CostPerUsableQuotaFen float64                 `json:"costPerUsableQuotaFen,omitempty"`
	LastError             string                  `json:"lastError,omitempty"`
}

type PlatformProductCatalog struct {
	PlatformID   string                            `json:"platformId"`
	PlatformName string                            `json:"platformName,omitempty"`
	PlatformType string                            `json:"platformType"`
	CheckedAtMS  int64                             `json:"checkedAtMs"`
	Products     []supplyclient.ProductCatalogItem `json:"products"`
}

type supplyPlatformSelection struct {
	platform store.ManagerSupplyPlatformConfig
	status   PlatformOverview
	all      []PlatformOverview
}

func supplyPlatforms(cfg store.ManagerSupplyConfig) []store.ManagerSupplyPlatformConfig {
	platforms := managerconfigsvc.SupplyPlatforms(cfg)
	result := make([]store.ManagerSupplyPlatformConfig, 0, len(platforms))
	for _, platform := range platforms {
		if !managerconfigsvc.SupplyPlatformEnabled(platform) {
			continue
		}
		result = append(result, platform)
	}
	sort.SliceStable(result, func(i, j int) bool {
		left := result[i].Priority
		right := result[j].Priority
		if left <= 0 {
			left = i + 1
		}
		if right <= 0 {
			right = j + 1
		}
		return left < right
	})
	return result
}

func supplyPlatformCredentials(platform store.ManagerSupplyPlatformConfig) supplyclient.Credentials {
	deliveryMode := "take_json"
	if strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformBugTeam) {
		deliveryMode = "cpa_zip"
	} else if strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformNvtokens) {
		deliveryMode = "nvtokens"
	}
	return supplyclient.Credentials{
		ID:                  strings.TrimSpace(platform.ID),
		PlatformType:        strings.ToLower(strings.TrimSpace(platform.Type)),
		BaseURL:             strings.TrimRight(strings.TrimSpace(platform.BaseURL), "/"),
		Username:            strings.TrimSpace(platform.Username),
		Password:            platform.Password,
		Token:               strings.TrimSpace(platform.Token),
		DeliveryMode:        deliveryMode,
		PurchaseAccountType: strings.ToLower(strings.TrimSpace(platform.PurchaseAccountType)),
		MaxUnitPriceFen:     valueOrZero(platform.MaxUnitPriceFen),
	}
}

func valueOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func lowPriceReservePlatformCeiling(cfg store.ManagerSupplyConfig, platform store.ManagerSupplyPlatformConfig) int64 {
	ceiling := valueOrZero(cfg.LowPriceReserveMaxUnitPriceFen)
	platformCeiling := valueOrZero(platform.MaxUnitPriceFen)
	if ceiling <= 0 || (platformCeiling > 0 && platformCeiling < ceiling) {
		return platformCeiling
	}
	return ceiling
}

func supplyPlatformConfigured(platform store.ManagerSupplyPlatformConfig) bool {
	credentials := supplyPlatformCredentials(platform)
	if credentials.BaseURL == "" || strings.TrimSpace(platform.Product) == "" {
		return false
	}
	return credentials.Token != "" || (credentials.Username != "" && credentials.Password != "")
}

func supplyProductSupportedByPlatform(platform store.ManagerSupplyPlatformConfig, product string) bool {
	product = strings.ToLower(strings.TrimSpace(product))
	switch strings.ToLower(strings.TrimSpace(platform.Type)) {
	case managerconfigsvc.SupplyPlatformBugTeam:
		return product == "team_1h"
	case managerconfigsvc.SupplyPlatformNvtokens:
		switch product {
		case "plus", "pro", "team", "bugteam", "k12", "grokfree", "grokpro", "free":
			return true
		default:
			return false
		}
	default:
		switch product {
		case "oauth_30d", "oauth_7d", "team_1h":
			return true
		default:
			return false
		}
	}
}

func mergeCatalogPlatform(draft store.ManagerSupplyPlatformConfig, saved store.ManagerSupplyConfig) store.ManagerSupplyPlatformConfig {
	for _, current := range managerconfigsvc.SupplyPlatforms(saved) {
		if !strings.EqualFold(strings.TrimSpace(current.ID), strings.TrimSpace(draft.ID)) {
			continue
		}
		if strings.TrimSpace(draft.Type) == "" {
			draft.Type = current.Type
		}
		if strings.TrimSpace(draft.Name) == "" {
			draft.Name = current.Name
		}
		if strings.TrimSpace(draft.BaseURL) == "" {
			draft.BaseURL = current.BaseURL
		}
		if strings.TrimSpace(draft.Username) == "" && !draft.ClearUsername {
			draft.Username = current.Username
		}
		sameIdentity := strings.EqualFold(strings.TrimSpace(draft.Type), strings.TrimSpace(current.Type)) &&
			strings.EqualFold(strings.TrimRight(strings.TrimSpace(draft.BaseURL), "/"), strings.TrimRight(strings.TrimSpace(current.BaseURL), "/")) &&
			strings.EqualFold(strings.TrimSpace(draft.Username), strings.TrimSpace(current.Username))
		if sameIdentity && strings.TrimSpace(draft.Password) == "" {
			draft.Password = current.Password
		}
		if sameIdentity && strings.TrimSpace(draft.Token) == "" {
			draft.Token = current.Token
		}
		if strings.TrimSpace(draft.Product) == "" {
			draft.Product = current.Product
		}
		break
	}
	return draft
}

func staticSupplyProductCatalog(platform store.ManagerSupplyPlatformConfig) []supplyclient.ProductCatalogItem {
	if strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformBugTeam) {
		return []supplyclient.ProductCatalogItem{{Code: "team_1h", Label: "Team 1h"}}
	}
	return []supplyclient.ProductCatalogItem{
		{Code: "oauth_30d", Label: "OAuth 30d"},
		{Code: "oauth_7d", Label: "OAuth 7d"},
		{Code: "team_1h", Label: "Team 1h"},
	}
}

func (s *Service) GetPlatformProductCatalog(
	ctx context.Context,
	draft store.ManagerSupplyPlatformConfig,
) (PlatformProductCatalog, error) {
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return PlatformProductCatalog{}, err
	}
	platform := mergeCatalogPlatform(draft, cfg.Supply)
	if strings.TrimSpace(platform.ID) == "" {
		platform.ID = strings.ToLower(strings.TrimSpace(platform.Type))
	}
	result := PlatformProductCatalog{
		PlatformID:   platform.ID,
		PlatformName: platform.Name,
		PlatformType: platform.Type,
		CheckedAtMS:  time.Now().UnixMilli(),
	}
	if !strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformNvtokens) {
		result.Products = staticSupplyProductCatalog(platform)
		return result, nil
	}
	if !supplyPlatformConfigured(platform) {
		return result, ErrNotConfigured
	}
	catalog, err := s.supplyClient.ProductCatalog(ctx, supplyPlatformCredentials(platform))
	if err != nil {
		return result, err
	}
	result.Products = catalog.Products
	return result, nil
}

func (s *Service) QuotePlatformProduct(
	ctx context.Context,
	quantity int,
	supplierID string,
	product string,
) (PlatformOverview, error) {
	if quantity <= 0 || quantity > 10000 {
		return PlatformOverview{}, ErrInvalidQuantity
	}
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return PlatformOverview{}, err
	}
	platform, err := resolveSupplyPlatform(cfg.Supply, supplierID, "")
	if err != nil {
		return PlatformOverview{}, err
	}
	product = strings.ToLower(strings.TrimSpace(product))
	if product == "" {
		product = platform.Product
	}
	if !supplyProductSupportedByPlatform(platform, product) {
		return PlatformOverview{}, fmt.Errorf("product %s is not supported by supply platform %s", product, platform.ID)
	}
	platform.Product = product
	if !supplyPlatformConfigured(platform) {
		return PlatformOverview{}, ErrNotConfigured
	}
	credentials := supplyPlatformCredentials(platform)
	inventory, err := s.supplyClient.Inventory(ctx, credentials, product, quantity)
	if err != nil {
		return PlatformOverview{}, err
	}
	balance, err := s.supplyClient.Balance(ctx, credentials)
	if err != nil {
		return PlatformOverview{}, err
	}
	status := PlatformOverview{
		ID:            platform.ID,
		Name:          platform.Name,
		Type:          platform.Type,
		Product:       product,
		Priority:      platform.Priority,
		EmergencyOnly: platform.EmergencyOnly,
		Selected:      true,
		CheckedAtMS:   time.Now().UnixMilli(),
		Inventory:     &inventory,
		Balance:       &balance,
	}
	applySupplyPlatformEconomics(&status, cfg.Supply, s.currentSmartResource(cfg.Supply), platform, quantity)
	return status, nil
}

func resolveSupplyPlatform(cfg store.ManagerSupplyConfig, supplierID string, product string) (store.ManagerSupplyPlatformConfig, error) {
	platforms := supplyPlatforms(cfg)
	supplierID = strings.TrimSpace(supplierID)
	if supplierID != "" {
		for _, platform := range platforms {
			if strings.EqualFold(strings.TrimSpace(platform.ID), supplierID) {
				return platform, nil
			}
		}
		return store.ManagerSupplyPlatformConfig{}, fmt.Errorf("%w: supply platform %s is not enabled or configured", ErrNotConfigured, supplierID)
	}
	product = strings.TrimSpace(product)
	for _, platform := range platforms {
		if strings.EqualFold(strings.TrimSpace(platform.Product), product) {
			return platform, nil
		}
	}
	if product != "" {
		return store.ManagerSupplyPlatformConfig{}, fmt.Errorf("%w: no enabled supply platform is configured for product %s", ErrNotConfigured, product)
	}
	if len(platforms) > 0 {
		return platforms[0], nil
	}
	return store.ManagerSupplyPlatformConfig{}, ErrNotConfigured
}

func recoverySupplyPlatforms(cfg store.ManagerSupplyConfig) []store.ManagerSupplyPlatformConfig {
	platforms := supplyPlatforms(cfg)
	result := make([]store.ManagerSupplyPlatformConfig, 0, len(platforms))
	for _, platform := range platforms {
		if !supplyPlatformConfigured(platform) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformNvtokens) {
			// nvtokens delivers fresh extraction bundles but has no compatible
			// customer recovery/claim contract.
			continue
		}
		if strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformBugTeam) &&
			strings.TrimSpace(platform.Token) == "" {
			// BugTeam deliberately requires a customer API token for recovery
			// listing and one-time ticket claims; browser/password sessions only
			// cover inventory, ordering and downloads.
			continue
		}
		result = append(result, platform)
	}
	return result
}

func recoverySupplyPlatform(cfg store.ManagerSupplyConfig, supplierID ...string) (store.ManagerSupplyPlatformConfig, error) {
	platforms := recoverySupplyPlatforms(cfg)
	requestedID := ""
	if len(supplierID) > 0 {
		requestedID = strings.TrimSpace(supplierID[0])
	}
	if requestedID != "" {
		for _, platform := range platforms {
			if strings.EqualFold(strings.TrimSpace(platform.ID), requestedID) {
				return platform, nil
			}
		}
		return store.ManagerSupplyPlatformConfig{}, fmt.Errorf("recovery supply platform %s is not configured with an API token", requestedID)
	}
	for _, platform := range platforms {
		if strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformLegacy) {
			return platform, nil
		}
	}
	if len(platforms) > 0 {
		return platforms[0], nil
	}
	return store.ManagerSupplyPlatformConfig{}, errors.New("no recovery-capable supply platform is configured")
}

func recoverySupplyPlatformConfigured(cfg store.ManagerSupplyConfig) bool {
	return len(recoverySupplyPlatforms(cfg)) > 0
}

func supplyProductConfigured(cfg store.ManagerSupplyConfig, product string) bool {
	product = strings.TrimSpace(product)
	if product == "" {
		return true
	}
	if strings.TrimSpace(cfg.Product) != "" && sameSupplyProduct(cfg.Product, product) {
		return true
	}
	for _, platform := range supplyPlatforms(cfg) {
		if sameSupplyProduct(platform.Product, product) {
			return true
		}
	}
	return false
}

func (s *Service) selectSupplyPlatform(
	ctx context.Context,
	cfg store.ManagerSupplyConfig,
	quantity int,
	openOrders []store.SupplyOrder,
	requestedSupplierID ...string,
) (supplyPlatformSelection, error) {
	requestedID := ""
	if len(requestedSupplierID) > 0 {
		requestedID = strings.TrimSpace(requestedSupplierID[0])
	}
	return s.selectSupplyPlatformProduct(ctx, cfg, quantity, openOrders, requestedID, "")
}

// selectLowPriceReservePlatform reuses the regular multi-platform quote pass,
// then admits only immediately available inventory whose quoted unit price is
// known and does not exceed the low-price reserve ceiling. Emergency-only
// suppliers remain reserved for the existing emergency path.
func (s *Service) selectLowPriceReservePlatform(
	ctx context.Context,
	cfg store.ManagerSupplyConfig,
	quantity int,
	openOrders []store.SupplyOrder,
	requestedProduct ...string,
) (supplyPlatformSelection, bool, error) {
	product := ""
	if len(requestedProduct) > 0 {
		product = strings.TrimSpace(requestedProduct[0])
	}
	ceiling := valueOrZero(cfg.LowPriceReserveMaxUnitPriceFen)
	if ceiling <= 0 {
		return supplyPlatformSelection{}, false, nil
	}
	quoteCfg := cfg
	nvtokensPlatforms := make([]store.ManagerSupplyPlatformConfig, 0, len(supplyPlatforms(cfg)))
	for _, platform := range supplyPlatforms(cfg) {
		if strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformNvtokens) {
			platformCeiling := lowPriceReservePlatformCeiling(cfg, platform)
			platform.MaxUnitPriceFen = &platformCeiling
			nvtokensPlatforms = append(nvtokensPlatforms, platform)
		}
	}
	if len(nvtokensPlatforms) == 0 {
		return supplyPlatformSelection{}, false, nil
	}
	quoteCfg.Platforms = nvtokensPlatforms
	// A normal priority-first strategy is useful for quality-sensitive routine
	// procurement. The explicit purpose of this path is cost capture, so rank
	// all price-qualified suppliers by effective cost after deliverability.
	quoteCfg.PlatformSelectionStrategy = managerconfigsvc.SupplyPlatformSelectionBestAvailable
	quoted, quoteErr := s.selectSupplyPlatformProduct(ctx, quoteCfg, quantity, openOrders, "", product)
	if quoteErr != nil && len(quoted.all) == 0 {
		return supplyPlatformSelection{}, false, quoteErr
	}
	platforms := nvtokensPlatforms
	platformByID := make(map[string]store.ManagerSupplyPlatformConfig, len(platforms))
	for _, platform := range platforms {
		platformByID[strings.ToLower(strings.TrimSpace(platform.ID))] = platform
	}
	used := make(map[string]struct{}, len(openOrders))
	for _, order := range openOrders {
		if id := strings.ToLower(strings.TrimSpace(order.SupplierID)); id != "" {
			used[id] = struct{}{}
		}
	}
	candidates := make([]int, 0, len(quoted.all))
	for index, status := range quoted.all {
		if status.Inventory == nil || status.Balance == nil || status.EmergencyOnly {
			continue
		}
		unitPrice := status.Inventory.EstimatedUnitPriceFen
		if unitPrice <= 0 || unitPrice > ceiling || status.Inventory.Available <= 0 {
			continue
		}
		if supplyPlatformAvailabilityTier(status, quantity, cfg.MinBalanceReserveFen) > 1 {
			continue
		}
		if _, found := platformByID[strings.ToLower(strings.TrimSpace(status.ID))]; !found {
			continue
		}
		candidates = append(candidates, index)
	}
	if len(candidates) == 0 {
		return supplyPlatformSelection{all: quoted.all}, false, nil
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return supplyPlatformLessWithPriority(
			quoted.all[candidates[i]],
			quoted.all[candidates[j]],
			quantity,
			cfg.MinBalanceReserveFen,
			used,
			false,
			false,
		)
	})
	selectedIndex := candidates[0]
	for index := range quoted.all {
		quoted.all[index].Selected = index == selectedIndex
	}
	status := quoted.all[selectedIndex]
	platform := platformByID[strings.ToLower(strings.TrimSpace(status.ID))]
	return supplyPlatformSelection{platform: platform, status: status, all: quoted.all}, true, nil
}

func (s *Service) selectSupplyPlatformProduct(
	ctx context.Context,
	cfg store.ManagerSupplyConfig,
	quantity int,
	openOrders []store.SupplyOrder,
	requestedID string,
	requestedProduct string,
) (supplyPlatformSelection, error) {
	platforms := supplyPlatforms(cfg)
	if len(platforms) == 0 {
		return supplyPlatformSelection{}, ErrNotConfigured
	}
	requestedID = strings.TrimSpace(requestedID)
	requestedProduct = strings.ToLower(strings.TrimSpace(requestedProduct))
	requestedIndex := -1
	if requestedID != "" {
		for index := range platforms {
			if strings.EqualFold(strings.TrimSpace(platforms[index].ID), requestedID) {
				requestedIndex = index
				break
			}
		}
		if requestedIndex < 0 {
			return supplyPlatformSelection{}, fmt.Errorf("%w: supply platform %s is not enabled or configured", ErrNotConfigured, requestedID)
		}
		if requestedProduct != "" {
			if !supplyProductSupportedByPlatform(platforms[requestedIndex], requestedProduct) {
				return supplyPlatformSelection{}, fmt.Errorf("product %s is not supported by supply platform %s", requestedProduct, requestedID)
			}
			platforms[requestedIndex].Product = requestedProduct
		}
	}
	statuses := make([]PlatformOverview, len(platforms))
	quoteErrors := make([]error, len(platforms))
	resource := s.currentSmartResource(cfg)
	type result struct {
		index     int
		inventory supplyclient.Inventory
		balance   supplyclient.Balance
		err       error
	}
	results := make(chan result, len(platforms))
	var wait sync.WaitGroup
	for index, platform := range platforms {
		wait.Add(1)
		go func(index int, platform store.ManagerSupplyPlatformConfig) {
			defer wait.Done()
			if !supplyPlatformConfigured(platform) {
				results <- result{index: index, err: ErrNotConfigured}
				return
			}
			credentials := supplyPlatformCredentials(platform)
			inventory, err := s.supplyClient.Inventory(ctx, credentials, platform.Product, quantity)
			if err != nil {
				results <- result{index: index, err: err}
				return
			}
			balance, err := s.supplyClient.Balance(ctx, credentials)
			results <- result{index: index, inventory: inventory, balance: balance, err: err}
		}(index, platform)
	}
	wait.Wait()
	close(results)
	checkedAtMS := time.Now().UnixMilli()
	platformErrors := make([]error, 0, len(platforms))
	for item := range results {
		platform := platforms[item.index]
		status := PlatformOverview{
			ID:            platform.ID,
			Name:          platform.Name,
			Type:          platform.Type,
			Product:       platform.Product,
			Priority:      platform.Priority,
			EmergencyOnly: platform.EmergencyOnly,
			CheckedAtMS:   checkedAtMS,
		}
		if item.err != nil {
			quoteErrors[item.index] = item.err
			status.LastError = safeError(item.err)
			platformErrors = append(platformErrors, fmt.Errorf("%s: %w", firstNonEmptyString(platform.Name, platform.ID), item.err))
		} else {
			if strings.TrimSpace(item.inventory.Product) == "" {
				item.inventory.Product = platform.Product
			}
			status.Inventory = &item.inventory
			status.Balance = &item.balance
			applySupplyPlatformEconomics(&status, cfg, resource, platform, quantity)
		}
		statuses[item.index] = status
	}

	if requestedID != "" {
		selectedIndex := requestedIndex
		if quoteErrors[selectedIndex] != nil {
			return supplyPlatformSelection{all: statuses}, fmt.Errorf(
				"supply platform %s quote failed: %w",
				firstNonEmptyString(platforms[selectedIndex].Name, platforms[selectedIndex].ID),
				quoteErrors[selectedIndex],
			)
		}
		if statuses[selectedIndex].Inventory == nil || statuses[selectedIndex].Balance == nil {
			return supplyPlatformSelection{all: statuses}, fmt.Errorf("supply platform %s quote is incomplete", firstNonEmptyString(platforms[selectedIndex].Name, platforms[selectedIndex].ID))
		}
		statuses[selectedIndex].Selected = true
		return supplyPlatformSelection{
			platform: platforms[selectedIndex],
			status:   statuses[selectedIndex],
			all:      statuses,
		}, nil
	}

	used := make(map[string]struct{}, len(openOrders))
	for _, order := range openOrders {
		if strings.TrimSpace(order.SupplierID) != "" {
			used[strings.ToLower(strings.TrimSpace(order.SupplierID))] = struct{}{}
		}
	}
	candidates := make([]int, 0, len(platforms))
	emergency := smartResourceEmergency(resource)
	for index := range statuses {
		if statuses[index].Inventory != nil && statuses[index].Balance != nil &&
			(!platforms[index].EmergencyOnly || emergency) {
			candidates = append(candidates, index)
		}
	}
	if len(candidates) == 0 {
		if len(platformErrors) > 0 {
			return supplyPlatformSelection{all: statuses}, errors.Join(platformErrors...)
		}
		return supplyPlatformSelection{all: statuses}, ErrNotConfigured
	}
	priorityFirst := strings.EqualFold(
		strings.TrimSpace(cfg.PlatformSelectionStrategy),
		managerconfigsvc.SupplyPlatformSelectionPriorityFirst,
	)
	sort.SliceStable(candidates, func(i, j int) bool {
		leftIndex := candidates[i]
		rightIndex := candidates[j]
		left := statuses[leftIndex]
		right := statuses[rightIndex]
		return supplyPlatformLessWithPriority(left, right, quantity, cfg.MinBalanceReserveFen, used, emergency, priorityFirst)
	})
	selectedIndex := candidates[0]
	statuses[selectedIndex].Selected = true
	return supplyPlatformSelection{
		platform: platforms[selectedIndex],
		status:   statuses[selectedIndex],
		all:      statuses,
	}, nil
}

func supplyPlatformLess(left PlatformOverview, right PlatformOverview, quantity int, balanceReserveFen int64, used map[string]struct{}, emergency bool) bool {
	return supplyPlatformLessWithPriority(left, right, quantity, balanceReserveFen, used, emergency, false)
}

func supplyPlatformLessWithPriority(left PlatformOverview, right PlatformOverview, quantity int, balanceReserveFen int64, used map[string]struct{}, emergency bool, priorityFirst bool) bool {
	leftTier := supplyPlatformAvailabilityTier(left, quantity, balanceReserveFen)
	rightTier := supplyPlatformAvailabilityTier(right, quantity, balanceReserveFen)
	if leftTier != rightTier {
		return leftTier < rightTier
	}
	if priorityFirst && left.Priority != right.Priority {
		leftPriority := left.Priority
		rightPriority := right.Priority
		if leftPriority <= 0 {
			leftPriority = math.MaxInt
		}
		if rightPriority <= 0 {
			rightPriority = math.MaxInt
		}
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
	}
	leftCost := left.CostPerUsableQuotaFen
	if leftCost <= 0 {
		leftCost = math.MaxFloat64
	}
	rightCost := right.CostPerUsableQuotaFen
	if rightCost <= 0 {
		rightCost = math.MaxFloat64
	}
	if leftCost != rightCost {
		return leftCost < rightCost
	}
	if left.UsableQuotaM != right.UsableQuotaM {
		return left.UsableQuotaM > right.UsableQuotaM
	}
	leftPrice := int64(math.MaxInt64)
	if left.Inventory != nil && left.Inventory.EstimatedUnitPriceFen > 0 {
		leftPrice = left.Inventory.EstimatedUnitPriceFen
	}
	rightPrice := int64(math.MaxInt64)
	if right.Inventory != nil && right.Inventory.EstimatedUnitPriceFen > 0 {
		rightPrice = right.Inventory.EstimatedUnitPriceFen
	}
	if leftPrice != rightPrice {
		return leftPrice < rightPrice
	}
	leftLifetime := int64(0)
	if left.Inventory != nil {
		leftLifetime = left.Inventory.MaximumRemainingSeconds
	}
	rightLifetime := int64(0)
	if right.Inventory != nil {
		rightLifetime = right.Inventory.MaximumRemainingSeconds
	}
	if emergency && leftLifetime != rightLifetime {
		return leftLifetime > rightLifetime
	}
	leftUsed := 0
	if _, ok := used[strings.ToLower(strings.TrimSpace(left.ID))]; ok {
		leftUsed = 1
	}
	rightUsed := 0
	if _, ok := used[strings.ToLower(strings.TrimSpace(right.ID))]; ok {
		rightUsed = 1
	}
	if leftUsed != rightUsed {
		// Normal procurement spreads expiry and supplier risk after comparing
		// effective cost. Emergency procurement still gets the same protection,
		// but only after immediate deliverability and usable capacity are equal.
		return leftUsed < rightUsed
	}
	if !emergency && leftLifetime != rightLifetime {
		return leftLifetime > rightLifetime
	}
	return left.Priority < right.Priority
}

func applySupplyPlatformEconomics(
	status *PlatformOverview,
	cfg store.ManagerSupplyConfig,
	resource SmartResource,
	platform store.ManagerSupplyPlatformConfig,
	quantity int,
) {
	if status == nil || status.Inventory == nil {
		return
	}
	expectedQuotaM := supplyPlatformExpectedQuotaM(cfg, resource, platform)
	usableQuotaM := expectedQuotaM
	remainingSeconds := status.Inventory.MaximumRemainingSeconds
	demandMPerMinute := math.Max(resource.ConsumeTokenMPerMinute, resource.DemandPlanningTokenMPerMinute)
	if demandMPerMinute > 0 && remainingSeconds > 0 {
		lifetimeDemandM := demandMPerMinute * (float64(remainingSeconds) / 60)
		if quantity > 1 {
			lifetimeDemandM /= float64(quantity)
		}
		if usableQuotaM <= 0 || lifetimeDemandM < usableQuotaM {
			usableQuotaM = lifetimeDemandM
		}
	}
	status.ExpectedQuotaM = round2(math.Max(expectedQuotaM, 0))
	status.UsableQuotaM = round2(math.Max(usableQuotaM, 0))
	if status.UsableQuotaM > 0 && status.Inventory.EstimatedUnitPriceFen > 0 {
		status.CostPerUsableQuotaFen = math.Round((float64(status.Inventory.EstimatedUnitPriceFen)/status.UsableQuotaM)*100) / 100
	}
}

func supplyPlatformExpectedQuotaM(
	cfg store.ManagerSupplyConfig,
	resource SmartResource,
	platform store.ManagerSupplyPlatformConfig,
) float64 {
	supplierID := normalizeSmartQuotaSupplierID(platform.ID)
	planType := "team"
	for _, estimate := range resource.AccountQuotaPlanEstimates {
		if normalizeSmartQuotaSupplierID(estimate.SupplierID) == supplierID &&
			strings.EqualFold(strings.TrimSpace(estimate.PlanType), planType) && estimate.AdoptedM > 0 {
			return estimate.AdoptedM
		}
	}
	policy := smartQuotaPolicyForSupplier(cfg, platform.ID, planType)
	if strings.EqualFold(policy.Mode, smartQuotaPolicyModeFixed) && policy.FixedM > 0 {
		return policy.FixedM
	}
	if policy.FallbackM > 0 {
		return policy.FallbackM
	}
	return smartQuotaFallbackForPlan(planType)
}

func supplyPlatformAvailabilityTier(status PlatformOverview, quantity int, balanceReserveFen int64) int {
	if status.Inventory == nil || status.Balance == nil {
		return 9
	}
	requiredBalanceFen := status.Inventory.EstimatedTotalFen
	if balanceReserveFen > 0 {
		requiredBalanceFen += balanceReserveFen
	}
	if status.Inventory.EstimatedTotalFen > 0 && status.Balance.AvailableFen < requiredBalanceFen {
		return 8
	}
	if status.Inventory.Available >= max(1, quantity) {
		return 0
	}
	if status.Inventory.Available > 0 {
		return 1
	}
	if status.Inventory.NeedsProduction {
		return 2
	}
	return 3
}
