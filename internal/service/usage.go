package service

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

type UsageService struct {
	store            *store.Store
	webhook          *WebhookService
	logger           *slog.Logger
	warningThreshold float64
}

func NewUsageService(s *store.Store, wh *WebhookService, logger *slog.Logger, warningThreshold float64) *UsageService {
	return &UsageService{store: s, webhook: wh, logger: logger, warningThreshold: warningThreshold}
}

type RecordUsageInput struct {
	LicenseKey string
	Feature    string
	Quantity   int64
	Metadata   map[string]any
	ProductID  string
	IPAddress  string
}

type RecordUsageResult struct {
	Accepted  bool   `json:"accepted"`
	Used      int64  `json:"used"`
	Limit     int64  `json:"limit"`
	Remaining int64  `json:"remaining"`
	Period    string `json:"period"`
	PeriodKey string `json:"period_key"`
}

func (s *UsageService) RecordUsage(ctx context.Context, in RecordUsageInput) (*RecordUsageResult, error) {
	if in.Quantity <= 0 {
		in.Quantity = 1
	}

	lic, err := s.store.FindLicenseByKey(ctx, in.LicenseKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
		}
		return nil, apperr.Internal(err)
	}
	if in.ProductID != "" && lic.ProductID != in.ProductID {
		return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
	}

	var quota *model.Entitlement
	if lic.Plan != nil {
		for _, e := range lic.Plan.Entitlements {
			if e.Feature == in.Feature && e.ValueType == "quota" {
				quota = e
				break
			}
		}
	}

	if quota == nil {
		return nil, apperr.New(400, "NO_QUOTA", "no quota entitlement found for feature: "+in.Feature)
	}

	limit, _ := strconv.ParseInt(quota.Value, 10, 64)
	period := quota.QuotaPeriod
	if period == "" {
		period = "monthly"
	}
	periodKey := store.CurrentPeriodKey(period)

	// Atomically check limit and increment in a single transaction to prevent race conditions.
	// This ensures two concurrent requests cannot both pass the limit check.
	updated, accepted, err := s.store.IncrementUsageCounterWithLimit(ctx, lic.ID, in.Feature, period, periodKey, in.Quantity, limit)
	if err != nil {
		return nil, apperr.Internal(err)
	}

	if !accepted {
		currentUsed := updated.Used
		if err := s.webhook.DispatchWithLog(ctx, lic.ProductID, model.EventQuotaExceeded, map[string]any{
			"license_id": lic.ID, "feature": in.Feature, "used": currentUsed, "limit": limit,
		}); err != nil {
			s.logger.Error("webhook dispatch failed", "event", model.EventQuotaExceeded, "error", err)
		}
		return nil, apperr.WithDetails(
			apperr.New(429, "QUOTA_EXCEEDED", "usage quota exceeded for "+in.Feature),
			map[string]any{"used": currentUsed, "limit": limit, "period": period},
		)
	}

	event := &model.UsageEvent{
		LicenseID: lic.ID,
		Feature:   in.Feature,
		Quantity:  in.Quantity,
		Metadata:  in.Metadata,
		IPAddress: in.IPAddress,
	}
	if err := s.store.RecordUsageEvent(ctx, event); err != nil {
		s.logger.Error("failed to record usage event", "error", err)
	}

	newUsed := updated.Used
	remaining := limit - newUsed
	if limit == 0 {
		remaining = -1
	}

	if limit > 0 && s.warningThreshold > 0 {
		ratio := float64(newUsed) / float64(limit)
		prevUsed := newUsed - in.Quantity
		prevRatio := float64(prevUsed) / float64(limit)
		if ratio >= s.warningThreshold && prevRatio < s.warningThreshold {
			if err := s.webhook.DispatchWithLog(ctx, lic.ProductID, model.EventQuotaWarning, map[string]any{
				"license_id": lic.ID, "feature": in.Feature, "used": newUsed, "limit": limit, "threshold": s.warningThreshold,
			}); err != nil {
				s.logger.Error("webhook dispatch failed", "event", model.EventQuotaWarning, "error", err)
			}
		}
	}

	s.logger.Info("usage recorded", "license_id", lic.ID, "feature", in.Feature, "quantity", in.Quantity, "used", newUsed)

	return &RecordUsageResult{
		Accepted:  true,
		Used:      newUsed,
		Limit:     limit,
		Remaining: remaining,
		Period:    period,
		PeriodKey: periodKey,
	}, nil
}

type QuotaStatus struct {
	Feature   string `json:"feature"`
	Used      int64  `json:"used"`
	Limit     int64  `json:"limit"`
	Remaining int64  `json:"remaining"`
	Period    string `json:"period"`
	PeriodKey string `json:"period_key"`
}

func (s *UsageService) GetQuotaStatus(ctx context.Context, licenseKey, feature, productID string) (*QuotaStatus, error) {
	lic, err := s.store.FindLicenseByKey(ctx, licenseKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
		}
		return nil, apperr.Internal(err)
	}
	if productID != "" && lic.ProductID != productID {
		return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
	}

	var quota *model.Entitlement
	if lic.Plan != nil {
		for _, e := range lic.Plan.Entitlements {
			if e.Feature == feature && e.ValueType == "quota" {
				quota = e
				break
			}
		}
	}
	if quota == nil {
		return nil, apperr.NotFound("QUOTA", feature)
	}

	limit, _ := strconv.ParseInt(quota.Value, 10, 64)
	period := quota.QuotaPeriod
	if period == "" {
		period = "monthly"
	}
	periodKey := store.CurrentPeriodKey(period)

	counter, _ := s.store.GetUsageCounter(ctx, lic.ID, feature, period, periodKey)
	used := int64(0)
	if counter != nil {
		used = counter.Used
	}

	remaining := limit - used
	if limit == 0 {
		remaining = -1
	}

	return &QuotaStatus{
		Feature:   feature,
		Used:      used,
		Limit:     limit,
		Remaining: remaining,
		Period:    period,
		PeriodKey: periodKey,
	}, nil
}
