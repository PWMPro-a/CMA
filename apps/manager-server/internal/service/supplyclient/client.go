package supplyclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

var ErrReleaseUnsupported = errors.New("supply order release endpoint is not supported")

type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("supply API returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("supply API returned HTTP %d: %s", e.StatusCode, e.Message)
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
	ItemRemainingSeconds []int64
	Pending              bool
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

func (c *Client) CreateOrder(ctx context.Context, credentials Credentials, product string, quantity int) (Order, error) {
	payload := map[string]any{"product": strings.TrimSpace(product), "quantity": quantity}
	value, _, err := c.doAuthenticated(ctx, credentials, http.MethodPost, "/api/customer/pickup/orders", payload)
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
	value, status, err := c.doAuthenticatedWithTimeout(ctx, credentials, http.MethodPost, endpoint, map[string]any{}, c.takeTimeout)
	if err != nil {
		return TakeResult{}, err
	}
	order := parseOrderValue(value)
	if order.ID == "" {
		order.ID = strings.TrimSpace(orderID)
	}
	accounts := rawAccounts(value)
	return TakeResult{
		Order:                order,
		Accounts:             accounts,
		ItemRemainingSeconds: orderItemRemainingSeconds(value),
		Pending:              status == http.StatusAccepted,
	}, nil
}

func (c *Client) ReleaseOrder(ctx context.Context, credentials Credentials, orderID string) (Order, error) {
	trimmedOrderID := strings.TrimSpace(orderID)
	if trimmedOrderID == "" {
		return Order{}, errors.New("supply order id is required")
	}
	escapedOrderID := url.PathEscape(trimmedOrderID)
	attempts := []struct {
		method string
		path   string
		body   any
	}{
		{method: http.MethodDelete, path: "/api/customer/pickup/orders/" + escapedOrderID},
		{method: http.MethodPost, path: "/api/customer/pickup/orders/" + escapedOrderID + "/cancel", body: map[string]any{}},
		{method: http.MethodPost, path: "/api/customer/pickup/orders/" + escapedOrderID + "/release", body: map[string]any{}},
	}
	for _, attempt := range attempts {
		value, _, err := c.doAuthenticated(ctx, credentials, attempt.method, attempt.path, attempt.body)
		if err == nil {
			order := parseOrderValue(value)
			if order.ID == "" {
				order.ID = trimmedOrderID
			}
			if order.Status == "" {
				order.Status = "released"
			}
			return order, nil
		}
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			switch httpErr.StatusCode {
			case http.StatusConflict:
				return Order{ID: trimmedOrderID, Status: "cancelled"}, nil
			case http.StatusNotFound, http.StatusMethodNotAllowed:
				continue
			}
		}
		return Order{}, err
	}
	return Order{}, ErrReleaseUnsupported
}

func (c *Client) doAuthenticated(ctx context.Context, credentials Credentials, method string, path string, body any) (any, int, error) {
	return c.doAuthenticatedWithTimeout(ctx, credentials, method, path, body, c.timeout)
}

func (c *Client) doAuthenticatedWithTimeout(ctx context.Context, credentials Credentials, method string, path string, body any, requestTimeout time.Duration) (any, int, error) {
	token, err := c.login(ctx, credentials, false)
	if err != nil {
		return nil, 0, err
	}
	value, status, err := c.requestWithTimeout(ctx, credentials.BaseURL, method, path, body, token, requestTimeout)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized {
		return value, status, err
	}
	c.invalidate(credentials)
	token, err = c.login(ctx, credentials, true)
	if err != nil {
		return nil, 0, err
	}
	return c.requestWithTimeout(ctx, credentials.BaseURL, method, path, body, token, requestTimeout)
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
	c.token = tokenState{key: key, token: token, expiresAt: time.Now().Add(11*time.Hour + 45*time.Minute)}
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
		return nil, res.StatusCode, &HTTPError{StatusCode: res.StatusCode, Message: errorMessage(data)}
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

// orderItemRemainingSeconds reads the supplier's per-delivery validity from
// order.items.  It intentionally does not inspect arbitrary "items" arrays:
// account payloads may use that name too, and treating those as order items
// would incorrectly shorten an imported account's lease.
func orderItemRemainingSeconds(value any) []int64 {
	root, _ := value.(map[string]any)
	if root == nil {
		return nil
	}
	return findOrderItemRemainingSeconds(root)
}

func findOrderItemRemainingSeconds(root map[string]any) []int64 {
	if order, ok := root["order"].(map[string]any); ok {
		if remaining, found := itemRemainingSeconds(order["items"]); found {
			return remaining
		}
	}
	if remaining, found := itemRemainingSeconds(root["items"]); found && orderLike(root) {
		return remaining
	}
	for _, key := range []string{"data", "payload", "result"} {
		if child, ok := root[key].(map[string]any); ok {
			if remaining := findOrderItemRemainingSeconds(child); len(remaining) > 0 {
				return remaining
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

func itemRemainingSeconds(value any) ([]int64, bool) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil, false
	}
	remaining := make([]int64, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		seconds, found := int64ValueOK(object, "remaining_seconds", "remainingSeconds")
		if !found {
			return nil, false
		}
		remaining = append(remaining, seconds)
	}
	return remaining, true
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
		for _, key := range []string{"data", "payload", "result"} {
			if child, exists := root[key]; exists {
				if result := findString(child, keys...); result != "" {
					return result
				}
			}
		}
	}
	return ""
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
		if message := findString(value, "message", "error", "detail"); message != "" {
			return message
		}
	}
	message := strings.TrimSpace(string(data))
	if len(message) > 512 {
		message = message[:512]
	}
	return message
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
