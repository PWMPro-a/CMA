package supply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supplyclient"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

const (
	defaultNvtokensRefreshCooldown = 5 * time.Minute
	nvtokensRefreshTimeout         = 3 * time.Minute
	challengePollInterval          = 2 * time.Second
	maxChallengeResponseBytes      = 1024 * 1024
)

type NvtokensSessionRefreshStatus struct {
	PlatformID      string `json:"platformId"`
	State           string `json:"state"`
	LastRefreshAtMS int64  `json:"lastRefreshAtMs,omitempty"`
	NextRetryAtMS   int64  `json:"nextRetryAtMs,omitempty"`
	LastError       string `json:"lastError,omitempty"`
}

type nvtokensRefreshFlight struct {
	done    chan struct{}
	session string
	err     error
}

type nvtokensRefreshState struct {
	state         string
	lastRefreshAt time.Time
	nextRetryAt   time.Time
	lastError     string
	flight        *nvtokensRefreshFlight
}

func (s *Service) nvtokensRefreshStatuses(cfg store.ManagerSupplyConfig) []NvtokensSessionRefreshStatus {
	platforms := managerconfigsvc.SupplyPlatforms(cfg)
	s.nvtokensRefreshMu.Lock()
	defer s.nvtokensRefreshMu.Unlock()
	result := make([]NvtokensSessionRefreshStatus, 0, len(platforms))
	for _, platform := range platforms {
		if !strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformNvtokens) {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(platform.ID))
		status := NvtokensSessionRefreshStatus{PlatformID: platform.ID, State: "disabled"}
		if !nvtokensSessionRefreshEnabled(platform) {
			result = append(result, status)
			continue
		}
		status.State = "healthy"
		if state := s.nvtokensRefreshState[key]; state != nil {
			status.State = state.state
			if status.State == "" {
				status.State = "healthy"
			}
			if !state.lastRefreshAt.IsZero() {
				status.LastRefreshAtMS = state.lastRefreshAt.UnixMilli()
			}
			if !state.nextRetryAt.IsZero() {
				status.NextRetryAtMS = state.nextRetryAt.UnixMilli()
			}
			status.LastError = state.lastError
		}
		result = append(result, status)
	}
	return result
}

func (s *Service) resetNvtokensRefreshStates(cfg store.ManagerSupplyConfig) {
	if s == nil {
		return
	}
	configured := make(map[string]struct{})
	for _, platform := range managerconfigsvc.SupplyPlatforms(cfg) {
		if strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformNvtokens) &&
			nvtokensSessionRefreshEnabled(platform) {
			configured[strings.ToLower(strings.TrimSpace(platform.ID))] = struct{}{}
		}
	}
	s.nvtokensRefreshMu.Lock()
	for key, state := range s.nvtokensRefreshState {
		if _, ok := configured[key]; !ok {
			delete(s.nvtokensRefreshState, key)
			continue
		}
		if state != nil && state.flight == nil {
			state.nextRetryAt = time.Time{}
			state.lastError = ""
			state.state = "healthy"
		}
	}
	s.nvtokensRefreshMu.Unlock()
}

func nvtokensSessionRefreshEnabled(platform store.ManagerSupplyPlatformConfig) bool {
	return platform.SessionRefreshEnabled != nil && *platform.SessionRefreshEnabled
}

func (s *Service) refreshNvtokensSession(ctx context.Context, credentials supplyclient.Credentials) (string, error) {
	if s == nil || s.managerConfig == nil || s.supplyClient == nil {
		return "", supplyclient.ErrNvtokensSessionRefreshUnavailable
	}
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return "", err
	}
	platform, found := nvtokensRefreshPlatform(cfg.Supply, credentials.ID)
	if !found || !nvtokensSessionRefreshEnabled(platform) {
		return "", supplyclient.ErrNvtokensSessionRefreshUnavailable
	}
	return s.refreshNvtokensPlatform(ctx, platform, false)
}

func nvtokensRefreshPlatform(cfg store.ManagerSupplyConfig, platformID string) (store.ManagerSupplyPlatformConfig, bool) {
	for _, platform := range managerconfigsvc.SupplyPlatforms(cfg) {
		if strings.EqualFold(strings.TrimSpace(platform.ID), strings.TrimSpace(platformID)) &&
			strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformNvtokens) {
			return platform, true
		}
	}
	return store.ManagerSupplyPlatformConfig{}, false
}

func (s *Service) RefreshNvtokensSession(ctx context.Context, platformID string) (NvtokensSessionRefreshStatus, error) {
	if s == nil || s.managerConfig == nil {
		return NvtokensSessionRefreshStatus{}, ErrNotConfigured
	}
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return NvtokensSessionRefreshStatus{}, err
	}
	platform, found := nvtokensRefreshPlatform(cfg.Supply, platformID)
	if !found {
		return NvtokensSessionRefreshStatus{}, fmt.Errorf("%w: nvtokens platform %s was not found", ErrNotConfigured, platformID)
	}
	if !nvtokensSessionRefreshEnabled(platform) {
		return NvtokensSessionRefreshStatus{}, errors.New("nvtokens automatic session refresh is not enabled")
	}
	_, err = s.refreshNvtokensPlatform(ctx, platform, true)
	if err != nil {
		return s.nvtokensRefreshStatus(platform.ID), err
	}
	s.invalidateStatusCache()
	return s.nvtokensRefreshStatus(platform.ID), nil
}

func (s *Service) nvtokensRefreshStatus(platformID string) NvtokensSessionRefreshStatus {
	key := strings.ToLower(strings.TrimSpace(platformID))
	s.nvtokensRefreshMu.Lock()
	defer s.nvtokensRefreshMu.Unlock()
	result := NvtokensSessionRefreshStatus{PlatformID: platformID, State: "healthy"}
	state := s.nvtokensRefreshState[key]
	if state == nil {
		return result
	}
	result.State = state.state
	if result.State == "" {
		result.State = "healthy"
	}
	if !state.lastRefreshAt.IsZero() {
		result.LastRefreshAtMS = state.lastRefreshAt.UnixMilli()
	}
	if !state.nextRetryAt.IsZero() {
		result.NextRetryAtMS = state.nextRetryAt.UnixMilli()
	}
	result.LastError = state.lastError
	return result
}

func (s *Service) refreshNvtokensPlatform(ctx context.Context, platform store.ManagerSupplyPlatformConfig, force bool) (string, error) {
	key := strings.ToLower(strings.TrimSpace(platform.ID))
	now := time.Now()
	s.nvtokensRefreshMu.Lock()
	state := s.nvtokensRefreshState[key]
	if state == nil {
		state = &nvtokensRefreshState{state: "healthy"}
		s.nvtokensRefreshState[key] = state
	}
	if flight := state.flight; flight != nil {
		s.nvtokensRefreshMu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-flight.done:
			return flight.session, flight.err
		}
	}
	if !force && !state.nextRetryAt.IsZero() && now.Before(state.nextRetryAt) {
		err := fmt.Errorf("nvtokens session refresh is cooling down until %s: %s", state.nextRetryAt.Format(time.RFC3339), state.lastError)
		s.nvtokensRefreshMu.Unlock()
		return "", err
	}
	flight := &nvtokensRefreshFlight{done: make(chan struct{})}
	state.flight = flight
	state.state = "refreshing"
	state.lastError = ""
	state.nextRetryAt = time.Time{}
	s.nvtokensRefreshMu.Unlock()

	refreshCtx, cancel := context.WithTimeout(ctx, nvtokensRefreshTimeout)
	session, err := s.performNvtokensSessionRefresh(refreshCtx, platform)
	cancel()

	s.nvtokensRefreshMu.Lock()
	state.flight = nil
	flight.session = session
	flight.err = err
	if err != nil {
		cooldown := time.Duration(platform.RefreshCooldownSeconds) * time.Second
		if cooldown <= 0 {
			cooldown = defaultNvtokensRefreshCooldown
		}
		state.state = "cooldown"
		state.nextRetryAt = time.Now().Add(cooldown)
		state.lastError = safeError(err)
	} else {
		state.state = "healthy"
		state.lastRefreshAt = time.Now()
		state.nextRetryAt = time.Time{}
		state.lastError = ""
	}
	close(flight.done)
	s.nvtokensRefreshMu.Unlock()
	s.invalidateStatusCache()
	return session, err
}

func (s *Service) performNvtokensSessionRefresh(ctx context.Context, platform store.ManagerSupplyPlatformConfig) (string, error) {
	credentials := supplyPlatformCredentials(platform)
	provider := strings.ToLower(strings.TrimSpace(platform.ChallengeProvider))
	if provider == managerconfigsvc.SupplyChallengeProviderSessionSidecar {
		session, err := requestNvtokensSessionSidecar(ctx, s.challengeHTTPClient(), platform, credentials)
		if err != nil {
			return "", err
		}
		if err := s.supplyClient.ValidateNvtokensSession(ctx, credentials, session); err != nil {
			return "", fmt.Errorf("validate sidecar nvtokens session: %w", err)
		}
		return s.persistNvtokensSession(ctx, platform.ID, credentials, session)
	}
	challenge, err := s.supplyClient.NvtokensChallenge(ctx, credentials)
	if err != nil {
		return "", fmt.Errorf("load nvtokens challenge config: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(challenge.Provider), "turnstile") || strings.TrimSpace(challenge.SiteKey) == "" {
		return "", fmt.Errorf("unsupported nvtokens challenge provider %q", challenge.Provider)
	}
	s.setNvtokensRefreshPhase(platform.ID, "waiting_challenge")
	challengeToken, err := solveTurnstile(ctx, s.challengeHTTPClient(), platform, challenge.SiteKey)
	if err != nil {
		return "", err
	}
	session, err := s.supplyClient.LoginNvtokensWithChallenge(ctx, credentials, challengeToken)
	if err != nil {
		return "", err
	}
	return s.persistNvtokensSession(ctx, platform.ID, credentials, session)
}

func (s *Service) setNvtokensRefreshPhase(platformID string, phase string) {
	if s == nil {
		return
	}
	key := strings.ToLower(strings.TrimSpace(platformID))
	s.nvtokensRefreshMu.Lock()
	if state := s.nvtokensRefreshState[key]; state != nil && state.flight != nil {
		state.state = phase
	}
	s.nvtokensRefreshMu.Unlock()
	s.invalidateStatusCache()
}

func (s *Service) challengeHTTPClient() *http.Client {
	if s != nil && s.challengeClient != nil {
		return s.challengeClient
	}
	return http.DefaultClient
}

func (s *Service) persistNvtokensSession(ctx context.Context, platformID string, oldCredentials supplyclient.Credentials, session string) (string, error) {
	session = normalizeNvtokensSessionValue(session)
	if session == "" {
		return "", errors.New("nvtokens session refresh returned an empty session")
	}
	updated, err := s.managerConfig.UpdateSupplyPlatformSession(ctx, platformID, session)
	if err != nil {
		return "", fmt.Errorf("save refreshed nvtokens session: %w", err)
	}
	s.supplyClient.Invalidate(oldCredentials)
	s.supplyClient.Invalidate(supplyPlatformCredentials(updated))
	return session, nil
}

func normalizeNvtokensSessionValue(value string) string {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "session=") || strings.HasPrefix(lower, "scm_session=") {
		value = strings.TrimSpace(strings.SplitN(value, "=", 2)[1])
	}
	return value
}

func solveTurnstile(ctx context.Context, client *http.Client, platform store.ManagerSupplyPlatformConfig, siteKey string) (string, error) {
	provider := strings.ToLower(strings.TrimSpace(platform.ChallengeProvider))
	switch provider {
	case managerconfigsvc.SupplyChallengeProviderCapSolver, managerconfigsvc.SupplyChallengeProviderCapMonster:
		return solveTaskAPI(ctx, client, platform, siteKey)
	case managerconfigsvc.SupplyChallengeProviderTwoCaptcha:
		return solveTwoCaptcha(ctx, client, platform, siteKey)
	case managerconfigsvc.SupplyChallengeProviderCustom:
		return solveCustomChallenge(ctx, client, platform, siteKey)
	default:
		return "", fmt.Errorf("unsupported challenge provider %q", provider)
	}
}

func solveTaskAPI(ctx context.Context, client *http.Client, platform store.ManagerSupplyPlatformConfig, siteKey string) (string, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(platform.ChallengeAPIBase), "/")
	taskType := "TurnstileTaskProxyless"
	if strings.EqualFold(strings.TrimSpace(platform.ChallengeProvider), managerconfigsvc.SupplyChallengeProviderCapSolver) {
		taskType = "AntiTurnstileTaskProxyLess"
	}
	createPayload := map[string]any{
		"clientKey": platform.ChallengeAPIKey,
		"task": map[string]any{
			"type":       taskType,
			"websiteURL": strings.TrimRight(strings.TrimSpace(platform.BaseURL), "/"),
			"websiteKey": siteKey,
		},
	}
	created, err := challengeJSONRequest(ctx, client, http.MethodPost, baseURL+"/createTask", createPayload, nil)
	if err != nil {
		return "", err
	}
	if challengeError(created) != "" {
		return "", errors.New(challengeError(created))
	}
	taskID := challengeString(created, "taskId", "task_id", "id")
	if taskID == "" {
		return "", errors.New("challenge provider did not return taskId")
	}
	return pollChallenge(ctx, func(ctx context.Context) (string, bool, error) {
		result, err := challengeJSONRequest(ctx, client, http.MethodPost, baseURL+"/getTaskResult", map[string]any{
			"clientKey": platform.ChallengeAPIKey,
			"taskId":    taskID,
		}, nil)
		if err != nil {
			return "", false, err
		}
		if message := challengeError(result); message != "" {
			return "", false, errors.New(message)
		}
		status := strings.ToLower(challengeString(result, "status"))
		if status == "processing" || status == "pending" || status == "queued" {
			return "", false, nil
		}
		token := challengeNestedString(result, []string{"solution"}, "token", "gRecaptchaResponse", "captchaKey")
		if token == "" {
			token = challengeString(result, "token", "gRecaptchaResponse", "captchaKey")
		}
		if token == "" {
			return "", false, fmt.Errorf("challenge task finished without a Turnstile token (status=%s)", status)
		}
		return token, true, nil
	})
}

func solveTwoCaptcha(ctx context.Context, client *http.Client, platform store.ManagerSupplyPlatformConfig, siteKey string) (string, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(platform.ChallengeAPIBase), "/")
	created, err := challengeJSONRequest(ctx, client, http.MethodPost, baseURL+"/createTask", map[string]any{
		"clientKey": platform.ChallengeAPIKey,
		"task": map[string]any{
			"type":       "TurnstileTaskProxyless",
			"websiteURL": strings.TrimRight(strings.TrimSpace(platform.BaseURL), "/"),
			"websiteKey": siteKey,
		},
	}, nil)
	if err != nil {
		return "", err
	}
	if challengeError(created) != "" {
		return "", errors.New(challengeError(created))
	}
	taskID := challengeString(created, "taskId", "task_id", "id")
	if taskID == "" {
		return "", errors.New("2captcha did not return taskId")
	}
	return pollChallenge(ctx, func(ctx context.Context) (string, bool, error) {
		result, err := challengeJSONRequest(ctx, client, http.MethodPost, baseURL+"/getTaskResult", map[string]any{
			"clientKey": platform.ChallengeAPIKey,
			"taskId":    taskID,
		}, nil)
		if err != nil {
			return "", false, err
		}
		if message := challengeError(result); message != "" {
			return "", false, errors.New(message)
		}
		if strings.EqualFold(challengeString(result, "status"), "processing") {
			return "", false, nil
		}
		token := challengeNestedString(result, []string{"solution"}, "token")
		if token == "" {
			return "", false, errors.New("2captcha task finished without a Turnstile token")
		}
		return token, true, nil
	})
}

func solveCustomChallenge(ctx context.Context, client *http.Client, platform store.ManagerSupplyPlatformConfig, siteKey string) (string, error) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+strings.TrimSpace(platform.ChallengeAPIKey))
	result, err := challengeJSONRequest(ctx, client, http.MethodPost, strings.TrimSpace(platform.ChallengeAPIBase), map[string]any{
		"provider":   "turnstile",
		"siteKey":    siteKey,
		"websiteURL": strings.TrimRight(strings.TrimSpace(platform.BaseURL), "/"),
		"platformId": platform.ID,
	}, headers)
	if err != nil {
		return "", err
	}
	if message := challengeError(result); message != "" {
		return "", errors.New(message)
	}
	if session := challengeString(result, "session", "sessionToken"); session != "" {
		return "", errors.New("custom challenge provider returned a session; use session_sidecar mode")
	}
	token := challengeString(result, "token", "challengeToken", "captchaToken")
	if token == "" {
		token = challengeNestedString(result, []string{"solution"}, "token")
	}
	if token == "" {
		return "", errors.New("custom challenge provider did not return token")
	}
	return token, nil
}

func requestNvtokensSessionSidecar(ctx context.Context, client *http.Client, platform store.ManagerSupplyPlatformConfig, credentials supplyclient.Credentials) (string, error) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+strings.TrimSpace(platform.ChallengeAPIKey))
	result, err := challengeJSONRequest(ctx, client, http.MethodPost, strings.TrimSpace(platform.ChallengeAPIBase), map[string]any{
		"platformId": platform.ID,
		"baseUrl":    credentials.BaseURL,
		"username":   credentials.Username,
		"password":   credentials.Password,
	}, headers)
	if err != nil {
		return "", err
	}
	if message := challengeError(result); message != "" {
		return "", errors.New(message)
	}
	session := challengeString(result, "session", "sessionToken", "token")
	if session == "" {
		return "", errors.New("session sidecar did not return session")
	}
	return session, nil
}

func pollChallenge(ctx context.Context, poll func(context.Context) (string, bool, error)) (string, error) {
	if token, done, err := poll(ctx); err != nil {
		return "", err
	} else if done {
		return token, nil
	}
	ticker := time.NewTicker(challengePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			token, done, err := poll(ctx)
			if err != nil {
				return "", err
			}
			if done {
				return token, nil
			}
		}
	}
}

func challengeJSONRequest(ctx context.Context, client *http.Client, method string, endpoint string, payload any, headers http.Header) (map[string]any, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("challenge API URL is invalid")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("challenge API request: %w", err)
	}
	defer res.Body.Close()
	limited := &io.LimitedReader{R: res.Body, N: maxChallengeResponseBytes + 1}
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if limited.N == 0 {
		return nil, errors.New("challenge API response exceeded size limit")
	}
	var result map[string]any
	if len(bytes.TrimSpace(body)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if err := decoder.Decode(&result); err != nil {
			return nil, fmt.Errorf("decode challenge API response: %w", err)
		}
	}
	if result == nil {
		result = map[string]any{}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		message := challengeError(result)
		if message == "" {
			message = strings.TrimSpace(string(body))
		}
		return nil, fmt.Errorf("challenge API returned HTTP %d: %s", res.StatusCode, message)
	}
	return result, nil
}

func challengeError(value map[string]any) string {
	if value == nil {
		return ""
	}
	if errorID, ok := challengeInt(value, "errorId", "error_id"); ok && errorID != 0 {
		return firstChallengeString(value, "errorDescription", "error_description", "errorCode", "error_code", "message", "error")
	}
	if success, ok := value["success"].(bool); ok && !success {
		return firstChallengeString(value, "message", "error", "error_description")
	}
	if status := strings.ToLower(challengeString(value, "status")); status == "failed" || status == "error" {
		return firstChallengeString(value, "message", "error", "errorDescription", "errorCode")
	}
	return ""
}

func firstChallengeString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if result := strings.TrimSpace(fmt.Sprint(value[key])); result != "" && result != "<nil>" {
			return result
		}
	}
	return "challenge provider returned an error"
}

func challengeString(value map[string]any, keys ...string) string {
	if value == nil {
		return ""
	}
	for _, key := range keys {
		if raw, ok := value[key]; ok {
			if result := strings.TrimSpace(fmt.Sprint(raw)); result != "" && result != "<nil>" {
				return result
			}
		}
	}
	return ""
}

func challengeNestedString(value map[string]any, path []string, keys ...string) string {
	current := value
	for _, key := range path {
		next, ok := current[key].(map[string]any)
		if !ok {
			return ""
		}
		current = next
	}
	return challengeString(current, keys...)
}

func challengeInt(value map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		raw, ok := value[key]
		if !ok {
			continue
		}
		switch typed := raw.(type) {
		case json.Number:
			parsed, err := typed.Int64()
			return parsed, err == nil
		case float64:
			return int64(typed), true
		case int64:
			return typed, true
		}
	}
	return 0, false
}
