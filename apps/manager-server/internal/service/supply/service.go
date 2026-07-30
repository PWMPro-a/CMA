package supply

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supplyclient"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

var (
	ErrNotConfigured       = errors.New("account supply is not configured")
	ErrOrderInProgress     = errors.New("a supply order is already in progress")
	ErrInvalidQuantity     = errors.New("replenishment quantity must be between 1 and 100")
	ErrInsufficientBalance = errors.New("supply account available balance is insufficient")
	ErrCreateUncertain     = errors.New("supply order creation result is uncertain")
	ErrOrderNotFound       = errors.New("supply order was not found")
	ErrNotCreateUncertain  = errors.New("supply order is not waiting for create-result confirmation")
)

type Overview struct {
	CheckedAtMS  int64                   `json:"checkedAtMs,omitempty"`
	CPAAvailable int                     `json:"cpaAvailable"`
	CPATarget    int                     `json:"cpaTarget"`
	CPADeficit   int                     `json:"cpaDeficit"`
	Inventory    *supplyclient.Inventory `json:"inventory,omitempty"`
	Balance      *supplyclient.Balance   `json:"balance,omitempty"`
	LastError    string                  `json:"lastError,omitempty"`
}

type Status struct {
	Config      store.ManagerSupplyConfig `json:"config"`
	Running     bool                      `json:"running"`
	Overview    Overview                  `json:"overview"`
	ActiveOrder *store.SupplyOrder        `json:"activeOrder,omitempty"`
	Orders      []store.SupplyOrder       `json:"orders"`
}

type Service struct {
	store         *store.Store
	managerConfig *managerconfigsvc.Service
	supplyClient  *supplyclient.Client
	authFiles     *cpaauthfiles.Client

	runMu    sync.Mutex
	stateMu  sync.RWMutex
	running  bool
	overview Overview
}

func New(st *store.Store, managerConfig *managerconfigsvc.Service, httpClient ...*http.Client) *Service {
	var client *http.Client
	if len(httpClient) > 0 {
		client = httpClient[0]
	}
	return &Service{
		store:         st,
		managerConfig: managerConfig,
		supplyClient:  supplyclient.New(client),
		authFiles:     cpaauthfiles.New(client),
	}
}

func (s *Service) GetStatus(ctx context.Context, limit int) (Status, error) {
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return Status{}, err
	}
	orders, err := s.store.ListSupplyOrders(ctx, limit)
	if err != nil {
		return Status{}, err
	}
	active, found, err := s.store.GetOpenSupplyOrder(ctx)
	if err != nil {
		return Status{}, err
	}
	s.stateMu.RLock()
	overview := s.overview
	running := s.running
	s.stateMu.RUnlock()
	overview.CPATarget = cfg.Supply.TargetAvailableAccounts
	if overview.CPATarget > overview.CPAAvailable {
		overview.CPADeficit = overview.CPATarget - overview.CPAAvailable
	} else {
		overview.CPADeficit = 0
	}
	status := Status{
		Config:   sanitizeConfig(cfg.Supply),
		Running:  running,
		Overview: overview,
		Orders:   orders,
	}
	if found {
		status.ActiveOrder = &active
	}
	return status, nil
}

func (s *Service) UpdateConfig(ctx context.Context, config store.ManagerSupplyConfig) (Status, error) {
	if _, err := s.managerConfig.UpdateSupply(ctx, config); err != nil {
		return Status{}, err
	}
	return s.GetStatus(ctx, 50)
}

func (s *Service) Check(ctx context.Context) (Status, error) {
	if err := s.run(ctx, false, 0, true); err != nil {
		s.recordError(err)
		return Status{}, err
	}
	return s.GetStatus(ctx, 50)
}

func (s *Service) Replenish(ctx context.Context, quantity int) (Status, error) {
	if quantity <= 0 || quantity > 100 {
		return Status{}, ErrInvalidQuantity
	}
	if err := s.run(ctx, true, quantity, true); err != nil {
		s.recordError(err)
		return Status{}, err
	}
	return s.GetStatus(ctx, 50)
}

func (s *Service) DismissCreateUncertain(ctx context.Context, orderID string) (Status, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	order, found, err := s.store.GetSupplyOrder(ctx, strings.TrimSpace(orderID))
	if err != nil {
		return Status{}, err
	}
	if !found {
		return Status{}, ErrOrderNotFound
	}
	if order.Status != "create_uncertain" {
		return Status{}, ErrNotCreateUncertain
	}
	order.Status = "dismissed"
	order.CompletedAtMS = time.Now().UnixMilli()
	order.NextPollAtMS = 0
	order.LastError = "create-result block dismissed after remote order verification"
	if err := s.store.UpdateSupplyOrder(ctx, order); err != nil {
		return Status{}, err
	}
	return s.GetStatus(ctx, 50)
}

func (s *Service) RunAutomatic(ctx context.Context) error {
	err := s.run(ctx, true, 0, false)
	if err != nil {
		s.recordError(err)
	}
	return err
}

func (s *Service) NextInterval(ctx context.Context) time.Duration {
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return 30 * time.Second
	}
	if order, found, err := s.store.GetOpenSupplyOrder(ctx); err == nil && found {
		if order.Status == "creating" || order.Status == "create_uncertain" {
			return time.Minute
		}
		if wait := time.Until(time.UnixMilli(order.NextPollAtMS)); wait > 0 {
			if wait > time.Minute {
				return time.Minute
			}
			return wait
		}
		seconds := cfg.Supply.PollIntervalSeconds
		if seconds <= 0 {
			seconds = 3
		}
		return time.Duration(seconds) * time.Second
	}
	if !managerconfigsvc.SupplyEnabled(cfg.Supply) {
		return time.Minute
	}
	seconds := cfg.Supply.CheckIntervalSeconds
	if seconds <= 0 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

func (s *Service) run(ctx context.Context, allowCreate bool, manualQuantity int, force bool) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.setRunning(true)
	defer s.setRunning(false)

	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return err
	}
	supplyCfg := cfg.Supply
	active, found, err := s.store.GetOpenSupplyOrder(ctx)
	if err != nil {
		return err
	}
	if found {
		if manualQuantity > 0 {
			return ErrOrderInProgress
		}
		if err := s.requireCredentials(supplyCfg); err != nil {
			return err
		}
		if !force && active.NextPollAtMS > time.Now().UnixMilli() {
			return nil
		}
		return s.processOrder(ctx, cfg, active)
	}

	if allowCreate && manualQuantity == 0 && !managerconfigsvc.SupplyEnabled(supplyCfg) {
		return nil
	}
	if err := s.requireCredentials(supplyCfg); err != nil {
		return err
	}
	if !allowCreate {
		return s.refreshOverview(ctx, cfg, supplyCfg.ReplenishBatchSize)
	}

	available, err := s.countAvailableAccounts(ctx, cfg)
	if err != nil {
		return err
	}
	quantity := manualQuantity
	if quantity == 0 {
		deficit := supplyCfg.TargetAvailableAccounts - available
		if deficit <= 0 {
			return s.refreshSupplyOverview(ctx, supplyCfg, available, max(1, supplyCfg.ReplenishBatchSize))
		}
		quantity = min(deficit, supplyCfg.ReplenishBatchSize)
	}
	if quantity <= 0 {
		return ErrInvalidQuantity
	}

	inventory, balance, err := s.fetchSupplyOverview(ctx, supplyCfg, quantity)
	if err != nil {
		return err
	}
	s.setOverview(Overview{
		CheckedAtMS:  time.Now().UnixMilli(),
		CPAAvailable: available,
		CPATarget:    supplyCfg.TargetAvailableAccounts,
		CPADeficit:   max(0, supplyCfg.TargetAvailableAccounts-available),
		Inventory:    &inventory,
		Balance:      &balance,
	})
	if inventory.EstimatedTotalFen > 0 && balance.AvailableFen < inventory.EstimatedTotalFen {
		return ErrInsufficientBalance
	}

	attempt := store.SupplyOrder{
		OrderID:           newCreateAttemptID(),
		Product:           supplyCfg.Product,
		RequestedQuantity: quantity,
		Automatic:         manualQuantity == 0,
		Status:            "creating",
	}
	if _, err := s.store.CreateSupplyOrder(ctx, attempt); err != nil {
		return err
	}

	credentials := credentialsFromConfig(supplyCfg)
	remote, err := s.supplyClient.CreateOrder(ctx, credentials, supplyCfg.Product, quantity)
	if err != nil {
		if isDefiniteCreateFailure(err) {
			attempt.Status = "failed"
			attempt.LastError = safeError(err)
			attempt.CompletedAtMS = time.Now().UnixMilli()
			if updateErr := s.store.UpdateSupplyOrder(ctx, attempt); updateErr != nil {
				return updateErr
			}
			return err
		}
		attempt.Status = "create_uncertain"
		attempt.LastError = safeError(err)
		if updateErr := s.store.UpdateSupplyOrder(ctx, attempt); updateErr != nil {
			return updateErr
		}
		return fmt.Errorf("%w: %v", ErrCreateUncertain, err)
	}
	order := store.SupplyOrder{
		OrderID:           remote.ID,
		Product:           supplyCfg.Product,
		RequestedQuantity: quantity,
		Automatic:         manualQuantity == 0,
		Status:            localOrderStatus(remote.Status),
		RemoteStatus:      remote.Status,
		ReadyQuantity:     remote.ReadyQuantity,
		Progress:          remote.Progress,
		StatusURL:         remote.StatusURL,
		TakeURL:           remote.TakeURL,
		ChargedFen:        remote.ChargedFen,
		ReleasedFen:       remote.ReleasedFen,
		NextPollAtMS:      nextPollAt(supplyCfg, remote.RetryAfterSeconds),
	}
	if err := s.store.PromoteSupplyCreateAttempt(ctx, attempt.OrderID, order); err != nil {
		attempt.Status = "create_uncertain"
		attempt.RemoteStatus = remote.Status
		attempt.StatusURL = remote.StatusURL
		attempt.TakeURL = remote.TakeURL
		attempt.LastError = safeError(fmt.Errorf("remote order %s was created but local persistence failed: %w", remote.ID, err))
		_ = s.store.UpdateSupplyOrder(ctx, attempt)
		return err
	}
	if order.Status == "ready" || order.Status == "taking" {
		return s.processOrder(ctx, cfg, order)
	}
	return nil
}

func (s *Service) processOrder(ctx context.Context, cfg store.ManagerConfig, order store.SupplyOrder) error {
	if order.Status == "creating" {
		order.Status = "create_uncertain"
		order.LastError = "manager restarted while the create request was in progress"
		order.NextPollAtMS = 0
		return s.store.UpdateSupplyOrder(ctx, order)
	}
	if order.Status == "create_uncertain" {
		return nil
	}
	credentials := credentialsFromConfig(cfg.Supply)
	total, imported, err := s.store.SupplyImportCounts(ctx, order.OrderID)
	if err != nil {
		return err
	}
	if total > imported {
		return s.importItems(ctx, cfg, &order)
	}

	remote, err := s.supplyClient.GetOrder(ctx, credentials, order.OrderID, order.StatusURL)
	if err != nil {
		if isHTTPStatus(err, http.StatusConflict) {
			return s.cancelOrder(ctx, &order, err)
		}
		return s.updateOrderError(ctx, &order, err, cfg.Supply)
	}
	applyRemoteOrder(&order, remote, cfg.Supply)
	if isTerminalRemoteStatus(remote.Status) && !isSuccessfulRemoteStatus(remote.Status) {
		order.Status = localOrderStatus(remote.Status)
		order.CompletedAtMS = time.Now().UnixMilli()
		return s.store.UpdateSupplyOrder(ctx, order)
	}
	if !isReadyForTake(remote.Status) {
		order.Status = "waiting_inventory"
		return s.store.UpdateSupplyOrder(ctx, order)
	}

	order.Status = "taking"
	if err := s.store.UpdateSupplyOrder(ctx, order); err != nil {
		return err
	}
	taken, err := s.supplyClient.Take(ctx, credentials, order.OrderID, order.TakeURL)
	if err != nil {
		if isHTTPStatus(err, http.StatusConflict) {
			return s.cancelOrder(ctx, &order, err)
		}
		return s.updateOrderError(ctx, &order, err, cfg.Supply)
	}
	applyRemoteOrder(&order, taken.Order, cfg.Supply)
	if taken.Pending {
		order.Status = "waiting_inventory"
		return s.store.UpdateSupplyOrder(ctx, order)
	}
	items := make([]store.SupplyImportItem, 0, len(taken.Accounts))
	for _, raw := range taken.Accounts {
		payload, key, err := normalizeAccountPayload(raw)
		if err != nil {
			continue
		}
		items = append(items, store.SupplyImportItem{
			OrderID:     order.OrderID,
			ItemKey:     key,
			FileName:    "supply-" + key[:16] + ".json",
			PayloadJSON: string(payload),
		})
	}
	if len(items) == 0 {
		err := errors.New("supply take response did not include importable accounts")
		return s.updateOrderError(ctx, &order, err, cfg.Supply)
	}
	if _, err := s.store.InsertSupplyImportItems(ctx, order.OrderID, items); err != nil {
		return s.updateOrderError(ctx, &order, err, cfg.Supply)
	}
	order.Status = "importing"
	order.LastError = ""
	if err := s.store.UpdateSupplyOrder(ctx, order); err != nil {
		return err
	}
	return s.importItems(ctx, cfg, &order)
}

func (s *Service) importItems(ctx context.Context, cfg store.ManagerConfig, order *store.SupplyOrder) error {
	items, err := s.store.ListPendingSupplyImportItems(ctx, order.OrderID, time.Now().UnixMilli(), 100)
	if err != nil {
		return err
	}
	var firstErr error
	for _, item := range items {
		err := s.authFiles.Upload(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey,
			item.FileName, []byte(item.PayloadJSON), cfg.Supply.DefaultWebsockets)
		if err == nil {
			if markErr := s.store.MarkSupplyImportItemImported(ctx, item.ID, time.Now().UnixMilli()); markErr != nil {
				return markErr
			}
			continue
		}
		if firstErr == nil {
			firstErr = err
		}
		delay := retryDelay(item.AttemptCount + 1)
		if markErr := s.store.MarkSupplyImportItemFailed(ctx, item.ID, safeError(err), time.Now().Add(delay).UnixMilli()); markErr != nil {
			return markErr
		}
	}
	total, imported, err := s.store.SupplyImportCounts(ctx, order.OrderID)
	if err != nil {
		return err
	}
	order.ItemCount = total
	order.ImportedCount = imported
	if total > 0 && imported == total {
		order.Status = "completed"
		order.CompletedAtMS = time.Now().UnixMilli()
		order.NextPollAtMS = 0
		order.LastError = ""
	} else {
		order.Status = "partial"
		order.NextPollAtMS = time.Now().Add(retryDelay(1)).UnixMilli()
		if firstErr != nil {
			order.LastError = safeError(firstErr)
		}
	}
	if err := s.store.UpdateSupplyOrder(ctx, *order); err != nil {
		return err
	}
	return firstErr
}

func (s *Service) refreshOverview(ctx context.Context, cfg store.ManagerConfig, quantity int) error {
	available, err := s.countAvailableAccounts(ctx, cfg)
	if err != nil {
		return err
	}
	return s.refreshSupplyOverview(ctx, cfg.Supply, available, max(1, quantity))
}

func (s *Service) refreshSupplyOverview(ctx context.Context, cfg store.ManagerSupplyConfig, available int, quantity int) error {
	inventory, balance, err := s.fetchSupplyOverview(ctx, cfg, quantity)
	if err != nil {
		return err
	}
	s.setOverview(Overview{
		CheckedAtMS:  time.Now().UnixMilli(),
		CPAAvailable: available,
		CPATarget:    cfg.TargetAvailableAccounts,
		CPADeficit:   max(0, cfg.TargetAvailableAccounts-available),
		Inventory:    &inventory,
		Balance:      &balance,
	})
	return nil
}

func (s *Service) fetchSupplyOverview(ctx context.Context, cfg store.ManagerSupplyConfig, quantity int) (supplyclient.Inventory, supplyclient.Balance, error) {
	credentials := credentialsFromConfig(cfg)
	inventory, err := s.supplyClient.Inventory(ctx, credentials, cfg.Product, quantity)
	if err != nil {
		return supplyclient.Inventory{}, supplyclient.Balance{}, err
	}
	balance, err := s.supplyClient.Balance(ctx, credentials)
	if err != nil {
		return supplyclient.Inventory{}, supplyclient.Balance{}, err
	}
	return inventory, balance, nil
}

func (s *Service) countAvailableAccounts(ctx context.Context, cfg store.ManagerConfig) (int, error) {
	if strings.TrimSpace(cfg.CPAConnection.CPABaseURL) == "" || strings.TrimSpace(cfg.CPAConnection.ManagementKey) == "" {
		return 0, errors.New("CPA connection is not configured")
	}
	count := 0
	err := s.authFiles.Visit(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, func(file cpaauthfiles.File) (bool, error) {
		if isAvailableCodexFile(file) {
			count++
		}
		return false, nil
	})
	return count, err
}

func (s *Service) requireCredentials(cfg store.ManagerSupplyConfig) error {
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Username) == "" || strings.TrimSpace(cfg.Password) == "" {
		return ErrNotConfigured
	}
	parsed, err := url.Parse(cfg.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("account supply base URL is invalid")
	}
	if cfg.Product != "oauth_30d" && cfg.Product != "oauth_7d" {
		return errors.New("account supply product is invalid")
	}
	return nil
}

func (s *Service) updateOrderError(ctx context.Context, order *store.SupplyOrder, err error, cfg store.ManagerSupplyConfig) error {
	order.LastError = safeError(err)
	order.NextPollAtMS = nextPollAt(cfg, 0)
	if updateErr := s.store.UpdateSupplyOrder(ctx, *order); updateErr != nil {
		return updateErr
	}
	return err
}

func (s *Service) setRunning(running bool) {
	s.stateMu.Lock()
	s.running = running
	s.stateMu.Unlock()
}

func (s *Service) setOverview(overview Overview) {
	s.stateMu.Lock()
	s.overview = overview
	s.stateMu.Unlock()
}

func (s *Service) recordError(err error) {
	if err == nil {
		return
	}
	s.stateMu.Lock()
	s.overview.LastError = safeError(err)
	s.stateMu.Unlock()
}

func sanitizeConfig(cfg store.ManagerSupplyConfig) store.ManagerSupplyConfig {
	cfg.PasswordConfigured = strings.TrimSpace(cfg.Password) != ""
	cfg.Password = ""
	return cfg
}

func credentialsFromConfig(cfg store.ManagerSupplyConfig) supplyclient.Credentials {
	return supplyclient.Credentials{BaseURL: cfg.BaseURL, Username: cfg.Username, Password: cfg.Password}
}

func applyRemoteOrder(order *store.SupplyOrder, remote supplyclient.Order, cfg store.ManagerSupplyConfig) {
	if remote.Status != "" {
		order.RemoteStatus = remote.Status
	}
	if remote.ChargedFen > 0 {
		order.ChargedFen = remote.ChargedFen
	}
	if remote.ReleasedFen > 0 {
		order.ReleasedFen = remote.ReleasedFen
	}
	if remote.ReadyQuantity > order.ReadyQuantity {
		order.ReadyQuantity = remote.ReadyQuantity
	}
	if remote.Progress > order.Progress {
		order.Progress = remote.Progress
	}
	if strings.TrimSpace(remote.StatusURL) != "" {
		order.StatusURL = remote.StatusURL
	}
	if strings.TrimSpace(remote.TakeURL) != "" {
		order.TakeURL = remote.TakeURL
	}
	order.NextPollAtMS = nextPollAt(cfg, remote.RetryAfterSeconds)
	order.LastError = ""
}

func (s *Service) cancelOrder(ctx context.Context, order *store.SupplyOrder, err error) error {
	order.Status = "cancelled"
	order.RemoteStatus = "cancelled"
	order.LastError = safeError(err)
	order.NextPollAtMS = 0
	order.CompletedAtMS = time.Now().UnixMilli()
	return s.store.UpdateSupplyOrder(ctx, *order)
}

func isHTTPStatus(err error, status int) bool {
	var upstreamErr *supplyclient.HTTPError
	return errors.As(err, &upstreamErr) && upstreamErr.StatusCode == status
}

func isDefiniteCreateFailure(err error) bool {
	var upstreamErr *supplyclient.HTTPError
	if !errors.As(err, &upstreamErr) {
		return false
	}
	switch upstreamErr.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusPaymentRequired, http.StatusConflict,
		http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func newCreateAttemptID() string {
	random := make([]byte, 6)
	if _, err := cryptorand.Read(random); err != nil {
		return fmt.Sprintf("create-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("create-%d-%s", time.Now().UnixMilli(), hex.EncodeToString(random))
}

func localOrderStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ready", "ready_for_pickup":
		return "ready"
	case "completed", "done", "taken", "delivered":
		return "ready"
	case "cancelled", "canceled":
		return "cancelled"
	case "failed", "error", "expired":
		return "failed"
	default:
		return "waiting_inventory"
	}
}

func isTerminalRemoteStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "done", "taken", "delivered", "cancelled", "canceled", "failed", "error", "expired":
		return true
	default:
		return false
	}
}

func isSuccessfulRemoteStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "done", "taken", "delivered":
		return true
	default:
		return false
	}
}

func isReadyForTake(status string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	return status == "ready" || status == "ready_for_pickup" || isSuccessfulRemoteStatus(status)
}

func nextPollAt(cfg store.ManagerSupplyConfig, retryAfterSeconds int) int64 {
	seconds := retryAfterSeconds
	if seconds <= 0 {
		seconds = cfg.PollIntervalSeconds
	}
	if seconds <= 0 {
		seconds = 3
	}
	return time.Now().Add(time.Duration(seconds) * time.Second).UnixMilli()
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	seconds := 5 * (1 << min(attempt-1, 6))
	if seconds > 300 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

func normalizeAccountPayload(raw json.RawMessage) ([]byte, string, error) {
	payload := bytes.TrimSpace(raw)
	if len(payload) == 0 {
		return nil, "", errors.New("empty supply account payload")
	}
	if payload[0] == '"' {
		var text string
		if err := json.Unmarshal(payload, &text); err != nil {
			return nil, "", err
		}
		payload = []byte(strings.TrimSpace(text))
	}
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || len(object) == 0 {
		if err == nil {
			err = errors.New("supply account payload is empty")
		}
		return nil, "", err
	}
	normalized, err := json.Marshal(object)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(normalized)
	return normalized, hex.EncodeToString(sum[:]), nil
}

func isAvailableCodexFile(file cpaauthfiles.File) bool {
	if file.Disabled {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(file.Provider))
	if provider != "codex" && provider != "openai-codex" {
		return false
	}
	if boolField(file.Raw, "unavailable", "expired", "revoked", "deleted") {
		return false
	}
	status := strings.ToLower(textField(file.Raw, "status", "state"))
	switch status {
	case "disabled", "inactive", "invalid", "expired", "revoked", "deleted", "failed", "error", "unavailable":
		return false
	}
	return true
}

func boolField(values map[string]any, keys ...string) bool {
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case bool:
			return typed
		case string:
			parsed, _ := strconv.ParseBool(strings.TrimSpace(typed))
			return parsed
		case json.Number:
			parsed, _ := typed.Int64()
			return parsed != 0
		case float64:
			return typed != 0
		}
	}
	return false
}

func textField(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key]; ok && value != nil {
			return strings.TrimSpace(fmt.Sprint(value))
		}
	}
	return ""
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 1000 {
		message = message[:1000]
	}
	return message
}
