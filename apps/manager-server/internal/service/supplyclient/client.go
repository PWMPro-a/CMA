package supplyclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeout       = 30 * time.Second
	defaultTakeTimeout   = 3 * time.Minute
	maxResponseBodyBytes = 16 * 1024 * 1024
)

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
	BaseURL  string
	Username string
	Password string
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
	OriginalEmail     string          `json:"originalEmail,omitempty"`
	OriginalAccount   string          `json:"originalAccount,omitempty"`
	OriginalAuthIndex string          `json:"originalAuthIndex,omitempty"`
	ClaimURL          string          `json:"claimUrl,omitempty"`
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
	CredentialVersion int
	Product           string
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
	key       string
	token     string
	expiresAt time.Time
}

type Client struct {
	httpClient  *http.Client
	timeout     time.Duration
	takeTimeout time.Duration
	mu          sync.Mutex
	token       tokenState
}

func New(httpClient *http.Client, timeout ...time.Duration) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	requestTimeout := defaultTimeout
	if len(timeout) > 0 && timeout[0] > 0 {
		requestTimeout = timeout[0]
	}
	takeTimeout := defaultTakeTimeout
	if requestTimeout > takeTimeout {
		takeTimeout = requestTimeout
	}
	return &Client{httpClient: httpClient, timeout: requestTimeout, takeTimeout: takeTimeout}
}

// DefaultTakeTimeout is intentionally longer than the normal API timeout.
// Taking an order may require the supplier to prepare and serialize multiple
// OAuth account files; inventory and status operations must stay short.
func DefaultTakeTimeout() time.Duration { return defaultTakeTimeout }

func (c *Client) Inventory(ctx context.Context, credentials Credentials, product string, quantity int) (Inventory, error) {
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

func (c *Client) CreateOrder(ctx context.Context, credentials Credentials, product string, quantity int, idempotencyKey ...string) (Order, error) {
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

func (c *Client) ClaimRecovery(ctx context.Context, credentials Credentials, recoveryID string, claimURL string) (RecoveryClaimResult, error) {
	endpoint := strings.TrimSpace(claimURL)
	if endpoint == "" {
		endpoint = "/api/customer/recoveries/" + url.PathEscape(strings.TrimSpace(recoveryID)) + "/claim"
	}
	headers := make(http.Header)
	headers.Set("Accept", "application/json")
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

func (c *Client) doAuthenticated(ctx context.Context, credentials Credentials, method string, path string, body any) (any, int, error) {
	return c.doAuthenticatedWithHeaders(ctx, credentials, method, path, body, nil, c.timeout)
}

func (c *Client) doAuthenticatedWithTimeout(ctx context.Context, credentials Credentials, method string, path string, body any, requestTimeout time.Duration) (any, int, error) {
	return c.doAuthenticatedWithHeaders(ctx, credentials, method, path, body, nil, requestTimeout)
}

func (c *Client) doAuthenticatedWithHeaders(ctx context.Context, credentials Credentials, method string, path string, body any, headers http.Header, requestTimeout time.Duration) (any, int, error) {
	token, err := c.login(ctx, credentials, false)
	if err != nil {
		return nil, 0, err
	}
	value, status, err := c.requestWithHeaders(ctx, credentials.BaseURL, method, path, body, token, headers, requestTimeout)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized {
		return value, status, err
	}
	c.invalidate(credentials)
	token, err = c.login(ctx, credentials, true)
	if err != nil {
		return nil, 0, err
	}
	return c.requestWithHeaders(ctx, credentials.BaseURL, method, path, body, token, headers, requestTimeout)
}

func (c *Client) login(ctx context.Context, credentials Credentials, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := credentialKey(credentials)
	if !force && c.token.key == key && c.token.token != "" && time.Now().Before(c.token.expiresAt) {
		return c.token.token, nil
	}
	value, _, err := c.request(ctx, credentials.BaseURL, http.MethodPost, "/api/customer/login", map[string]any{
		"username": strings.TrimSpace(credentials.Username),
		"password": credentials.Password,
	}, "")
	if err != nil {
		return "", err
	}
	token := findString(value, "token", "access_token", "accessToken")
	if token == "" {
		return "", errors.New("supply login response did not include token")
	}
	c.token = tokenState{key: key, token: token, expiresAt: time.Now().Add(29 * 24 * time.Hour)}
	return token, nil
}

func (c *Client) invalidate(credentials Credentials) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token.key == credentialKey(credentials) {
		c.token = tokenState{}
	}
}

func (c *Client) request(ctx context.Context, baseURL string, method string, endpointRef string, body any, token string) (any, int, error) {
	return c.requestWithTimeout(ctx, baseURL, method, endpointRef, body, token, c.timeout)
}

func (c *Client) requestWithTimeout(ctx context.Context, baseURL string, method string, endpointRef string, body any, token string, requestTimeout time.Duration) (any, int, error) {
	return c.requestWithHeaders(ctx, baseURL, method, endpointRef, body, token, nil, requestTimeout)
}

func (c *Client) requestWithHeaders(ctx context.Context, baseURL string, method string, endpointRef string, body any, token string, headers http.Header, requestTimeout time.Duration) (any, int, error) {
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
	if token != "" {
		req.Header.Set("X-Customer-Token", token)
	}
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
			RetryAfterSeconds: retryAfterSeconds(res.Header.Get("Retry-After"), time.Now()),
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
		Status:            strings.ToLower(stringValue(root, "status")),
		Product:           stringValue(root, "product"),
		Quantity:          intValue(root, "quantity", "requested_quantity", "requestedQuantity"),
		ReadyQuantity:     intValue(root, "ready_quantity", "readyQuantity", "available"),
		Progress:          intValue(root, "progress", "progress_percent", "progressPercent"),
		ChargedFen:        int64Value(root, "charged_fen", "chargedFen"),
		ReleasedFen:       int64Value(root, "released_fen", "releasedFen"),
		RetryAfterSeconds: intValue(root, "retry_after_seconds", "retryAfterSeconds"),
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
		OriginalEmail:     stringValue(root, "original_email", "originalEmail", "account_email", "accountEmail", "email"),
		OriginalAccount:   stringValue(root, "original_account", "originalAccount", "auth_file_name", "authFileName", "file_name", "fileName", "account"),
		OriginalAuthIndex: stringValue(root, "original_auth_index", "originalAuthIndex", "auth_index", "authIndex"),
		ClaimURL:          claimURL,
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
			CredentialVersion: int(int64Value(object, "credential_version", "credentialVersion")),
			Product:           stringValue(object, "product"),
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

func credentialKey(credentials Credentials) string {
	return strings.TrimRight(strings.TrimSpace(credentials.BaseURL), "/") + "\x00" + strings.TrimSpace(credentials.Username)
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
