package codexquota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpa"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

const (
	codexUsageURL                    = "https://chatgpt.com/backend-api/wham/usage"
	codexResetCreditsURL             = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	codexConsumeResetCreditURL       = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
	defaultCodexUserAgent            = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
	maxCPAResponseBytes        int64 = 16 * 1024 * 1024
	maxLocalResetBytes         int64 = 1024 * 1024
	defaultOperationTimeout          = 8 * time.Second
)

type apiCallResult struct {
	StatusCode int
	Body       json.RawMessage
}

type cpaAdapter struct {
	client  *http.Client
	timeout time.Duration
}

func newCPAAdapter(client *http.Client) *cpaAdapter {
	if client == nil {
		client = &http.Client{Timeout: defaultOperationTimeout}
	}
	return &cpaAdapter{client: client, timeout: defaultOperationTimeout}
}

func (a *cpaAdapter) usage(ctx context.Context, setup store.Setup, authIndex string, accountID string) (apiCallResult, error) {
	return a.apiCall(ctx, setup, authIndex, accountID, http.MethodGet, codexUsageURL, "")
}

func (a *cpaAdapter) resetCredits(ctx context.Context, setup store.Setup, authIndex string, accountID string) (apiCallResult, error) {
	return a.apiCall(ctx, setup, authIndex, accountID, http.MethodGet, codexResetCreditsURL, "")
}

func (a *cpaAdapter) consumeResetCredit(ctx context.Context, setup store.Setup, authIndex string, accountID string, operationID string) (apiCallResult, error) {
	body, err := json.Marshal(map[string]string{"redeem_request_id": operationID})
	if err != nil {
		return apiCallResult{}, err
	}
	return a.apiCall(ctx, setup, authIndex, accountID, http.MethodPost, codexConsumeResetCreditURL, string(body))
}

func (a *cpaAdapter) apiCall(
	ctx context.Context,
	setup store.Setup,
	authIndex string,
	accountID string,
	method string,
	upstreamURL string,
	data string,
) (apiCallResult, error) {
	headers := map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Content-Type":  "application/json",
		"User-Agent":    defaultCodexUserAgent,
		"Accept":        "application/json",
		"OpenAI-Beta":   "codex-1",
		"Originator":    "Codex Desktop",
	}
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		headers["Chatgpt-Account-Id"] = accountID
	}
	payload, err := json.Marshal(map[string]any{
		"authIndex":        strings.TrimSpace(authIndex),
		"ensureFreshToken": true,
		"method":           method,
		"url":              upstreamURL,
		"header":           headers,
		"data":             data,
	})
	if err != nil {
		return apiCallResult{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodPost,
		cpa.NormalizeBaseURL(setup.CPAUpstreamURL)+"/v0/management/api-call",
		bytes.NewReader(payload),
	)
	if err != nil {
		return apiCallResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+setup.ManagementKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return apiCallResult{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return apiCallResult{}, fmt.Errorf("CPA api-call HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope map[string]json.RawMessage
	if err := decodeLimitedJSON(res.Body, maxCPAResponseBytes, &envelope); err != nil {
		return apiCallResult{}, err
	}
	statusRaw, ok := firstRaw(envelope, "status_code", "statusCode")
	if !ok {
		return apiCallResult{}, errors.New("CPA api-call response is missing status_code")
	}
	var statusCode int
	if err := json.Unmarshal(statusRaw, &statusCode); err != nil {
		return apiCallResult{}, errors.New("CPA api-call response has an invalid status_code")
	}
	bodyRaw, _ := firstRaw(envelope, "body")
	body, err := normalizeAPICallBody(bodyRaw)
	if err != nil {
		return apiCallResult{}, err
	}
	return apiCallResult{StatusCode: statusCode, Body: body}, nil
}

func (a *cpaAdapter) resetLocalQuota(ctx context.Context, setup store.Setup, authIndex string) (json.RawMessage, int, error) {
	payload, err := json.Marshal(map[string]string{"auth_index": strings.TrimSpace(authIndex)})
	if err != nil {
		return nil, 0, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodPost,
		cpa.NormalizeBaseURL(setup.CPAUpstreamURL)+"/v0/management/reset-quota",
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+setup.ManagementKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxLocalResetBytes+1))
	if err != nil {
		return nil, res.StatusCode, err
	}
	if int64(len(body)) > maxLocalResetBytes {
		return nil, res.StatusCode, fmt.Errorf("CPA reset-quota response exceeds %d bytes", maxLocalResetBytes)
	}
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		return nil, res.StatusCode, fmt.Errorf("CPA reset-quota HTTP %d: %s", res.StatusCode, truncate(string(body), 2048))
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return json.RawMessage(`{}`), res.StatusCode, nil
	}
	if !json.Valid(body) {
		return nil, res.StatusCode, errors.New("CPA reset-quota returned invalid JSON")
	}
	return json.RawMessage(body), res.StatusCode, nil
}

func decodeLimitedJSON(reader io.Reader, limit int64, target any) error {
	limited := &io.LimitedReader{R: reader, N: limit + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(target); err != nil {
		if limited.N == 0 {
			return fmt.Errorf("CPA response exceeds %d bytes", limit)
		}
		return err
	}
	if limited.N == 0 {
		return fmt.Errorf("CPA response exceeds %d bytes", limit)
	}
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("CPA response contains multiple JSON values")
	}
	return err
}

func firstRaw(values map[string]json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value, true
		}
	}
	return nil, false
}

func normalizeAPICallBody(raw json.RawMessage) (json.RawMessage, error) {
	trimmedRaw := bytes.TrimSpace(raw)
	if len(trimmedRaw) == 0 || bytes.Equal(trimmedRaw, []byte("null")) {
		return json.RawMessage(`null`), nil
	}
	if trimmedRaw[0] != '"' {
		if !json.Valid(trimmedRaw) {
			return nil, errors.New("CPA api-call body is invalid JSON")
		}
		return append(json.RawMessage(nil), trimmedRaw...), nil
	}
	var text string
	if err := json.Unmarshal(trimmedRaw, &text); err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace([]byte(text))
	if len(trimmed) == 0 {
		return json.RawMessage(`null`), nil
	}
	if json.Valid(trimmed) {
		return append(json.RawMessage(nil), trimmed...), nil
	}
	encoded, err := json.Marshal(text)
	return json.RawMessage(encoded), err
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
