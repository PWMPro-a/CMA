package quotathreshold

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

type Service struct{ store *store.Store }

type UpsertRequest struct {
	Rules []model.QuotaThresholdRule `json:"rules"`
}
type ListResponse struct {
	Items []model.QuotaThresholdRule `json:"items"`
}

func New(st *store.Store) *Service { return &Service{store: st} }
func (s *Service) List(ctx context.Context) (ListResponse, error) {
	items, err := s.store.QuotaThresholdRules.List(ctx)
	if err != nil {
		return ListResponse{}, err
	}
	return ListResponse{Items: items}, nil
}
func (s *Service) Upsert(ctx context.Context, rules []model.QuotaThresholdRule) (ListResponse, error) {
	items, err := s.store.QuotaThresholdRules.UpsertBatch(ctx, rules)
	if err != nil {
		return ListResponse{}, err
	}
	return ListResponse{Items: items}, nil
}
func (s *Service) Delete(ctx context.Context, id int64) error {
	err := s.store.QuotaThresholdRules.Delete(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	return err
}
func NormalizeProvider(value string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "_", "-"))
}
