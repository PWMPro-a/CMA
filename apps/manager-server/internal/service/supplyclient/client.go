package supplyclient

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeout             = 30 * time.Second
	defaultTakeTimeout         = 3 * time.Minute
	maxResponseBodyBytes       = 16 * 1024 * 1024
	maxDownloadBodyBytes       = 64 * 1024 * 1024
	nvtokensEstimateRetryDelay = 150 * time.Millisecond
	customerTokenHeader        = "X-Customer-Token"
	customerSessionHeader      = "X-Customer-Session"
	nvtokensPlatform           = "nvtokens"
	nvtokensSessionCookie      = "scm_session"
	nvtokensLegacyCookie       = "session"
	nvtokensBatchTimeout       = 10 * time.Minute
)

var ErrNvtokensSessionRefreshUnavailable = errors.New("nvtokens automatic session refresh is unavailable")
var ErrNvtokensEstimateUnavailable = errors.New("nvtokens estimate is temporarily unavailable")

type NvtokensSessionRefresher func(ctx context.Context, credentials Credentials) (string, error)

type NvtokensChallengeConfig struct {
	Provider                  string
	SiteKey                   string
	TestBypass                bool
	EmailVerificationRequired bool
}

type HTTPError struct {
	StatusCode        int
	Message           string
	Code              string
	RetryAfterSeconds int
}

func (e *HTTPError) Error() string {
	detail := strings.TrimSpace(e.Message)
	if strings.TrimSpace(e.Code) != "" && !strings.Contains(detail, e.Code) {
		if detail == "" {
			detail = e.Code
		} else {
			detail = e.Code + ": " + detail
		}
	}
	if detail == "" {
		return fmt.Sprintf("supply API returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("supply API returned HTTP %d: %s", e.StatusCode, detail)
}

type Credentials struct {
	ID                  string
	PlatformType        string
	BaseURL             string
	Username            string
	Password            string
	Token               string
	DeliveryMode        string
	PurchaseAccountType string
	MaxUnitPriceFen     int64
	PreferredSellers    []string
	SellerWhitelist     []string
	SellerBlacklist     []string
	PreferredChannelIDs []string
}

type Inventory struct {
	Product                 string `json:"product"`
	RequestedQuantity       int    `json:"requestedQuantity"`
	Available               int    `json:"available"`
	Missing                 int    `json:"missing"`
	NeedsProduction         bool   `json:"needsProduction"`
	EstimatedTotalFen       int64  `json:"estimatedTotalFen"`
	EstimatedUnitPriceFen   int64  `json:"estimatedUnitPriceFen"`
	MinimumRemainingSeconds int64  `json:"minimumRemainingSeconds"`
	MaximumRemainingSeconds int64  `json:"maximumRemainingSeconds"`
}

type Balance struct {
	BalanceFen   int64  `json:"balanceFen"`
	HeldFen      int64  `json:"heldFen"`
	AvailableFen int64  `json:"availableFen"`
	Currency     string `json:"currency"`
}

// ProductCatalog is the supplier-native product list used by the management
// UI and manual procurement flow. Product codes are intentionally not mapped
// to CPAM's legacy oauth_* aliases: callers must persist and submit the exact
// code accepted by the selected supplier.
type ProductCatalog struct {
	Products []ProductCatalogItem `json:"products"`
}

type ProductCatalogItem struct {
	Code            string `json:"code"`
	Label           string `json:"label"`
	Available       int    `json:"available"`
	MinUnitPriceFen int64  `json:"minUnitPriceFen,omitempty"`
	MaxUnitPriceFen int64  `json:"maxUnitPriceFen,omitempty"`
}

// MarketplaceSellerCandidate is one concrete seller/quality-channel choice
// returned by a marketplace platform. SelectionToken is the exact value sent
// back in seller_whitelist; SellerID is the stable cross-channel identity used
// for local quota scoring.
type MarketplaceSellerCandidate struct {
	SellerID          string   `json:"sellerId"`
	Name              string   `json:"name"`
	SelectionToken    string   `json:"selectionToken"`
	ChannelID         string   `json:"channelId,omitempty"`
	Product           string   `json:"product"`
	Available         int      `json:"available"`
	MinUnitPriceFen   int64    `json:"minUnitPriceFen,omitempty"`
	MaxUnitPriceFen   int64    `json:"maxUnitPriceFen,omitempty"`
	PurchasedBefore   bool     `json:"purchasedBefore,omitempty"`
	PurchaseCount     int      `json:"purchaseCount,omitempty"`
	QualityScore      *float64 `json:"qualityScore,omitempty"`
	ActiveRatePercent *float64 `json:"activeRatePercent,omitempty"`
}

type Order struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	Product           string `json:"product"`
	Quantity          int    `json:"quantity"`
	ReadyQuantity     int    `json:"readyQuantity"`
	Progress          int    `json:"progress"`
	ChargedFen        int64  `json:"chargedFen"`
	ReleasedFen       int64  `json:"releasedFen"`
	RetryAfterSeconds int    `json:"retryAfterSeconds"`
	StatusURL         string `json:"statusUrl,omitempty"`
	TakeURL           string `json:"takeUrl,omitempty"`
}

type TakeResult struct {
	Order                Order
	Accounts             []json.RawMessage
	OrderItems           []OrderItem
	ItemRemainingSeconds []int64
	ReplacementFiles     []ReplacementFile
	Pending              bool
}

type OrderItem struct {
	RemainingSeconds int64
	HasRemaining     bool
	BasePriceFen     int64
	ChargedFen       int64
}

type Recovery struct {
	ID                string          `json:"id"`
	DeliveryStatus    string          `json:"deliveryStatus"`
	Product           string          `json:"product,omitempty"`
	SourceOrderID     string          `json:"sourceOrderId,omitempty"`
	OriginalEmail     string          `json:"originalEmail,omitempty"`
	OriginalAccount   string          `json:"originalAccount,omitempty"`
	OriginalAuthIndex string          `json:"originalAuthIndex,omitempty"`
	ClaimURL          string          `json:"claimUrl,omitempty"`
	ClaimTicket       string          `json:"-"`
	StatusURL         string          `json:"statusUrl,omitempty"`
	CredentialVersion int             `json:"credentialVersion,omitempty"`
	RefundedFen       int64           `json:"refundedFen,omitempty"`
	Raw               json.RawMessage `json:"-"`
}

type RecoveryClaimResult struct {
	Recovery          Recovery
	Accounts          []json.RawMessage
	CredentialVersion int
}

type ReplacementFile struct {
	RecoveryID        string
	Ready             bool
	StatusURL         string
	ClaimURL          string
	ClaimTicket       string
	CredentialVersion int
	Product           string
	SourceOrderID     string
	OriginalEmail     string
	OriginalAccount   string
	OriginalAuthIndex string
	Raw               json.RawMessage
}

type RecoveryPage struct {
	Recoveries   []Recovery
	NextBeforeID string
}

type tokenState struct {
	key        string
	token      string
	header     string
	cookie     string
	cookieAuth bool
	expiresAt  time.Time
}

type Client struct {
	httpClient      *http.Client
	timeout         time.Duration
	takeTimeout     time.Duration
	mu              sync.Mutex
	loginMu         sync.Mutex
	token           tokenState
	tokens          map[string]tokenState
	nvtokensResults map[string]TakeResult
	refresherMu     sync.RWMutex
	refresher       NvtokensSessionRefresher
}

func New(httpClient *http.Client, timeout ...time.Duration) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	// nvtokens authenticates with the HttpOnly session cookie set by /api/login.
	// Keep a private client copy and cookie jar so the supplier session does not
	// leak into the other services that share the application's HTTP client.
	clientCopy := *httpClient
	if clientCopy.Jar == nil {
		if jar, err := cookiejar.New(nil); err == nil {
			clientCopy.Jar = jar
		}
	}
	httpClient = &clientCopy
	requestTimeout := defaultTimeout
	if len(timeout) > 0 && timeout[0] > 0 {
		requestTimeout = timeout[0]
	}
	takeTimeout := defaultTakeTimeout
	if requestTimeout > takeTimeout {
		takeTimeout = requestTimeout
	}
	return &Client{
		httpClient:      httpClient,
		timeout:         requestTimeout,
		takeTimeout:     takeTimeout,
		tokens:          make(map[string]tokenState),
		nvtokensResults: make(map[string]TakeResult),
	}
}

func (c *Client) SetNvtokensSessionRefresher(refresher NvtokensSessionRefresher) {
	if c == nil {
		return
	}
	c.refresherMu.Lock()
	c.refresher = refresher
	c.refresherMu.Unlock()
}

// DefaultTakeTimeout is intentionally longer than the normal API timeout.
// Taking an order may require the supplier to prepare and serialize multiple
// OAuth account files; inventory and status operations must stay short.
func DefaultTakeTimeout() time.Duration { return defaultTakeTimeout }

func (c *Client) Inventory(ctx context.Context, credentials Credentials, product string, quantity int) (Inventory, error) {
	if isNvtokens(credentials) {
		return c.nvtokensInventory(ctx, credentials, product, quantity)
	}
	query := url.Values{}
	query.Set("product", strings.TrimSpace(product))
	query.Set("quantity", strconv.Itoa(quantity))
	value, _, err := c.doAuthenticated(ctx, credentials, http.MethodGet, "/api/customer/inventory?"+query.Encode(), nil)
	if err != nil {
		return Inventory{}, err
	}
	root := primaryObject(value)
	return Inventory{
		Product:                 stringValue(root, "product"),
		RequestedQuantity:       intValue(root, "quantity", "requested_quantity"),
		Available:               intValue(root, "available"),
		Missing:                 intValue(root, "missing"),
		NeedsProduction:         boolValue(root, "needs_production", "needsProduction"),
		EstimatedTotalFen:       int64Value(root, "estimated_total_fen", "estimatedTotalFen"),
		EstimatedUnitPriceFen:   int64Value(root, "estimated_unit_price_fen", "estimatedUnitPriceFen"),
		MinimumRemainingSeconds: int64Value(root, "minimum_remaining_seconds", "minimumRemainingSeconds"),
		MaximumRemainingSeconds: int64Value(root, "maximum_remaining_seconds", "maximumRemainingSeconds"),
	}, nil
}

func (c *Client) Balance(ctx context.Context, credentials Credentials) (Balance, error) {
	if isNvtokens(credentials) {
		return c.nvtokensBalance(ctx, credentials)
	}
	value, _, err := c.doAuthenticated(ctx, credentials, http.MethodGet, "/api/customer/balance", nil)
	if err != nil {
		return Balance{}, err
	}
	root := primaryObject(value)
	return Balance{
		BalanceFen:   int64Value(root, "balance_fen", "balanceFen"),
		HeldFen:      int64Value(root, "held_fen", "heldFen"),
		AvailableFen: int64Value(root, "available_fen", "availableFen"),
		Currency:     stringValue(root, "currency"),
	}, nil
}

func (c *Client) ProductCatalog(ctx context.Context, credentials Credentials) (ProductCatalog, error) {
	if isNvtokens(credentials) {
		return c.nvtokensProductCatalog(ctx, credentials)
	}
	return ProductCatalog{}, errors.New("supply platform does not expose a dynamic product catalog")
}

func (c *Client) MarketplaceSellerCandidates(
	ctx context.Context,
	credentials Credentials,
	product string,
) ([]MarketplaceSellerCandidate, error) {
	if !isNvtokens(credentials) {
		return nil, errors.New("supply platform does not expose marketplace sellers")
	}
	return c.nvtokensMarketplaceSellerCandidates(ctx, credentials, product)
}

func (c *Client) CreateOrder(ctx context.Context, credentials Credentials, product string, quantity int, idempotencyKey ...string) (Order, error) {
	if isNvtokens(credentials) {
		return c.nvtokensCreateOrder(ctx, credentials, product, quantity, idempotencyKey...)
	}
	payload := map[string]any{"product": strings.TrimSpace(product), "quantity": quantity}
	headers := make(http.Header)
	if len(idempotencyKey) > 0 && strings.TrimSpace(idempotencyKey[0]) != "" {
		headers.Set("Idempotency-Key", strings.TrimSpace(idempotencyKey[0]))
	}
	value, _, err := c.doAuthenticatedWithHeaders(ctx, credentials, http.MethodPost, "/api/customer/pickup/orders", payload, headers, c.timeout)
	if err != nil {
		return Order{}, err
	}
	order := parseOrderValue(value)
	if order.ID == "" {
		return Order{}, errors.New("supply create order response did not include order.id")
	}
	return order, nil
}

func (c *Client) GetOrder(ctx context.Context, credentials Credentials, orderID string, statusURL ...string) (Order, error) {
	if isNvtokens(credentials) {
		return c.nvtokensGetOrder(ctx, credentials, orderID, statusURL...)
	}
	endpoint := "/api/customer/pickup/orders/" + url.PathEscape(strings.TrimSpace(orderID))
	if len(statusURL) > 0 && strings.TrimSpace(statusURL[0]) != "" {
		endpoint = strings.TrimSpace(statusURL[0])
	}
	value, _, err := c.doAuthenticated(ctx, credentials, http.MethodGet, endpoint, nil)
	if err != nil {
		return Order{}, err
	}
	order := parseOrderValue(value)
	if order.ID == "" {
		order.ID = strings.TrimSpace(orderID)
	}
	return order, nil
}

func (c *Client) Take(ctx context.Context, credentials Credentials, orderID string, takeURL ...string) (TakeResult, error) {
	if isNvtokens(credentials) {
		return c.nvtokensTake(ctx, credentials, orderID, takeURL...)
	}
	if strings.EqualFold(strings.TrimSpace(credentials.DeliveryMode), "cpa_zip") {
		return c.downloadCPA(ctx, credentials, orderID)
	}
	endpoint := "/api/customer/pickup/orders/" + url.PathEscape(strings.TrimSpace(orderID)) + "/take"
	if len(takeURL) > 0 && strings.TrimSpace(takeURL[0]) != "" {
		endpoint = strings.TrimSpace(takeURL[0])
	}
	value, status, err := c.doAuthenticatedWithTimeout(ctx, credentials, http.MethodPost, endpoint, nil, c.takeTimeout)
	if err != nil {
		return TakeResult{}, err
	}
	order := parseOrderValue(value)
	if order.ID == "" {
		order.ID = strings.TrimSpace(orderID)
	}
	accounts := rawAccounts(value)
	items := orderItems(value)
	return TakeResult{
		Order:                order,
		Accounts:             accounts,
		OrderItems:           items,
		ItemRemainingSeconds: orderItemRemainingSeconds(items),
		ReplacementFiles:     replacementFiles(value),
		Pending:              status == http.StatusAccepted,
	}, nil
}

func isNvtokens(credentials Credentials) bool {
	return strings.EqualFold(strings.TrimSpace(credentials.PlatformType), nvtokensPlatform)
}

// nvtokensPurchasePayload mirrors the workspace extraction request used by the
// nvtokens UI. CPAM supplies the product, quantity and platform-level purchase
// filters while keeping the remaining supplier rules at their neutral values.
func nvtokensPurchasePayload(credentials Credentials, product string, quantity int) map[string]any {
	salePlan := normalizeNvtokensSalePlan(product)
	credentialType := normalizeNvtokensCredentialType(credentials.PurchaseAccountType)
	var maxUnitPriceCents any
	if credentials.MaxUnitPriceFen > 0 {
		maxUnitPriceCents = credentials.MaxUnitPriceFen
	}
	return map[string]any{
		"quantity":                        quantity,
		"credential_type":                 credentialType,
		"inventory_token_filter":          "all",
		"sale_plan_filter":                salePlan,
		"subscription_payment_channels":   []string{},
		"plus_subscription_time_filter":   "all",
		"plus_subscription_custom_hours":  nil,
		"plus_subscription_age_max_hours": nil,
		"email_suffixes":                  []string{},
		"max_unit_price_cents":            maxUnitPriceCents,
		"purchase_priority":               "price_first",
		"preferred_sellers":               append([]string(nil), credentials.PreferredSellers...),
		"seller_whitelist":                append([]string(nil), credentials.SellerWhitelist...),
		"seller_blacklist":                append([]string(nil), credentials.SellerBlacklist...),
		"preferred_channel_ids":           append([]string(nil), credentials.PreferredChannelIDs...),
	}
}

func normalizeNvtokensSalePlan(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "oauth_30d", "oauth_7d":
		return "plus"
	case "team_1h":
		return "team"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

var nvtokensSalePlanOrder = []string{
	"plus", "pro", "team", "bugteam", "k12", "grokfree", "grokpro", "free",
}

func nvtokensSalePlanLabel(code string) string {
	switch normalizeNvtokensSalePlan(code) {
	case "plus":
		return "Plus"
	case "pro":
		return "Pro"
	case "team":
		return "Team"
	case "bugteam":
		return "BugTeam"
	case "k12":
		return "K12"
	case "grokfree":
		return "Grok Free"
	case "grokpro":
		return "GrokPro"
	case "free":
		return "Free"
	default:
		return strings.TrimSpace(code)
	}
}

type nvtokensCatalogAggregate struct {
	available int
	minFen    int64
	maxFen    int64
}

func (c *Client) nvtokensProductCatalog(ctx context.Context, credentials Credentials) (ProductCatalog, error) {
	value, _, err := c.doAuthenticated(
		ctx,
		credentials,
		http.MethodGet,
		"/api/workspace/seller-candidates",
		nil,
	)
	if err != nil {
		return ProductCatalog{}, err
	}
	aggregates := make(map[string]nvtokensCatalogAggregate)
	for _, seller := range nvtokensSellerCandidates(value) {
		plans := make(map[string]struct{})
		if values, ok := seller["sale_plans"].([]any); ok {
			for _, raw := range values {
				if code := normalizeNvtokensSalePlan(fmt.Sprint(raw)); code != "" {
					plans[code] = struct{}{}
				}
			}
		}
		counts, _ := seller["sale_plan_counts"].(map[string]any)
		prices, _ := seller["sale_plan_prices"].(map[string]any)
		stats, _ := seller["sale_plan_stats"].(map[string]any)
		for code := range counts {
			plans[normalizeNvtokensSalePlan(code)] = struct{}{}
		}
		for code := range prices {
			plans[normalizeNvtokensSalePlan(code)] = struct{}{}
		}
		for code := range stats {
			plans[normalizeNvtokensSalePlan(code)] = struct{}{}
		}
		for code := range plans {
			if code == "" || len(code) > 64 {
				continue
			}
			aggregate := aggregates[code]
			available := intValue(counts, code)
			stat, _ := stats[code].(map[string]any)
			available = maxInt(available, intValue(stat, "available_count", "availableCount"))
			if available == 0 && counts == nil && code == "plus" {
				available = intValue(seller, "available_count", "availableCount")
			}
			aggregate.available += maxInt(available, 0)

			price, _ := prices[code].(map[string]any)
			minFen := int64Value(price, "min_cents", "minCents", "price_min_cents", "priceMinCents")
			maxFen := int64Value(price, "max_cents", "maxCents", "price_max_cents", "priceMaxCents")
			if minFen == 0 {
				minFen = int64Value(stat, "min_cents", "minCents", "price_min_cents", "priceMinCents")
			}
			if maxFen == 0 {
				maxFen = int64Value(stat, "max_cents", "maxCents", "price_max_cents", "priceMaxCents")
			}
			if minFen > 0 && (aggregate.minFen == 0 || minFen < aggregate.minFen) {
				aggregate.minFen = minFen
			}
			if maxFen > aggregate.maxFen {
				aggregate.maxFen = maxFen
			}
			aggregates[code] = aggregate
		}
	}

	ordered := make([]string, 0, len(aggregates))
	seen := make(map[string]struct{}, len(aggregates))
	for _, code := range nvtokensSalePlanOrder {
		if _, ok := aggregates[code]; ok {
			ordered = append(ordered, code)
			seen[code] = struct{}{}
		}
	}
	extra := make([]string, 0, len(aggregates))
	for code := range aggregates {
		if _, ok := seen[code]; !ok {
			extra = append(extra, code)
		}
	}
	sort.Strings(extra)
	ordered = append(ordered, extra...)
	products := make([]ProductCatalogItem, 0, len(ordered))
	for _, code := range ordered {
		aggregate := aggregates[code]
		products = append(products, ProductCatalogItem{
			Code:            code,
			Label:           nvtokensSalePlanLabel(code),
			Available:       aggregate.available,
			MinUnitPriceFen: aggregate.minFen,
			MaxUnitPriceFen: aggregate.maxFen,
		})
	}
	if len(products) == 0 {
		return ProductCatalog{}, errors.New("nvtokens product catalog did not include any sale plans")
	}
	return ProductCatalog{Products: products}, nil
}

func (c *Client) nvtokensMarketplaceSellerCandidates(
	ctx context.Context,
	credentials Credentials,
	product string,
) ([]MarketplaceSellerCandidate, error) {
	product = normalizeNvtokensSalePlan(product)
	query := url.Values{}
	if product != "" {
		query.Set("sale_plan_filter", product)
	}
	endpoint := "/api/workspace/seller-candidates"
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	value, _, err := c.doAuthenticated(ctx, credentials, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	candidates := make([]MarketplaceSellerCandidate, 0)
	seen := make(map[string]struct{})
	for _, seller := range nvtokensSellerCandidates(value) {
		selectionToken := firstNonEmpty(
			stringValue(seller, "selection_token", "selectionToken"),
			stringValue(seller, "seller_token", "sellerToken"),
		)
		sellerID := firstNonEmpty(
			stringValue(seller, "seller_token", "sellerToken"),
			selectionToken,
			stringValue(seller, "id", "supplier_id", "supplierId"),
		)
		if selectionToken == "" || sellerID == "" {
			continue
		}
		counts, _ := seller["sale_plan_counts"].(map[string]any)
		prices, _ := seller["sale_plan_prices"].(map[string]any)
		stats, _ := seller["sale_plan_stats"].(map[string]any)
		stat, _ := stats[product].(map[string]any)
		price, _ := prices[product].(map[string]any)
		available := maxInt(intValue(counts, product), intValue(stat, "available_count", "availableCount"))
		if available == 0 && product == "plus" {
			available = intValue(seller, "available_count", "availableCount")
		}
		minFen := int64Value(price, "min_cents", "minCents", "price_min_cents", "priceMinCents")
		maxFen := int64Value(price, "max_cents", "maxCents", "price_max_cents", "priceMaxCents")
		if minFen == 0 {
			minFen = int64Value(stat, "min_cents", "minCents", "price_min_cents", "priceMinCents")
		}
		if maxFen == 0 {
			maxFen = int64Value(stat, "max_cents", "maxCents", "price_max_cents", "priceMaxCents")
		}
		key := strings.ToLower(strings.TrimSpace(selectionToken))
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		candidate := MarketplaceSellerCandidate{
			SellerID:        sellerID,
			Name:            firstNonEmpty(stringValue(seller, "display_name", "displayName"), stringValue(seller, "name", "username")),
			SelectionToken:  selectionToken,
			ChannelID:       stringValue(seller, "channel_id", "channelId"),
			Product:         product,
			Available:       maxInt(available, 0),
			MinUnitPriceFen: minFen,
			MaxUnitPriceFen: maxFen,
			PurchasedBefore: boolValue(seller, "purchased_before", "purchasedBefore"),
			PurchaseCount:   intValue(seller, "purchase_count", "purchaseCount"),
		}
		if score, ok := float64ValueOK(seller, "quality_score", "qualityScore", "rank_score", "rankScore"); ok {
			candidate.QualityScore = &score
		}
		if rate, ok := float64ValueOK(seller, "active_rate_percent", "activeRatePercent"); ok {
			candidate.ActiveRatePercent = &rate
		}
		candidates = append(candidates, candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		leftPrice := candidates[i].MinUnitPriceFen
		rightPrice := candidates[j].MinUnitPriceFen
		if leftPrice <= 0 {
			leftPrice = math.MaxInt64
		}
		if rightPrice <= 0 {
			rightPrice = math.MaxInt64
		}
		if leftPrice != rightPrice {
			return leftPrice < rightPrice
		}
		return strings.ToLower(candidates[i].Name) < strings.ToLower(candidates[j].Name)
	})
	return candidates, nil
}

func nvtokensSellerCandidates(value any) []map[string]any {
	root, ok := value.(map[string]any)
	if !ok || root == nil {
		return nil
	}
	for _, key := range []string{"sellers", "candidates", "items"} {
		if values, ok := root[key].([]any); ok {
			result := make([]map[string]any, 0, len(values))
			for _, raw := range values {
				if seller, ok := raw.(map[string]any); ok {
					result = append(result, seller)
				}
			}
			return result
		}
	}
	for _, key := range []string{"data", "payload", "result"} {
		if child, ok := root[key]; ok {
			if result := nvtokensSellerCandidates(child); len(result) > 0 {
				return result
			}
		}
	}
	return nil
}

func normalizeNvtokensCredentialType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "has_refresh_token", "access_refresh", "refresh_token":
		return "has_refresh_token"
	case "without_refresh_token", "access_token", "id_token", "session_token", "unknown":
		return "without_refresh_token"
	default:
		return "all"
	}
}

func (c *Client) nvtokensInventory(ctx context.Context, credentials Credentials, product string, quantity int) (Inventory, error) {
	if quantity <= 0 {
		quantity = 1
	}
	var value any
	var status int
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		value, status, err = c.doAuthenticatedWithTimeout(
			ctx,
			credentials,
			http.MethodPost,
			"/api/workspace/extractions/estimate",
			nvtokensPurchasePayload(credentials, product, quantity),
			c.timeout,
		)
		if err != nil {
			return Inventory{}, err
		}
		if status != http.StatusNoContent {
			break
		}
		if attempt == 0 {
			timer := time.NewTimer(nvtokensEstimateRetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return Inventory{}, ctx.Err()
			case <-timer.C:
			}
		}
	}
	if status == http.StatusNoContent {
		return Inventory{}, ErrNvtokensEstimateUnavailable
	}
	root := primaryObject(value)
	estimate := nestedObject(root, "estimate", "quote", "pricing")
	available := intValue(root,
		"available_quantity", "availableQuantity", "inventory_available", "inventoryAvailable",
		"max_quantity", "maxQuantity", "available", "matched_quantity", "matchedQuantity")
	if available == 0 {
		available = intValue(estimate,
			"available_quantity", "availableQuantity", "inventory_available", "inventoryAvailable",
			"max_quantity", "maxQuantity", "available", "matched_quantity", "matchedQuantity")
	}
	totalFen := int64Value(root, "estimated_total_fen", "estimatedTotalFen", "total_fen", "totalFen")
	if totalFen == 0 {
		totalFen = int64Value(estimate, "estimated_total_fen", "estimatedTotalFen", "total_fen", "totalFen")
	}
	if totalFen == 0 {
		totalFen = int64Value(root, "buyer_total_cents", "buyerTotalCents", "amount_cents", "amountCents")
		if totalFen == 0 {
			totalFen = int64Value(estimate, "buyer_total_cents", "buyerTotalCents", "amount_cents", "amountCents")
		}
	}
	unitFen := int64Value(root, "estimated_unit_price_fen", "estimatedUnitPriceFen", "unit_fen", "unitFen")
	if unitFen == 0 {
		unitFen = int64Value(estimate, "estimated_unit_price_fen", "estimatedUnitPriceFen", "unit_fen", "unitFen")
	}
	if unitFen == 0 {
		unitFen = int64Value(root, "unit_price_cents", "unitPriceCents", "min_unit_price_cents", "minUnitPriceCents")
		if unitFen == 0 {
			unitFen = int64Value(estimate, "unit_price_cents", "unitPriceCents", "min_unit_price_cents", "minUnitPriceCents")
		}
	}
	if totalFen == 0 {
		totalFen = int64Value(root, "total_cost_cents", "totalCostCents", "total_price_cents", "totalPriceCents")
		if totalFen == 0 {
			totalFen = int64Value(estimate, "total_cost_cents", "totalCostCents", "total_price_cents", "totalPriceCents")
		}
	}
	if unitFen == 0 {
		unitFen = int64Value(root, "unit_price_cents", "unitPriceCents", "price_cents", "priceCents")
		if unitFen == 0 {
			unitFen = int64Value(estimate, "unit_price_cents", "unitPriceCents", "price_cents", "priceCents")
		}
	}
	if totalFen == 0 && unitFen > 0 {
		totalFen = unitFen * int64(quantity)
	}
	if unitFen == 0 && totalFen > 0 {
		unitFen = totalFen / int64(maxInt(quantity, 1))
	}
	// Some compatible suppliers omit inventory counts while still returning a
	// valid price quote. Only infer availability after a positive quote is
	// present. A zero-price response with matched_quantity=0 means the current
	// filters (especially a hard max-unit-price ceiling) found no inventory.
	if available == 0 && quantity > 0 && (totalFen > 0 || unitFen > 0) {
		available = quantity
	}
	return Inventory{
		Product:               strings.TrimSpace(product),
		RequestedQuantity:     quantity,
		Available:             maxInt(available, 0),
		Missing:               maxInt(quantity-available, 0),
		EstimatedTotalFen:     totalFen,
		EstimatedUnitPriceFen: unitFen,
	}, nil
}

func (c *Client) nvtokensBalance(ctx context.Context, credentials Credentials) (Balance, error) {
	value, _, err := c.doAuthenticated(ctx, credentials, http.MethodGet, "/api/me", nil)
	if err != nil {
		return Balance{}, err
	}
	root := primaryObject(value)
	user := nestedObject(root, "user", "account", "balance")
	balance := int64Value(root, "balance_cents", "balanceCents", "balance_fen", "balanceFen")
	if balance == 0 {
		balance = int64Value(user, "balance_cents", "balanceCents", "balance_fen", "balanceFen")
	}
	held := int64Value(root, "frozen_balance_cents", "frozenBalanceCents", "held_fen", "heldFen")
	if held == 0 {
		held = int64Value(user, "frozen_balance_cents", "frozenBalanceCents", "held_fen", "heldFen")
	}
	available := int64Value(root, "available_balance_cents", "availableBalanceCents", "available_fen", "availableFen")
	if available == 0 {
		available = int64Value(user, "available_balance_cents", "availableBalanceCents", "available_fen", "availableFen")
	}
	if available == 0 && balance > held {
		available = balance - held
	}
	return Balance{
		BalanceFen:   balance,
		HeldFen:      held,
		AvailableFen: available,
		Currency:     firstNonEmpty(stringValue(root, "currency"), stringValue(user, "currency"), "CNY"),
	}, nil
}

func (c *Client) nvtokensCreateOrder(ctx context.Context, credentials Credentials, product string, quantity int, idempotencyKey ...string) (Order, error) {
	headers := make(http.Header)
	if len(idempotencyKey) > 0 && strings.TrimSpace(idempotencyKey[0]) != "" {
		headers.Set("Idempotency-Key", strings.TrimSpace(idempotencyKey[0]))
	}
	value, status, err := c.doAuthenticatedWithHeaders(
		ctx,
		credentials,
		http.MethodPost,
		"/api/workspace/extractions/batch",
		nvtokensPurchasePayload(credentials, product, quantity),
		headers,
		nvtokensBatchTimeout,
	)
	if err != nil {
		return Order{}, err
	}
	orderID := firstNonEmpty(
		nvtokensBatchOrderID(value),
		findString(value, "id", "order_id", "orderId", "extraction_id", "extractionId", "request_id", "requestId"),
		firstString(idempotencyKey...),
		fmt.Sprintf("nvtokens-%d", time.Now().UnixNano()),
	)
	order := parseOrderValue(value)
	order.ID = orderID
	if order.Product == "" {
		order.Product = strings.TrimSpace(product)
	}
	if order.Quantity == 0 {
		order.Quantity = quantity
	}
	accounts := nvtokensResultAccounts(value)
	extractedQuantity := nvtokensExtractedQuantity(value)
	order.ReadyQuantity = len(accounts)
	if order.Progress == 0 && (status >= http.StatusOK && status < http.StatusMultipleChoices) {
		order.Progress = 100
	}
	if order.Status == "" {
		if status == http.StatusAccepted {
			order.Status = "processing"
		} else {
			order.Status = "completed"
		}
	}
	order.ChargedFen = firstInt64(
		order.ChargedFen,
		nvtokensBatchChargedFen(value),
		findInt64(value, "charged_fen", "chargedFen", "total_cost_cents", "totalCostCents", "cost_cents", "costCents"),
	)
	items := nvtokensResultOrderItems(value, order.ChargedFen, len(accounts))
	result := TakeResult{
		Order:                order,
		Accounts:             accounts,
		OrderItems:           items,
		ItemRemainingSeconds: orderItemRemainingSeconds(items),
		Pending:              status == http.StatusAccepted,
	}
	if len(result.Accounts) > 0 {
		order.Status = "completed"
		result.Pending = false
		result.Order.Status = "completed"
		result.Order.ReadyQuantity = len(result.Accounts)
		result.Order.Progress = 100
	} else if status == http.StatusAccepted {
		// NV occasionally reports a completed quantity while its preferred bundle
		// is empty. Do not expose a phantom ready account: keep polling the same
		// extraction only when the supplier explicitly accepted asynchronous work.
		order.Status = "processing"
		order.ReadyQuantity = 0
		order.Progress = 0
		result.Order = order
		result.Pending = true
	} else if nvtokensSuccessfulStatusWithoutAccounts(order.Status) && nvtokensPurchaseEvidence(value, order) {
		// A synchronous 2xx can already represent a paid extraction even when a
		// newly introduced response shape prevents this client from finding the
		// account payload. Keep the same supplier operation open for reconciliation;
		// treating it as a free failure would let the purchase task charge again.
		order.Status = "processing"
		order.ReadyQuantity = extractedQuantity
		order.Progress = 0
		result.Order = order
		result.Pending = true
	} else if nvtokensSuccessfulStatusWithoutAccounts(order.Status) {
		order.Status = "failed"
		order.ReadyQuantity = 0
		order.Progress = 0
		order.RetryAfterSeconds = maxInt(order.RetryAfterSeconds, 30)
		result.Order = order
		result.Pending = false
	}
	if len(result.Accounts) > 0 {
		c.mu.Lock()
		c.nvtokensResults[credentialKey(credentials)+"|"+order.ID] = result
		c.mu.Unlock()
	}
	return order, nil
}

func (c *Client) nvtokensGetOrder(ctx context.Context, credentials Credentials, orderID string, statusURL ...string) (Order, error) {
	key := credentialKey(credentials) + "|" + strings.TrimSpace(orderID)
	c.mu.Lock()
	if result, ok := c.nvtokensResults[key]; ok {
		c.mu.Unlock()
		return result.Order, nil
	}
	c.mu.Unlock()
	endpoint := "/api/workspace/extractions/" + url.PathEscape(strings.TrimSpace(orderID))
	if len(statusURL) > 0 && strings.TrimSpace(statusURL[0]) != "" {
		endpoint = strings.TrimSpace(statusURL[0])
	}
	value, status, err := c.nvtokensReadExtraction(ctx, credentials, strings.TrimSpace(orderID), endpoint, defaultTimeout)
	if err != nil {
		return Order{}, err
	}
	order := parseOrderValue(value)
	if order.ID == "" {
		order.ID = strings.TrimSpace(orderID)
	}
	order.ChargedFen = firstInt64(order.ChargedFen, nvtokensBatchChargedFen(value))
	if order.Status == "" {
		order.Status = "completed"
	}
	if status == http.StatusAccepted {
		order.Status = "processing"
	}
	accounts := nvtokensResultAccounts(value)
	if len(accounts) > 0 {
		order.Status = "completed"
		order.ReadyQuantity = len(accounts)
		order.Progress = 100
	} else {
		order.ReadyQuantity = 0
		if status == http.StatusAccepted {
			order.Status = "processing"
			order.Progress = 0
		} else if nvtokensSuccessfulStatusWithoutAccounts(order.Status) {
			if nvtokensPurchaseEvidence(value, order) {
				order.Status = "processing"
			} else {
				order.Status = "failed"
				order.RetryAfterSeconds = maxInt(order.RetryAfterSeconds, 30)
			}
			order.Progress = 0
		}
	}
	if len(accounts) > 0 {
		items := nvtokensResultOrderItems(value, order.ChargedFen, len(accounts))
		c.mu.Lock()
		c.nvtokensResults[key] = TakeResult{
			Order:                order,
			Accounts:             accounts,
			OrderItems:           items,
			ItemRemainingSeconds: orderItemRemainingSeconds(items),
		}
		c.mu.Unlock()
	}
	return order, nil
}

func (c *Client) nvtokensTake(ctx context.Context, credentials Credentials, orderID string, takeURL ...string) (TakeResult, error) {
	key := credentialKey(credentials) + "|" + strings.TrimSpace(orderID)
	c.mu.Lock()
	if result, ok := c.nvtokensResults[key]; ok && len(result.Accounts) > 0 {
		c.mu.Unlock()
		return result, nil
	}
	c.mu.Unlock()
	endpoint := "/api/workspace/extractions/" + url.PathEscape(strings.TrimSpace(orderID))
	if len(takeURL) > 0 && strings.TrimSpace(takeURL[0]) != "" {
		endpoint = strings.TrimSpace(takeURL[0])
	}
	value, _, err := c.nvtokensReadExtraction(ctx, credentials, strings.TrimSpace(orderID), endpoint, nvtokensBatchTimeout)
	if err != nil {
		return TakeResult{}, err
	}
	order := parseOrderValue(value)
	if order.ID == "" {
		order.ID = strings.TrimSpace(orderID)
	}
	order.ChargedFen = firstInt64(order.ChargedFen, nvtokensBatchChargedFen(value))
	if order.Status == "" {
		order.Status = "completed"
	}
	accounts := nvtokensResultAccounts(value)
	if len(accounts) == 0 {
		return TakeResult{}, errors.New("nvtokens extraction response did not include importable account JSON")
	}
	order.Status = "completed"
	order.ReadyQuantity = len(accounts)
	order.Progress = 100
	items := nvtokensResultOrderItems(value, order.ChargedFen, len(accounts))
	result := TakeResult{
		Order:                order,
		Accounts:             accounts,
		OrderItems:           items,
		ItemRemainingSeconds: orderItemRemainingSeconds(items),
	}
	c.mu.Lock()
	c.nvtokensResults[key] = result
	c.mu.Unlock()
	return result, nil
}

// nvtokensReadExtraction first uses the historical single-order route and then
// falls back to the workspace purchase ledger. NV exposes paid accounts through
// GET /api/workspace/extractions, but does not expose a compatible
// /extractions/{id} route. Treating that route's 404 as a cancelled order hides
// already-paid inventory, so both status reconciliation and Take share this
// lookup path.
func (c *Client) nvtokensReadExtraction(
	ctx context.Context,
	credentials Credentials,
	orderID string,
	endpoint string,
	requestTimeout time.Duration,
) (any, int, error) {
	value, status, err := c.doAuthenticatedWithTimeout(ctx, credentials, http.MethodGet, endpoint, nil, requestTimeout)
	if err == nil || !httpErrorStatus(err, http.StatusNotFound) || strings.TrimSpace(orderID) == "" {
		return value, status, err
	}
	directErr := err
	if recovered, recoveredStatus, found, lookupErr := c.nvtokensLookupExtraction(ctx, credentials, orderID, requestTimeout); lookupErr != nil {
		return nil, recoveredStatus, lookupErr
	} else if found {
		return recovered, recoveredStatus, nil
	}
	return nil, status, directErr
}

func (c *Client) nvtokensLookupExtraction(
	ctx context.Context,
	credentials Credentials,
	orderID string,
	requestTimeout time.Duration,
) (any, int, bool, error) {
	orderID = strings.TrimSpace(orderID)
	queryEndpoint := "/api/workspace/extractions?page=1&limit=100&q=" + url.QueryEscape(orderID)
	value, status, err := c.doAuthenticatedWithTimeout(ctx, credentials, http.MethodGet, queryEndpoint, nil, requestTimeout)
	if err != nil {
		return nil, status, false, err
	}
	if matches := nvtokensMatchingExtractions(value, orderID, false); len(matches) > 0 {
		return map[string]any{"orders": matches}, status, true, nil
	}

	// Multi-account NV purchases are represented by several extraction rows
	// sharing one extraction_batch_id. The list API supports batch_id even though
	// its free-text q filter does not, so a persisted batch ID can be recovered
	// after a Manager restart without purchasing the accounts again.
	batchEndpoint := "/api/workspace/extractions?page=1&limit=100&batch_id=" + url.QueryEscape(orderID)
	value, status, err = c.doAuthenticatedWithTimeout(ctx, credentials, http.MethodGet, batchEndpoint, nil, requestTimeout)
	if err != nil {
		return nil, status, false, err
	}
	matches := nvtokensMatchingExtractions(value, orderID, true)
	for page := 2; page <= 10 && nvtokensExtractionPageHasNext(value, page-1); page++ {
		pageEndpoint := "/api/workspace/extractions?page=" + strconv.Itoa(page) +
			"&limit=100&batch_id=" + url.QueryEscape(orderID)
		pageValue, pageStatus, pageErr := c.doAuthenticatedWithTimeout(ctx, credentials, http.MethodGet, pageEndpoint, nil, requestTimeout)
		status = pageStatus
		if pageErr != nil {
			return nil, status, false, pageErr
		}
		matches = append(matches, nvtokensMatchingExtractions(pageValue, orderID, true)...)
		value = pageValue
	}
	if len(matches) == 0 {
		return nil, status, false, nil
	}
	return map[string]any{"orders": matches}, status, true, nil
}

func nvtokensExtractionPageHasNext(value any, currentPage int) bool {
	root, ok := nvtokensObject(value)
	if !ok {
		return false
	}
	pagination, ok := nvtokensObject(root["pagination"])
	if !ok {
		return false
	}
	if hasBoolField(pagination, "has_next") {
		return boolValue(pagination, "has_next")
	}
	totalPages := intValue(pagination, "total_pages", "totalPages")
	return totalPages > currentPage
}

func nvtokensMatchingExtractions(value any, orderID string, matchBatch bool) []any {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return nil
	}
	objects := nvtokensResultObjects(value)
	result := make([]any, 0, len(objects))
	for _, object := range objects {
		candidate := strings.TrimSpace(stringValue(object, "id", "order_id", "orderId", "extraction_id", "extractionId"))
		if matchBatch {
			candidate = strings.TrimSpace(stringValue(object,
				"extraction_batch_id", "extractionBatchId", "batch_id", "batchId"))
		}
		if candidate == orderID {
			result = append(result, object)
		}
	}
	return result
}

func httpErrorStatus(err error, status int) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == status
}

func nvtokensResultAccounts(value any) []json.RawMessage {
	collector := nvtokensAccountCollector{seen: make(map[string]struct{})}
	collectNvtokensResultEntries(value, &collector)
	if len(collector.accounts) > 0 {
		return collector.accounts
	}
	collectNvtokensBundles(value, &collector)
	return collector.accounts
}

func nvtokensBatchOrderID(value any) string {
	results := nvtokensResultObjects(value)
	unique := make(map[string]struct{})
	batchIDs := make(map[string]struct{})
	for _, result := range results {
		if nvtokensResultFailed(result) {
			continue
		}
		order := nvtokensResultOrder(result)
		id := strings.TrimSpace(stringValue(order, "id", "order_id", "orderId", "extraction_id", "extractionId"))
		if id != "" {
			unique[id] = struct{}{}
		}
		batchID := strings.TrimSpace(stringValue(order,
			"extraction_batch_id", "extractionBatchId", "batch_id", "batchId"))
		if batchID == "" {
			batchID = strings.TrimSpace(stringValue(result,
				"extraction_batch_id", "extractionBatchId", "batch_id", "batchId"))
		}
		if batchID != "" {
			batchIDs[batchID] = struct{}{}
		}
	}
	if len(unique) == 1 {
		for id := range unique {
			return id
		}
	}
	// A multi-account purchase has one remote extraction per account. Persist
	// the shared batch identifier rather than a local create-* id so the paid
	// delivery remains discoverable through the ledger after a restart.
	if len(batchIDs) == 1 {
		for id := range batchIDs {
			return id
		}
	}
	return ""
}

func nvtokensBatchChargedFen(value any) int64 {
	if root, ok := nvtokensObject(value); ok {
		for _, key := range []string{"summary", "pricing", "quote"} {
			if aggregate, ok := nvtokensObject(root[key]); ok {
				if charged := nvtokensChargedFen(aggregate); charged > 0 {
					return charged
				}
			}
		}
		if charged := nvtokensChargedFen(root); charged > 0 {
			return charged
		}
	}

	var total int64
	for _, result := range nvtokensResultObjects(value) {
		if nvtokensResultFailed(result) {
			continue
		}
		if charged := nvtokensChargedFen(nvtokensResultOrder(result)); charged > 0 {
			total += charged
		}
	}
	return total
}

func nvtokensExtractedQuantity(value any) int {
	root, ok := nvtokensObject(value)
	if !ok {
		return 0
	}
	for _, key := range []string{"summary", "pricing", "quote"} {
		if aggregate, ok := nvtokensObject(root[key]); ok {
			if quantity := intValue(aggregate, "extracted", "purchased", "succeeded", "success_count", "successCount"); quantity > 0 {
				return quantity
			}
		}
	}
	if quantity := intValue(root, "extracted", "purchased", "succeeded", "success_count", "successCount"); quantity > 0 {
		return quantity
	}
	count := 0
	for _, result := range nvtokensResultObjects(value) {
		if !nvtokensResultFailed(result) {
			count++
		}
	}
	return count
}

func nvtokensPurchaseEvidence(value any, order Order) bool {
	return order.ChargedFen > 0 || nvtokensExtractedQuantity(value) > 0 || nvtokensBatchOrderID(value) != ""
}

func nvtokensResultOrderItems(value any, fallbackChargedFen int64, accountCount int) []OrderItem {
	results := nvtokensResultObjects(value)
	items := make([]OrderItem, 0, len(results))
	for _, result := range results {
		if nvtokensResultFailed(result) {
			continue
		}
		order := nvtokensResultOrder(result)
		charged := nvtokensChargedFen(order)
		if charged <= 0 {
			charged = nvtokensChargedFen(result)
		}
		remaining, hasRemaining := int64ValueOK(order,
			"remaining_seconds", "remainingSeconds", "remaining_valid_seconds", "remainingValidSeconds")
		if !hasRemaining {
			remaining, hasRemaining = int64ValueOK(result,
				"remaining_seconds", "remainingSeconds", "remaining_valid_seconds", "remainingValidSeconds")
		}
		if !hasRemaining {
			remaining, hasRemaining = nvtokensWarrantyRemainingSeconds(order)
		}
		if !hasRemaining {
			remaining, hasRemaining = nvtokensWarrantyRemainingSeconds(result)
		}
		items = append(items, OrderItem{
			RemainingSeconds: remaining,
			HasRemaining:     hasRemaining,
			BasePriceFen:     charged,
			ChargedFen:       charged,
		})
	}
	if len(items) > 0 {
		// NV can return the per-account warranty window while charging only at
		// the batch summary level. Do not discard every warranty timestamp merely
		// because an individual result omitted its price. Split the trusted batch
		// total across only the zero-priced delivered items; explicit item prices
		// remain authoritative.
		knownCharged := int64(0)
		missingPriceIndexes := make([]int, 0, len(items))
		for index := range items {
			if items[index].ChargedFen > 0 {
				knownCharged += items[index].ChargedFen
				continue
			}
			missingPriceIndexes = append(missingPriceIndexes, index)
		}
		remainingCharged := max(fallbackChargedFen-knownCharged, 0)
		for ordinal, index := range missingPriceIndexes {
			charged := splitFenShare(remainingCharged, len(missingPriceIndexes), ordinal)
			items[index].BasePriceFen = charged
			items[index].ChargedFen = charged
		}
		return items
	}
	if accountCount == 1 && fallbackChargedFen > 0 {
		return []OrderItem{{BasePriceFen: fallbackChargedFen, ChargedFen: fallbackChargedFen}}
	}
	return nil
}

func nvtokensWarrantyRemainingSeconds(value map[string]any) (int64, bool) {
	if value == nil {
		return 0, false
	}
	if expiresAtMS, ok := int64ValueOK(value,
		"warranty_expires_at_ms", "warrantyExpiresAtMs", "warranty_until_ms", "warrantyUntilMs"); ok {
		return max(int64(0), (expiresAtMS-time.Now().UnixMilli()+999)/1000), true
	}
	text := strings.TrimSpace(stringValue(value,
		"warranty_until", "warrantyUntil", "warranty_expires_at", "warrantyExpiresAt"))
	if text == "" {
		return 0, false
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return 0, false
	}
	return max(int64(0), (expiresAt.UnixMilli()-time.Now().UnixMilli()+999)/1000), true
}

func splitFenShare(total int64, count int, ordinal int) int64 {
	if total <= 0 || count <= 0 || ordinal < 0 || ordinal >= count {
		return 0
	}
	share := total / int64(count)
	if int64(ordinal) < total%int64(count) {
		share++
	}
	return share
}

func nvtokensResultObjects(value any) []map[string]any {
	root, ok := nvtokensObject(value)
	if !ok {
		return nil
	}
	for _, key := range []string{"results", "items", "orders"} {
		list, ok := root[key].([]any)
		if !ok {
			continue
		}
		results := make([]map[string]any, 0, len(list))
		for _, item := range list {
			if object, ok := nvtokensObject(item); ok {
				results = append(results, object)
			}
		}
		if len(results) > 0 {
			return results
		}
	}
	for _, key := range []string{"data", "payload", "result"} {
		if child, exists := root[key]; exists && child != nil {
			if results := nvtokensResultObjects(child); len(results) > 0 {
				return results
			}
		}
	}
	return nil
}

func nvtokensResultOrder(result map[string]any) map[string]any {
	for _, key := range []string{"order", "extraction"} {
		if order, ok := result[key].(map[string]any); ok {
			return order
		}
	}
	return result
}

func nvtokensChargedFen(value map[string]any) int64 {
	return int64Value(value,
		"charged_fen", "chargedFen",
		"buyer_total_cents", "buyerTotalCents",
		"amount_cents", "amountCents",
		"total_cost_cents", "totalCostCents",
		"cost_cents", "costCents",
		"spent_cents", "spentCents",
	)
}

func nvtokensSuccessfulStatusWithoutAccounts(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "complete", "completed", "ready", "success", "succeeded":
		return true
	default:
		return false
	}
}

type nvtokensAccountCollector struct {
	accounts []json.RawMessage
	seen     map[string]struct{}
}

func collectNvtokensResultEntries(value any, collector *nvtokensAccountCollector) {
	root, ok := nvtokensObject(value)
	if !ok {
		return
	}
	collectNvtokensResultObject(root, collector)
	for _, key := range []string{"results", "items", "orders"} {
		list, ok := root[key].([]any)
		if !ok {
			continue
		}
		for _, item := range list {
			object, ok := nvtokensObject(item)
			if !ok || nvtokensResultFailed(object) {
				continue
			}
			collectNvtokensResultObject(object, collector)
		}
	}
	for _, key := range []string{"data", "payload", "result"} {
		if child, exists := root[key]; exists && child != nil {
			collectNvtokensResultEntries(child, collector)
		}
	}
}

func collectNvtokensResultObject(object map[string]any, collector *nvtokensAccountCollector) {
	if nvtokensResultFailed(object) {
		return
	}
	for _, key := range []string{
		"account_json", "accountJson",
		"card_payload", "cardPayload",
		"account", "credential",
	} {
		if candidate, exists := object[key]; exists && candidate != nil {
			collector.addCandidate(candidate)
		}
	}
}

func collectNvtokensBundles(value any, collector *nvtokensAccountCollector) {
	root, ok := nvtokensObject(value)
	if !ok {
		return
	}
	for _, key := range []string{
		"sub2api_bundle", "sub2apiBundle",
		"cpa_bundle", "cpaBundle",
		"cockpit_bundle", "cockpitBundle",
	} {
		if bundle, exists := root[key]; exists && bundle != nil {
			collector.addDeepCandidate(bundle)
		}
	}
	for _, key := range []string{"data", "payload", "result"} {
		if child, exists := root[key]; exists && child != nil {
			collectNvtokensBundles(child, collector)
		}
	}
}

func nvtokensResultFailed(object map[string]any) bool {
	if hasBoolField(object, "failed") && boolValue(object, "failed") {
		return true
	}
	for _, key := range []string{"success", "ok"} {
		if hasBoolField(object, key) && !boolValue(object, key) {
			return true
		}
	}
	switch strings.ToLower(strings.TrimSpace(stringValue(object, "status", "state"))) {
	case "failed", "error", "cancelled", "canceled", "refunded", "rejected":
		return true
	}
	return stringValue(object, "error", "error_message", "errorMessage") != ""
}

func (collector *nvtokensAccountCollector) addCandidate(value any) {
	value, ok := nvtokensDecodedValue(value)
	if !ok {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		if nvtokensOAuthAccount(typed) {
			collector.appendAccount(typed)
			return
		}
		for _, key := range []string{
			"account_json", "accountJson",
			"sub2api_account", "sub2apiAccount",
			"codex_account", "codexAccount",
			"card_payload", "cardPayload",
			"account", "credential",
		} {
			if child, exists := typed[key]; exists && child != nil {
				collector.addCandidate(child)
			}
		}
		for _, key := range []string{"accounts", "items"} {
			if list, ok := typed[key].([]any); ok {
				for _, child := range list {
					collector.addCandidate(child)
				}
			}
		}
	case []any:
		for _, child := range typed {
			collector.addCandidate(child)
		}
	}
}

func (collector *nvtokensAccountCollector) addDeepCandidate(value any) {
	value, ok := nvtokensDecodedValue(value)
	if !ok {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		if nvtokensOAuthAccount(typed) {
			collector.appendAccount(typed)
			return
		}
		for _, child := range typed {
			collector.addDeepCandidate(child)
		}
	case []any:
		for _, child := range typed {
			collector.addDeepCandidate(child)
		}
	}
}

func (collector *nvtokensAccountCollector) appendAccount(account map[string]any) {
	identities := nvtokensAccountIdentities(account)
	if len(identities) == 0 {
		return
	}
	for _, identity := range identities {
		if _, exists := collector.seen[identity]; exists {
			return
		}
	}
	data, err := json.Marshal(account)
	if err != nil || len(data) == 0 {
		return
	}
	for _, identity := range identities {
		collector.seen[identity] = struct{}{}
	}
	collector.accounts = append(collector.accounts, data)
}

func nvtokensDecodedValue(value any) (any, bool) {
	text, ok := value.(string)
	if !ok {
		return value, value != nil
	}
	data := bytes.TrimSpace([]byte(text))
	if len(data) == 0 || !json.Valid(data) {
		return nil, false
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}

func nvtokensObject(value any) (map[string]any, bool) {
	decoded, ok := nvtokensDecodedValue(value)
	if !ok {
		return nil, false
	}
	object, ok := decoded.(map[string]any)
	return object, ok
}

func nvtokensOAuthAccount(account map[string]any) bool {
	credentials := account
	if child, ok := account["credentials"].(map[string]any); ok {
		credentials = child
	}
	platform := strings.ToLower(strings.TrimSpace(stringValue(account, "platform", "provider")))
	typeName := strings.ToLower(strings.TrimSpace(stringValue(account, "type")))
	credentialType := strings.ToLower(strings.TrimSpace(stringValue(credentials, "type")))
	if platform != "" && platform != "openai" && platform != "codex" {
		return false
	}
	if typeName != "" && typeName != "oauth" && typeName != "codex" {
		return false
	}
	if credentialType != "" && credentialType != "oauth" && credentialType != "codex" && credentialType != "openai" {
		return false
	}
	return len(nvtokensAccountIdentities(account)) > 0
}

func nvtokensAccountIdentities(account map[string]any) []string {
	credentials := account
	if child, ok := account["credentials"].(map[string]any); ok {
		credentials = child
	}
	identities := make([]string, 0, 5)
	for _, field := range []string{
		"refresh_token", "refreshToken",
		"access_token", "accessToken",
		"session_access_token", "sessionAccessToken",
		"id_token", "idToken",
		"session_token", "sessionToken",
	} {
		if token := strings.TrimSpace(stringValue(credentials, field)); token != "" {
			identities = append(identities, fmt.Sprintf("%x", sha256.Sum256([]byte("token\x00"+token))))
		}
	}
	return identities
}

func (c *Client) downloadCPA(ctx context.Context, credentials Credentials, orderID string) (TakeResult, error) {
	endpoint := "/api/customer/pickup/orders/" + url.PathEscape(strings.TrimSpace(orderID)) + "/download?format=cpa"
	data, status, err := c.doAuthenticatedBytes(ctx, credentials, http.MethodGet, endpoint, c.takeTimeout)
	if err != nil {
		return TakeResult{}, err
	}
	accounts, items, err := cpaDeliveryFromZIP(data, time.Now())
	if err != nil {
		return TakeResult{}, err
	}
	return TakeResult{
		Order: Order{
			ID:            strings.TrimSpace(orderID),
			Status:        "completed",
			ReadyQuantity: len(accounts),
		},
		Accounts:             accounts,
		OrderItems:           items,
		ItemRemainingSeconds: orderItemRemainingSeconds(items),
		Pending:              status == http.StatusAccepted,
	}, nil
}

func (c *Client) doAuthenticatedBytes(ctx context.Context, credentials Credentials, method string, path string, requestTimeout time.Duration) ([]byte, int, error) {
	auth, err := c.login(ctx, credentials, false)
	if err != nil {
		return nil, 0, err
	}
	data, status, err := c.requestBytes(ctx, credentials.BaseURL, method, path, auth, requestTimeout)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized || !canRefreshAuthentication(credentials) {
		return data, status, err
	}
	c.invalidate(credentials)
	auth, err = c.login(ctx, credentials, true)
	if err != nil {
		return nil, 0, err
	}
	return c.requestBytes(ctx, credentials.BaseURL, method, path, auth, requestTimeout)
}

func (c *Client) requestBytes(ctx context.Context, baseURL string, method string, endpointRef string, auth tokenState, requestTimeout time.Duration) ([]byte, int, error) {
	if requestTimeout <= 0 {
		requestTimeout = c.timeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	endpoint, err := resolveEndpoint(baseURL, endpointRef)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(reqCtx, method, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; CPA-Manager/1.0)")
	applyAuthentication(req.Header, auth)
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, endpointRef, err)
	}
	defer res.Body.Close()
	limited := &io.LimitedReader{R: res.Body, N: maxDownloadBodyBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, res.StatusCode, err
	}
	if limited.N == 0 {
		return nil, res.StatusCode, errors.New("supply download exceeded size limit")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, res.StatusCode, &HTTPError{
			StatusCode:        res.StatusCode,
			Message:           errorMessage(data),
			Code:              errorCode(data),
			RetryAfterSeconds: responseRetryAfterSeconds(data, res.Header.Get("Retry-After"), time.Now()),
		}
	}
	return data, res.StatusCode, nil
}

type cpaManifest struct {
	Items []cpaManifestItem `json:"items"`
}

type cpaManifestItem struct {
	Ordinal       int    `json:"ordinal"`
	LogicalName   string `json:"logical_name"`
	ContentSHA256 string `json:"content_sha256"`
	ExpiresAt     string `json:"expires_at"`
}

func cpaDeliveryFromZIP(data []byte, now time.Time) ([]json.RawMessage, []OrderItem, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, fmt.Errorf("decode supply CPA ZIP: %w", err)
	}
	files := make(map[string]*zip.File, len(reader.File))
	fileNames := make([]string, 0, len(reader.File))
	var manifest *zip.File
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		name := normalizedZIPName(file.Name)
		if name == "" {
			return nil, nil, fmt.Errorf("supply CPA ZIP contains invalid entry path %q", file.Name)
		}
		if _, duplicate := files[name]; duplicate {
			return nil, nil, fmt.Errorf("supply CPA ZIP contains duplicate entry %s", name)
		}
		files[name] = file
		fileNames = append(fileNames, name)
		if strings.EqualFold(filepath.Base(name), "manifest.json") {
			manifest = file
		}
	}

	var manifestItems []cpaManifestItem
	if manifest != nil {
		payload, readErr := readZIPJSON(manifest)
		if readErr != nil {
			return nil, nil, readErr
		}
		var decoded cpaManifest
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return nil, nil, fmt.Errorf("decode supply CPA manifest: %w", err)
		}
		manifestItems = decoded.Items
		sort.SliceStable(manifestItems, func(i, j int) bool {
			if manifestItems[i].Ordinal == manifestItems[j].Ordinal {
				return normalizedZIPName(manifestItems[i].LogicalName) < normalizedZIPName(manifestItems[j].LogicalName)
			}
			return manifestItems[i].Ordinal < manifestItems[j].Ordinal
		})
	}

	accounts := make([]json.RawMessage, 0, len(reader.File))
	items := make([]OrderItem, 0, len(reader.File))
	seen := make(map[string]struct{}, len(manifestItems))
	for _, item := range manifestItems {
		name := normalizedZIPName(item.LogicalName)
		file := files[name]
		if file == nil {
			return nil, nil, fmt.Errorf("supply CPA manifest references missing entry %s", item.LogicalName)
		}
		payload, readErr := readZIPJSON(file)
		if readErr != nil {
			return nil, nil, readErr
		}
		if expected := strings.ToLower(strings.TrimSpace(item.ContentSHA256)); expected != "" {
			actual := fmt.Sprintf("%x", sha256.Sum256(payload))
			if actual != expected {
				return nil, nil, fmt.Errorf("supply CPA ZIP entry %s failed manifest checksum", filepath.Base(file.Name))
			}
		}
		if !looksLikeCPAAccount(payload) {
			return nil, nil, fmt.Errorf("supply CPA ZIP entry %s is not an importable account", filepath.Base(file.Name))
		}
		accounts = append(accounts, append(json.RawMessage(nil), payload...))
		orderItem := OrderItem{}
		if expiresAt, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(item.ExpiresAt)); parseErr == nil {
			orderItem.HasRemaining = true
			orderItem.RemainingSeconds = max(int64(expiresAt.Sub(now)/time.Second), 0)
		}
		items = append(items, orderItem)
		seen[name] = struct{}{}
	}

	sort.Strings(fileNames)
	for _, name := range fileNames {
		file := files[name]
		if _, ok := seen[name]; ok || strings.EqualFold(filepath.Base(name), "manifest.json") || strings.ToLower(filepath.Ext(name)) != ".json" {
			continue
		}
		payload, readErr := readZIPJSON(file)
		if readErr != nil {
			return nil, nil, readErr
		}
		if !looksLikeCPAAccount(payload) {
			continue
		}
		accounts = append(accounts, append(json.RawMessage(nil), payload...))
		items = append(items, OrderItem{})
	}
	if len(accounts) == 0 {
		return nil, nil, errors.New("supply CPA ZIP did not include importable account JSON files")
	}
	return accounts, items, nil
}

func normalizedZIPName(name string) string {
	name = filepath.ToSlash(strings.TrimSpace(name))
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, '\x00') {
		return ""
	}
	name = strings.TrimPrefix(name, "./")
	if name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
		return ""
	}
	return name
}

func readZIPJSON(file *zip.File) ([]byte, error) {
	if file == nil {
		return nil, errors.New("supply CPA ZIP entry is missing")
	}
	if file.UncompressedSize64 > maxResponseBodyBytes {
		return nil, fmt.Errorf("supply CPA ZIP entry %s exceeded size limit", filepath.Base(file.Name))
	}
	entry, err := file.Open()
	if err != nil {
		return nil, err
	}
	payload, readErr := io.ReadAll(io.LimitReader(entry, maxResponseBodyBytes+1))
	closeErr := entry.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || len(payload) > maxResponseBodyBytes || !json.Valid(payload) {
		return nil, fmt.Errorf("supply CPA ZIP entry %s is not valid JSON", filepath.Base(file.Name))
	}
	return payload, nil
}

func looksLikeCPAAccount(payload []byte) bool {
	var value map[string]any
	if err := json.Unmarshal(payload, &value); err != nil {
		return false
	}
	if _, ok := value["accounts"]; ok {
		return true
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "account", "email", "type", "provider"} {
		if _, ok := value[key]; ok {
			return true
		}
	}
	return false
}

func (c *Client) Recoveries(ctx context.Context, credentials Credentials) ([]Recovery, error) {
	const pageLimit = 100
	const maximumPages = 100
	result := make([]Recovery, 0, pageLimit)
	beforeID := ""
	seenCursors := make(map[string]struct{})
	for pageIndex := 0; pageIndex < maximumPages; pageIndex++ {
		page, err := c.RecoveriesPage(ctx, credentials, beforeID, pageLimit)
		if err != nil {
			return nil, err
		}
		result = append(result, page.Recoveries...)
		next := strings.TrimSpace(page.NextBeforeID)
		if next == "" || len(page.Recoveries) == 0 {
			break
		}
		if _, duplicate := seenCursors[next]; duplicate {
			return nil, errors.New("supply recovery pagination returned a repeated next_before_id")
		}
		if pageIndex == maximumPages-1 {
			return nil, fmt.Errorf("supply recovery pagination exceeded %d pages", maximumPages)
		}
		seenCursors[next] = struct{}{}
		beforeID = next
	}
	return result, nil
}

func (c *Client) RecoveriesPage(ctx context.Context, credentials Credentials, beforeID string, limit int) (RecoveryPage, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	query := url.Values{}
	query.Set("limit", strconv.Itoa(limit))
	if strings.TrimSpace(beforeID) != "" {
		query.Set("before_id", strings.TrimSpace(beforeID))
	}
	value, _, err := c.doAuthenticated(ctx, credentials, http.MethodGet, "/api/customer/recoveries?"+query.Encode(), nil)
	if err != nil {
		return RecoveryPage{}, err
	}
	objects := recoveryObjects(value)
	recoveries := make([]Recovery, 0, len(objects))
	for _, object := range objects {
		recovery := parseRecovery(object)
		if recovery.ID == "" {
			continue
		}
		recoveries = append(recoveries, recovery)
	}
	return RecoveryPage{
		Recoveries:   recoveries,
		NextBeforeID: findString(value, "next_before_id", "nextBeforeId"),
	}, nil
}

func (c *Client) GetRecovery(ctx context.Context, credentials Credentials, recoveryID string, statusURL string) (Recovery, error) {
	endpoint := strings.TrimSpace(statusURL)
	if endpoint == "" {
		endpoint = "/api/customer/recoveries/" + url.PathEscape(strings.TrimSpace(recoveryID))
	}
	value, _, err := c.doAuthenticated(ctx, credentials, http.MethodGet, endpoint, nil)
	if err != nil {
		return Recovery{}, err
	}
	recovery := parseRecoveryValue(value)
	if recovery.ID == "" {
		recovery.ID = strings.TrimSpace(recoveryID)
	}
	return recovery, nil
}

func (c *Client) ClaimRecovery(ctx context.Context, credentials Credentials, recoveryID string, claimURL string, claimTicket ...string) (RecoveryClaimResult, error) {
	endpoint := strings.TrimSpace(claimURL)
	if endpoint == "" {
		endpoint = "/api/customer/recoveries/" + url.PathEscape(strings.TrimSpace(recoveryID)) + "/claim"
	}
	ticket := ""
	if len(claimTicket) > 0 {
		ticket = strings.TrimSpace(claimTicket[0])
	}
	if parsed, err := url.Parse(endpoint); err == nil {
		if ticket == "" {
			ticket = strings.TrimSpace(parsed.Query().Get("ticket"))
		}
		query := parsed.Query()
		if strings.EqualFold(strings.TrimSpace(credentials.PlatformType), "bugteam") {
			query.Del("ticket")
		} else if ticket != "" {
			// The legacy supplier contract signs claim_url with a ticket query
			// parameter. Keep it on the request while also sending the header for
			// suppliers that accept the newer transport.
			query.Set("ticket", ticket)
		}
		parsed.RawQuery = query.Encode()
		endpoint = parsed.String()
	}
	headers := make(http.Header)
	headers.Set("Accept", "application/json")
	headers.Set("Idempotency-Key", recoveryClaimIdempotencyKey(recoveryID))
	if ticket != "" {
		headers.Set("X-Recovery-Ticket", ticket)
	}
	value, _, err := c.doAuthenticatedWithHeaders(ctx, credentials, http.MethodPost, endpoint, nil, headers, c.takeTimeout)
	if err != nil {
		return RecoveryClaimResult{}, err
	}
	recovery := parseRecoveryValue(value)
	if recovery.ID == "" {
		recovery.ID = strings.TrimSpace(recoveryID)
	}
	return RecoveryClaimResult{
		Recovery:          recovery,
		Accounts:          recoveryClaimAccounts(value),
		CredentialVersion: int(findInt64(value, "credential_version", "credentialVersion")),
	}, nil
}

func (c *Client) NvtokensChallenge(ctx context.Context, credentials Credentials) (NvtokensChallengeConfig, error) {
	if c == nil || !isNvtokens(credentials) {
		return NvtokensChallengeConfig{}, errors.New("supply platform is not nvtokens")
	}
	client, err := c.newIsolatedClient()
	if err != nil {
		return NvtokensChallengeConfig{}, err
	}
	value, _, err := client.request(ctx, credentials.BaseURL, http.MethodGet, "/api/auth/challenge-config", nil, tokenState{})
	if err != nil {
		return NvtokensChallengeConfig{}, err
	}
	root := primaryObject(value)
	return NvtokensChallengeConfig{
		Provider:                  stringValue(root, "provider"),
		SiteKey:                   stringValue(root, "site_key", "siteKey"),
		TestBypass:                boolValue(root, "test_bypass", "testBypass"),
		EmailVerificationRequired: boolValue(root, "email_verification_required", "emailVerificationRequired"),
	}, nil
}

func (c *Client) LoginNvtokensWithChallenge(ctx context.Context, credentials Credentials, challengeToken string) (string, error) {
	if c == nil || !isNvtokens(credentials) {
		return "", errors.New("supply platform is not nvtokens")
	}
	challengeToken = strings.TrimSpace(challengeToken)
	if challengeToken == "" {
		return "", errors.New("nvtokens challenge token is empty")
	}
	client, err := c.newIsolatedClient()
	if err != nil {
		return "", err
	}
	payload := map[string]any{
		"username":              strings.TrimSpace(credentials.Username),
		"password":              credentials.Password,
		"cf-turnstile-response": challengeToken,
	}
	value, _, err := client.request(ctx, credentials.BaseURL, http.MethodPost, "/api/login", payload, tokenState{})
	if err != nil {
		return "", normalizeNvtokensLoginError(err)
	}
	session := client.nvtokensSessionCookie(credentials.BaseURL)
	if session == "" {
		session = findString(value, "scm_session", "session", "session_token", "sessionToken", "token", "access_token", "accessToken")
	}
	if session == "" {
		return "", errors.New("nvtokens login response did not set a recognized session cookie")
	}
	if err := client.ValidateNvtokensSession(ctx, credentials, session); err != nil {
		return "", fmt.Errorf("validate refreshed nvtokens session: %w", err)
	}
	return normalizeNvtokensSession(session), nil
}

func (c *Client) ValidateNvtokensSession(ctx context.Context, credentials Credentials, session string) error {
	if c == nil || !isNvtokens(credentials) {
		return errors.New("supply platform is not nvtokens")
	}
	session = normalizeNvtokensSession(session)
	if session == "" {
		return errors.New("nvtokens session is empty")
	}
	client, err := c.newIsolatedClient()
	if err != nil {
		return err
	}
	auth := tokenState{
		token:      session,
		header:     customerTokenHeader,
		cookie:     nvtokensSessionCookieHeader(session),
		cookieAuth: true,
	}
	_, _, err = client.request(ctx, credentials.BaseURL, http.MethodGet, "/api/me", nil, auth)
	return err
}

func (c *Client) newIsolatedClient() (*Client, error) {
	if c == nil || c.httpClient == nil {
		return nil, errors.New("supply HTTP client is unavailable")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	clientCopy := *c.httpClient
	clientCopy.Jar = jar
	return &Client{
		httpClient:      &clientCopy,
		timeout:         c.timeout,
		takeTimeout:     c.takeTimeout,
		tokens:          make(map[string]tokenState),
		nvtokensResults: make(map[string]TakeResult),
	}, nil
}

func (c *Client) nvtokensSessionCookie(baseURL string) string {
	if c == nil || c.httpClient == nil || c.httpClient.Jar == nil {
		return ""
	}
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/api/me")
	if err != nil {
		return ""
	}
	cookies := c.httpClient.Jar.Cookies(parsed)
	for _, candidate := range []string{nvtokensSessionCookie, nvtokensLegacyCookie} {
		for _, cookie := range cookies {
			if strings.EqualFold(cookie.Name, candidate) {
				return strings.TrimSpace(cookie.Value)
			}
		}
	}
	return ""
}

func nvtokensSessionCookieHeader(session string) string {
	session = normalizeNvtokensSession(session)
	if session == "" {
		return ""
	}
	// Current NexusVault deployments use scm_session. Keep the former session
	// name alongside it so older NV installations continue to authenticate.
	return (&http.Cookie{Name: nvtokensSessionCookie, Value: session}).String() + "; " +
		(&http.Cookie{Name: nvtokensLegacyCookie, Value: session}).String()
}

func recoveryClaimIdempotencyKey(recoveryID string) string {
	recoveryID = strings.TrimSpace(recoveryID)
	if recoveryID == "" {
		return "cpam-recovery-claim"
	}
	return "cpam-recovery-" + recoveryID
}

func (c *Client) doAuthenticated(ctx context.Context, credentials Credentials, method string, path string, body any) (any, int, error) {
	return c.doAuthenticatedWithHeaders(ctx, credentials, method, path, body, nil, c.timeout)
}

func (c *Client) doAuthenticatedWithTimeout(ctx context.Context, credentials Credentials, method string, path string, body any, requestTimeout time.Duration) (any, int, error) {
	return c.doAuthenticatedWithHeaders(ctx, credentials, method, path, body, nil, requestTimeout)
}

func (c *Client) doAuthenticatedWithHeaders(ctx context.Context, credentials Credentials, method string, path string, body any, headers http.Header, requestTimeout time.Duration) (any, int, error) {
	auth, err := c.login(ctx, credentials, false)
	if err != nil {
		refreshedCredentials, refreshedAuth, refreshErr := c.refreshNvtokensAuthentication(ctx, credentials)
		if refreshErr != nil {
			if errors.Is(refreshErr, ErrNvtokensSessionRefreshUnavailable) {
				return nil, 0, err
			}
			return nil, 0, refreshErr
		}
		credentials = refreshedCredentials
		auth = refreshedAuth
	}
	value, status, err := c.requestWithHeaders(ctx, credentials.BaseURL, method, path, body, auth, headers, requestTimeout)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized {
		return value, status, err
	}
	if isNvtokens(credentials) {
		refreshedCredentials, refreshedAuth, refreshErr := c.refreshNvtokensAuthentication(ctx, credentials)
		if refreshErr == nil {
			return c.requestWithHeaders(ctx, refreshedCredentials.BaseURL, method, path, body, refreshedAuth, headers, requestTimeout)
		}
		if !errors.Is(refreshErr, ErrNvtokensSessionRefreshUnavailable) {
			return nil, 0, refreshErr
		}
	}
	if !canRefreshAuthentication(credentials) {
		return value, status, err
	}
	c.invalidate(credentials)
	auth, err = c.login(ctx, credentials, true)
	if err != nil {
		return nil, 0, err
	}
	return c.requestWithHeaders(ctx, credentials.BaseURL, method, path, body, auth, headers, requestTimeout)
}

func (c *Client) refreshNvtokensAuthentication(ctx context.Context, credentials Credentials) (Credentials, tokenState, error) {
	if c == nil || !isNvtokens(credentials) {
		return credentials, tokenState{}, ErrNvtokensSessionRefreshUnavailable
	}
	c.refresherMu.RLock()
	refresher := c.refresher
	c.refresherMu.RUnlock()
	if refresher == nil {
		return credentials, tokenState{}, ErrNvtokensSessionRefreshUnavailable
	}
	session, err := refresher(ctx, credentials)
	if err != nil {
		return credentials, tokenState{}, err
	}
	session = normalizeNvtokensSession(session)
	if session == "" {
		return credentials, tokenState{}, errors.New("nvtokens automatic session refresh returned an empty session")
	}
	c.invalidate(credentials)
	credentials.Token = session
	auth, err := c.login(ctx, credentials, false)
	return credentials, auth, err
}

func (c *Client) login(ctx context.Context, credentials Credentials, force bool) (tokenState, error) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	key := credentialKey(credentials)
	c.mu.Lock()
	if state, ok := c.tokens[key]; !force && ok && (state.token != "" || state.cookieAuth) && time.Now().Before(state.expiresAt) {
		c.token = state
		c.mu.Unlock()
		return state, nil
	}
	c.mu.Unlock()
	if isNvtokens(credentials) {
		// A configured nvtokens token is a browser session snapshot. Prefer it on
		// the first request so deployments that only provide a session keep
		// working, but do not reuse it after the supplier has rejected it. When
		// password credentials are available, a forced refresh must establish a
		// fresh HttpOnly session through /api/login.
		if token := strings.TrimSpace(credentials.Token); token != "" && (!force || !canPasswordLogin(credentials)) {
			cookieValue := token
			if strings.HasPrefix(strings.ToLower(cookieValue), "session=") {
				cookieValue = strings.TrimSpace(strings.SplitN(cookieValue, "=", 2)[1])
			}
			state := tokenState{
				key:        key,
				token:      cookieValue,
				header:     customerTokenHeader,
				cookie:     nvtokensSessionCookieHeader(cookieValue),
				cookieAuth: true,
				expiresAt:  time.Now().Add(12 * time.Hour),
			}
			c.mu.Lock()
			c.tokens[key] = state
			c.token = state
			c.mu.Unlock()
			return state, nil
		}
		payload := map[string]any{
			"username": strings.TrimSpace(credentials.Username),
			"password": credentials.Password,
		}
		value, _, err := c.request(ctx, credentials.BaseURL, http.MethodPost, "/api/login", payload, tokenState{})
		if err != nil {
			return tokenState{}, normalizeNvtokensLoginError(err)
		}
		// The web client uses the HttpOnly session cookie set by this endpoint.
		// A token field is accepted as a fallback for deployments that expose an
		// API token in addition to the browser session.
		token := findString(value, "token", "access_token", "accessToken")
		state := tokenState{
			key:        key,
			token:      token,
			header:     customerTokenHeader,
			cookieAuth: true,
			expiresAt:  time.Now().Add(12 * time.Hour),
		}
		c.mu.Lock()
		c.tokens[key] = state
		c.token = state
		c.mu.Unlock()
		return state, nil
	}
	if token := strings.TrimSpace(credentials.Token); token != "" && (!force || !canPasswordLogin(credentials)) {
		return tokenState{key: key, token: token, header: customerTokenHeader}, nil
	}
	payload := map[string]any{"password": credentials.Password}
	if strings.EqualFold(strings.TrimSpace(credentials.PlatformType), "bugteam") {
		payload["account"] = strings.TrimSpace(credentials.Username)
	} else {
		payload["username"] = strings.TrimSpace(credentials.Username)
	}
	value, _, err := c.request(ctx, credentials.BaseURL, http.MethodPost, "/api/customer/login", payload, tokenState{})
	if err != nil {
		return tokenState{}, err
	}
	header := customerTokenHeader
	token := findString(value, "token", "access_token", "accessToken")
	if token == "" && strings.EqualFold(strings.TrimSpace(credentials.PlatformType), "bugteam") {
		token = findString(value, "session", "customer_session", "customerSession")
		header = customerSessionHeader
	}
	if token == "" {
		return tokenState{}, errors.New("supply login response did not include token or session")
	}
	state := tokenState{key: key, token: token, header: header, expiresAt: time.Now().Add(29 * 24 * time.Hour)}
	c.mu.Lock()
	c.tokens[key] = state
	c.token = state
	c.mu.Unlock()
	return state, nil
}

func (c *Client) invalidate(credentials Credentials) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := credentialKey(credentials)
	delete(c.tokens, key)
	if c.token.key == key {
		c.token = tokenState{}
	}
}

func (c *Client) Invalidate(credentials Credentials) {
	if c == nil {
		return
	}
	c.invalidate(credentials)
}

func normalizeNvtokensSession(value string) string {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, nvtokensSessionCookie+"=") || strings.HasPrefix(lower, nvtokensLegacyCookie+"=") {
		value = strings.TrimSpace(strings.SplitN(value, "=", 2)[1])
	}
	return value
}

func canPasswordLogin(credentials Credentials) bool {
	return strings.TrimSpace(credentials.Username) != "" && credentials.Password != ""
}

func canRefreshAuthentication(credentials Credentials) bool {
	if strings.TrimSpace(credentials.Token) == "" {
		return canPasswordLogin(credentials)
	}
	platformType := strings.TrimSpace(credentials.PlatformType)
	return (strings.EqualFold(platformType, "bugteam") || isNvtokens(credentials)) && canPasswordLogin(credentials)
}

func normalizeNvtokensLoginError(err error) error {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest ||
		!strings.Contains(strings.TrimSpace(httpErr.Message), "人机验证") {
		return err
	}
	return &HTTPError{
		StatusCode: http.StatusUnauthorized,
		Code:       "AUTH_REQUIRED",
		Message:    "NV 登录态已失效，密码续登需要完成人机验证；请登录 nvtokens 后更新 Session",
	}
}

func applyAuthentication(headers http.Header, auth tokenState) {
	if strings.TrimSpace(auth.cookie) != "" {
		headers.Set("Cookie", auth.cookie)
	}
	if strings.TrimSpace(auth.token) == "" {
		return
	}
	header := strings.TrimSpace(auth.header)
	if header == "" {
		header = customerTokenHeader
	}
	headers.Set(header, auth.token)
}

func (c *Client) request(ctx context.Context, baseURL string, method string, endpointRef string, body any, auth tokenState) (any, int, error) {
	return c.requestWithTimeout(ctx, baseURL, method, endpointRef, body, auth, c.timeout)
}

func (c *Client) requestWithTimeout(ctx context.Context, baseURL string, method string, endpointRef string, body any, auth tokenState, requestTimeout time.Duration) (any, int, error) {
	return c.requestWithHeaders(ctx, baseURL, method, endpointRef, body, auth, nil, requestTimeout)
}

func (c *Client) requestWithHeaders(ctx context.Context, baseURL string, method string, endpointRef string, body any, auth tokenState, headers http.Header, requestTimeout time.Duration) (any, int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(data)
	}
	if requestTimeout <= 0 {
		requestTimeout = c.timeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	endpoint, err := resolveEndpoint(baseURL, endpointRef)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(reqCtx, method, endpoint, reader)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; CPA-Manager/1.0)")
	applyAuthentication(req.Header, auth)
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, endpointRef, err)
	}
	defer res.Body.Close()
	limited := &io.LimitedReader{R: res.Body, N: maxResponseBodyBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, res.StatusCode, err
	}
	if limited.N == 0 {
		return nil, res.StatusCode, errors.New("supply API response exceeded size limit")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, res.StatusCode, &HTTPError{
			StatusCode:        res.StatusCode,
			Message:           errorMessage(data),
			Code:              errorCode(data),
			RetryAfterSeconds: responseRetryAfterSeconds(data, res.Header.Get("Retry-After"), time.Now()),
		}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, res.StatusCode, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, res.StatusCode, fmt.Errorf("decode supply API response: %w", err)
	}
	return value, res.StatusCode, nil
}

func parseOrder(root map[string]any) Order {
	return Order{
		ID:                stringValue(root, "id", "order_id", "orderId"),
		Status:            strings.ToLower(stringValue(root, "status", "state")),
		Product:           stringValue(root, "product"),
		Quantity:          intValue(root, "quantity", "requested_quantity", "requestedQuantity"),
		ReadyQuantity:     intValue(root, "ready_quantity", "readyQuantity", "delivered_quantity", "deliveredQuantity", "available"),
		Progress:          intValue(root, "progress", "progress_percent", "progressPercent"),
		ChargedFen:        int64Value(root, "charged_fen", "chargedFen", "buyer_total_cents", "buyerTotalCents", "amount_cents", "amountCents"),
		ReleasedFen:       int64Value(root, "released_fen", "releasedFen"),
		RetryAfterSeconds: intValue(root, "retry_after_seconds", "retryAfterSeconds", "retry_after", "retryAfter"),
		StatusURL:         stringValue(root, "status_url", "statusUrl"),
		TakeURL:           stringValue(root, "take_url", "takeUrl"),
	}
}

func parseOrderValue(value any) Order {
	root, _ := value.(map[string]any)
	if root == nil {
		return Order{}
	}
	maps := []map[string]any{root}
	current := root
	for {
		var next map[string]any
		for _, key := range []string{"data", "payload", "result"} {
			if child, ok := current[key].(map[string]any); ok {
				next = child
				break
			}
		}
		if next == nil {
			break
		}
		maps = append(maps, next)
		current = next
	}
	if nested, ok := current["order"].(map[string]any); ok {
		maps = append(maps, nested)
	} else if nested, ok := root["order"].(map[string]any); ok {
		maps = append(maps, nested)
	}

	var order Order
	for index := len(maps) - 1; index >= 0; index-- {
		mergeOrder(&order, parseOrder(maps[index]))
	}
	return order
}

func mergeOrder(target *Order, candidate Order) {
	if target.ID == "" {
		target.ID = candidate.ID
	}
	if target.Status == "" {
		target.Status = candidate.Status
	}
	if target.Product == "" {
		target.Product = candidate.Product
	}
	if target.Quantity == 0 {
		target.Quantity = candidate.Quantity
	}
	if target.ReadyQuantity == 0 {
		target.ReadyQuantity = candidate.ReadyQuantity
	}
	if target.Progress == 0 {
		target.Progress = candidate.Progress
	}
	if target.ChargedFen == 0 {
		target.ChargedFen = candidate.ChargedFen
	}
	if target.ReleasedFen == 0 {
		target.ReleasedFen = candidate.ReleasedFen
	}
	if target.RetryAfterSeconds == 0 {
		target.RetryAfterSeconds = candidate.RetryAfterSeconds
	}
	if target.StatusURL == "" {
		target.StatusURL = candidate.StatusURL
	}
	if target.TakeURL == "" {
		target.TakeURL = candidate.TakeURL
	}
}

func rawAccounts(value any) []json.RawMessage {
	var find func(any) []json.RawMessage
	find = func(current any) []json.RawMessage {
		switch typed := current.(type) {
		case map[string]any:
			for _, key := range []string{"accounts", "items"} {
				if list, ok := typed[key].([]any); ok {
					result := make([]json.RawMessage, 0, len(list))
					for _, item := range list {
						data, err := json.Marshal(item)
						if err == nil && len(data) > 0 {
							result = append(result, data)
						}
					}
					return result
				}
			}
			for _, key := range []string{"payload", "data", "result"} {
				if child, ok := typed[key]; ok {
					if result := find(child); len(result) > 0 {
						return result
					}
				}
			}
		}
		return nil
	}
	return find(value)
}

func recoveryObjects(value any) []map[string]any {
	switch typed := value.(type) {
	case []any:
		result := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if object, ok := item.(map[string]any); ok {
				result = append(result, object)
			}
		}
		return result
	case map[string]any:
		for _, key := range []string{"recoveries", "items", "data", "payload", "result"} {
			child, ok := typed[key]
			if !ok || child == nil {
				continue
			}
			if result := recoveryObjects(child); len(result) > 0 {
				return result
			}
		}
		if stringValue(typed, "id", "recovery_id", "recoveryId") != "" || stringValue(typed, "claim_url", "claimUrl") != "" {
			return []map[string]any{typed}
		}
	}
	return nil
}

func parseRecoveryValue(value any) Recovery {
	root, _ := value.(map[string]any)
	if root == nil {
		return Recovery{}
	}
	for _, key := range []string{"recovery", "data", "payload", "result"} {
		if child, ok := root[key].(map[string]any); ok {
			if recovery := parseRecoveryValue(child); recovery.ID != "" || recovery.DeliveryStatus != "" || recovery.ClaimURL != "" {
				return recovery
			}
		}
	}
	return parseRecovery(root)
}

func parseRecovery(root map[string]any) Recovery {
	if root == nil {
		return Recovery{}
	}
	raw, _ := json.Marshal(root)
	claimURL := stringValue(root, "claim_url", "claimUrl")
	id := stringValue(root, "id", "recovery_id", "recoveryId")
	if id == "" {
		id = recoveryIDFromClaimURL(claimURL)
	}
	return Recovery{
		ID:                id,
		DeliveryStatus:    strings.ToLower(stringValue(root, "delivery_status", "deliveryStatus", "status")),
		Product:           stringValue(root, "product"),
		SourceOrderID:     stringValue(root, "source_order_id", "sourceOrderId"),
		OriginalEmail:     stringValue(root, "original_email", "originalEmail", "account_email", "accountEmail", "email"),
		OriginalAccount:   stringValue(root, "original_account", "originalAccount", "auth_file_name", "authFileName", "file_name", "fileName", "account"),
		OriginalAuthIndex: stringValue(root, "original_auth_index", "originalAuthIndex", "auth_index", "authIndex"),
		ClaimURL:          claimURL,
		ClaimTicket:       stringValue(root, "claim_ticket", "claimTicket"),
		StatusURL:         stringValue(root, "status_url", "statusUrl"),
		CredentialVersion: int(int64Value(root, "credential_version", "credentialVersion")),
		RefundedFen:       int64Value(root, "refunded_fen", "refundedFen", "refund_fen", "refundFen"),
		Raw:               raw,
	}
}

func replacementFiles(value any) []ReplacementFile {
	root, _ := value.(map[string]any)
	if root == nil {
		return nil
	}
	if values, ok := root["replacement_files"].([]any); ok {
		return parseReplacementFiles(values)
	}
	if values, ok := root["replacementFiles"].([]any); ok {
		return parseReplacementFiles(values)
	}
	for _, key := range []string{"payload", "data", "result", "order"} {
		if child, ok := root[key]; ok {
			if result := replacementFiles(child); len(result) > 0 {
				return result
			}
		}
	}
	return nil
}

func parseReplacementFiles(values []any) []ReplacementFile {
	result := make([]ReplacementFile, 0, len(values))
	for _, value := range values {
		object, ok := value.(map[string]any)
		if !ok {
			continue
		}
		raw, _ := json.Marshal(object)
		claimURL := stringValue(object, "claim_url", "claimUrl")
		recoveryID := stringValue(object, "recovery_id", "recoveryId", "id")
		if recoveryID == "" {
			recoveryID = recoveryIDFromClaimURL(claimURL)
		}
		result = append(result, ReplacementFile{
			RecoveryID:        recoveryID,
			Ready:             boolValue(object, "ready") || strings.EqualFold(stringValue(object, "delivery_status", "deliveryStatus", "status"), "claimable"),
			StatusURL:         stringValue(object, "status_url", "statusUrl"),
			ClaimURL:          claimURL,
			ClaimTicket:       stringValue(object, "claim_ticket", "claimTicket"),
			CredentialVersion: int(int64Value(object, "credential_version", "credentialVersion")),
			Product:           stringValue(object, "product"),
			SourceOrderID:     stringValue(object, "source_order_id", "sourceOrderId"),
			OriginalEmail:     stringValue(object, "original_email", "originalEmail", "email"),
			OriginalAccount:   stringValue(object, "original_account", "originalAccount", "auth_file_name", "authFileName", "file_name", "fileName"),
			OriginalAuthIndex: stringValue(object, "original_auth_index", "originalAuthIndex", "auth_index", "authIndex"),
			Raw:               raw,
		})
	}
	return result
}

func recoveryClaimAccounts(value any) []json.RawMessage {
	if accounts := rawAccounts(value); len(accounts) > 0 {
		return accounts
	}
	root, _ := value.(map[string]any)
	if root == nil {
		return nil
	}
	if looksLikeCredentialPayload(root) {
		data, err := json.Marshal(root)
		if err == nil && len(data) > 0 {
			return []json.RawMessage{data}
		}
	}
	for _, key := range []string{"payload", "data", "result"} {
		child, ok := root[key]
		if !ok || child == nil {
			continue
		}
		if object, ok := child.(map[string]any); ok && looksLikeCredentialPayload(object) {
			data, err := json.Marshal(object)
			if err == nil && len(data) > 0 {
				return []json.RawMessage{data}
			}
		}
	}
	return nil
}

func looksLikeCredentialPayload(value map[string]any) bool {
	if _, ok := value["credentials"].(map[string]any); ok {
		return true
	}
	return stringValue(value, "access_token", "accessToken", "session_access_token", "sessionAccessToken") != ""
}

func recoveryIDFromClaimURL(claimURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(claimURL))
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for index := 0; index < len(parts)-1; index++ {
		if parts[index] == "recoveries" && parts[index+1] != "" {
			return parts[index+1]
		}
	}
	return ""
}

// orderItems reads the supplier's per-delivery validity and price fields from
// order.items. It intentionally does not inspect arbitrary "items" arrays:
// account payloads may use that name too, and treating those as order items
// would incorrectly assign leases or costs to imported accounts.
func orderItems(value any) []OrderItem {
	root, _ := value.(map[string]any)
	if root == nil {
		return nil
	}
	return findOrderItems(root)
}

func findOrderItems(root map[string]any) []OrderItem {
	if order, ok := root["order"].(map[string]any); ok {
		if items, found := parseOrderItems(order["items"]); found {
			return items
		}
	}
	if items, found := parseOrderItems(root["items"]); found && orderLike(root) {
		return items
	}
	for _, key := range []string{"data", "payload", "result"} {
		if child, ok := root[key].(map[string]any); ok {
			if items := findOrderItems(child); len(items) > 0 {
				return items
			}
		}
	}
	return nil
}

func orderLike(value map[string]any) bool {
	if stringValue(value, "id", "order_id", "orderId") != "" {
		return true
	}
	return stringValue(value, "product") != "" && int64Value(value, "quantity", "requested_quantity", "requestedQuantity") > 0
}

func parseOrderItems(value any) ([]OrderItem, bool) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil, false
	}
	result := make([]OrderItem, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		remainingSeconds, hasRemaining := int64ValueOK(object, "remaining_seconds", "remainingSeconds")
		basePriceFen, hasBasePrice := int64ValueOK(object, "base_price_fen", "basePriceFen")
		chargedFen, hasCharged := int64ValueOK(object, "charged_fen", "chargedFen")
		if !hasRemaining && !hasBasePrice && !hasCharged {
			return nil, false
		}
		result = append(result, OrderItem{
			RemainingSeconds: remainingSeconds,
			HasRemaining:     hasRemaining,
			BasePriceFen:     basePriceFen,
			ChargedFen:       chargedFen,
		})
	}
	return result, true
}

func orderItemRemainingSeconds(items []OrderItem) []int64 {
	if len(items) == 0 {
		return nil
	}
	remaining := make([]int64, 0, len(items))
	for _, item := range items {
		if !item.HasRemaining {
			return nil
		}
		remaining = append(remaining, item.RemainingSeconds)
	}
	return remaining
}

func primaryObject(value any) map[string]any {
	root, _ := value.(map[string]any)
	for _, key := range []string{"data", "payload", "result"} {
		if child, ok := root[key].(map[string]any); ok {
			if order, exists := child["order"].(map[string]any); exists {
				return order
			}
			return child
		}
	}
	return root
}

func nestedObject(root map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if child, ok := root[key].(map[string]any); ok {
			return child
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if text := strings.TrimSpace(value); text != "" {
			return text
		}
	}
	return ""
}

func firstString(values ...string) string {
	return firstNonEmpty(values...)
}

func firstInt64(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func maxInt(value, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
}

func findString(value any, keys ...string) string {
	if root, ok := value.(map[string]any); ok {
		if result := stringValue(root, keys...); result != "" {
			return result
		}
		for _, key := range []string{"data", "payload", "result", "error", "recovery"} {
			if child, exists := root[key]; exists {
				if result := findString(child, keys...); result != "" {
					return result
				}
			}
		}
	}
	return ""
}

func findInt64(value any, keys ...string) int64 {
	if root, ok := value.(map[string]any); ok {
		if result, found := int64ValueOK(root, keys...); found && result != 0 {
			return result
		}
		for _, key := range []string{"data", "payload", "result", "recovery"} {
			if child, exists := root[key]; exists {
				if result := findInt64(child, keys...); result != 0 {
					return result
				}
			}
		}
	}
	return 0
}

func stringValue(root map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := root[key]; ok && value != nil {
			text := strings.TrimSpace(fmt.Sprint(value))
			if text != "" && text != "<nil>" {
				return text
			}
		}
	}
	return ""
}

func intValue(root map[string]any, keys ...string) int { return int(int64Value(root, keys...)) }

func int64Value(root map[string]any, keys ...string) int64 {
	value, _ := int64ValueOK(root, keys...)
	return value
}

func int64ValueOK(root map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		value, ok := root[key]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case json.Number:
			if result, err := typed.Int64(); err == nil {
				return result, true
			}
			if result, err := typed.Float64(); err == nil {
				return int64(result), true
			}
		case float64:
			return int64(typed), true
		case int:
			return int64(typed), true
		case int64:
			return typed, true
		case string:
			if result, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
				return int64(result), true
			}
		}
	}
	return 0, false
}

func float64ValueOK(root map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		value, ok := root[key]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case json.Number:
			result, err := typed.Float64()
			return result, err == nil
		case float64:
			return typed, true
		case int:
			return float64(typed), true
		case int64:
			return float64(typed), true
		case string:
			result, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
			return result, err == nil
		}
	}
	return 0, false
}

func boolValue(root map[string]any, keys ...string) bool {
	for _, key := range keys {
		value, ok := root[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case bool:
			return typed
		case string:
			result, _ := strconv.ParseBool(strings.TrimSpace(typed))
			return result
		case json.Number:
			result, _ := typed.Int64()
			return result != 0
		}
	}
	return false
}

func hasBoolField(root map[string]any, keys ...string) bool {
	for _, key := range keys {
		if _, ok := root[key]; ok {
			return true
		}
	}
	return false
}

func errorMessage(data []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) == nil {
		if message := findString(value, "message", "detail", "error_description", "errorDescription"); message != "" {
			return message
		}
		if root, ok := value.(map[string]any); ok {
			if message, ok := root["error"].(string); ok && strings.TrimSpace(message) != "" {
				return strings.TrimSpace(message)
			}
		}
		if code := findString(value, "code", "error_code", "errorCode"); code != "" {
			return code
		}
	}
	message := strings.TrimSpace(string(data))
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

func errorCode(data []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return ""
	}
	return findString(value, "code", "error_code", "errorCode")
}

func retryAfterSeconds(value string, now time.Time) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return max(seconds, 0)
	}
	deadline, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	seconds := int(math.Ceil(deadline.Sub(now).Seconds()))
	return max(seconds, 0)
}

func responseRetryAfterSeconds(data []byte, header string, now time.Time) int {
	if seconds := retryAfterSeconds(header, now); seconds > 0 {
		return seconds
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return 0
	}
	return int(findInt64(value, "retry_after_seconds", "retryAfterSeconds", "retry_after", "retryAfter"))
}

func credentialKey(credentials Credentials) string {
	return strings.TrimSpace(credentials.ID) + "\x00" + strings.ToLower(strings.TrimSpace(credentials.PlatformType)) + "\x00" +
		strings.TrimRight(strings.TrimSpace(credentials.BaseURL), "/") + "\x00" + strings.TrimSpace(credentials.Username)
}

func resolveEndpoint(baseURL string, endpointRef string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return "", errors.New("supply base URL is invalid")
	}
	reference, err := url.Parse(strings.TrimSpace(endpointRef))
	if err != nil || strings.TrimSpace(endpointRef) == "" {
		return "", errors.New("supply endpoint URL is invalid")
	}
	endpoint := base.ResolveReference(reference)
	if endpoint.User != nil || !sameOrigin(base, endpoint) {
		return "", errors.New("supply endpoint URL must use the configured base URL origin")
	}
	endpoint.Fragment = ""
	return endpoint.String(), nil
}

func sameOrigin(left *url.URL, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) &&
		effectivePort(left) == effectivePort(right)
}

func effectivePort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	if strings.EqualFold(value.Scheme, "https") {
		return "443"
	}
	return "80"
}
