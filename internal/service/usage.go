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

// quotaRemaining is what is left of a quota: -1 when it is unlimited (limit
// 0), and never below 0 otherwise, so usage past a limit that was lowered
// afterwards cannot read as unlimited.
func quotaRemaining(limit, used int64) int64 {
	if limit == 0 {
		return -1
	}
	return max(limit-used, 0)
}

type UsageService struct {
	store            *store.Store
	webhook          *WebhookService
	email            *EmailService
	logger           *slog.Logger
	warningThreshold float64
}

func NewUsageService(s *store.Store, wh *WebhookService, em *EmailService, logger *slog.Logger, warningThreshold float64) *UsageService {
	return &UsageService{store: s, webhook: wh, email: em, logger: logger, warningThreshold: warningThreshold}
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

	// Public SDK endpoint: collapse every "license unavailable"
	// condition into 404 LICENSE_NOT_FOUND. A leaked "exists but
	// suspended" would let attackers probe key validity without
	// burning the activation rate limit.
	lic, err := loadLicenseForSDK(ctx, s.store, in.LicenseKey, in.ProductID, "", true)
	if err != nil {
		return nil, err
	}

	quota, err := s.quotaFor(ctx, lic, in.Feature)
	if err != nil {
		return nil, apperr.Internal(err)
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
	if errors.Is(err, store.ErrUsageQuantityTooLarge) {
		return nil, apperr.New(400, "INVALID_INPUT", "quantity too large")
	}
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

	// Stripe metered-billing: when this entitlement is wired to a
	// Stripe meter, enqueue a meter-event row carrying the delta
	// from THIS call. The background MeteredBillingSync job pushes
	// rows to Stripe's Billing Meter API; pushing absolutes here
	// would double-count because Stripe accumulates server-side.
	//
	// Best-effort: a failure here doesn't roll back the in-Keygate
	// accounting (we'd rather over-grant than under-bill on a
	// transient blip; the next RecordUsage isn't affected).
	//
	// The row keeps the meter and customer it is recorded against, so
	// an edit to the entitlement or the license before it syncs bills
	// later usage differently, not this.
	if quota.StripeMeterEventName != "" {
		if err := s.store.InsertMeteredEvent(ctx, lic.ID, in.Feature, periodKey, quota.StripeMeterEventName, lic.StripeCustomerID, in.Quantity); err != nil {
			s.logger.Error("metered enqueue failed", "license_id", lic.ID, "feature", in.Feature, "error", err)
		}
	}

	newUsed := updated.Used
	remaining := quotaRemaining(limit, newUsed)

	if limit > 0 && s.warningThreshold > 0 {
		ratio := float64(newUsed) / float64(limit)
		prevUsed := newUsed - in.Quantity
		prevRatio := float64(prevUsed) / float64(limit)
		// Fire warning ONCE per crossing: previous reading was under
		// threshold, current crosses it.
		if ratio >= s.warningThreshold && prevRatio < s.warningThreshold {
			if err := s.webhook.DispatchWithLog(ctx, lic.ProductID, model.EventQuotaWarning, map[string]any{
				"license_id": lic.ID, "feature": in.Feature, "used": newUsed, "limit": limit, "threshold": s.warningThreshold,
			}); err != nil {
				s.logger.Error("webhook dispatch failed", "event", model.EventQuotaWarning, "error", err)
			}
			if s.email != nil && lic.Email != "" {
				productName := ""
				if lic.Product != nil {
					productName = lic.Product.Name
				}
				s.email.SendQuotaWarning(lic.Email, productName, in.Feature, newUsed, limit, int(ratio*100))
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

// quotaFor is the quota enforced for a feature: the plan's, or an
// enabled addon's, which replaces the plan's for the same feature.
// RecordUsage and GetQuotaStatus both read it, so the status always
// describes the limit and period that usage is counted against.
func (s *UsageService) quotaFor(ctx context.Context, lic *model.License, feature string) (*model.Entitlement, error) {
	ents, err := LicenseFeatures(ctx, s.store, lic)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if e.Feature == feature && e.ValueType == "quota" {
			return e, nil
		}
	}
	return nil, nil
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
	lic, err := loadLicenseForSDK(ctx, s.store, licenseKey, productID, "", true)
	if err != nil {
		return nil, err
	}

	quota, err := s.quotaFor(ctx, lic, feature)
	if err != nil {
		return nil, apperr.Internal(err)
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

	counter, err := s.store.GetUsageCounter(ctx, lic.ID, feature, period, periodKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.Internal(err)
	}
	used := int64(0)
	if counter != nil {
		used = counter.Used
	}

	remaining := quotaRemaining(limit, used)

	return &QuotaStatus{
		Feature:   feature,
		Used:      used,
		Limit:     limit,
		Remaining: remaining,
		Period:    period,
		PeriodKey: periodKey,
	}, nil
}
