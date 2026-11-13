package service

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

type EntitlementService struct {
	store  *store.Store
	logger *slog.Logger
}

func NewEntitlementService(s *store.Store, logger *slog.Logger) *EntitlementService {
	return &EntitlementService{store: s, logger: logger}
}

type CheckInput struct {
	LicenseKey string
	Feature    string // optional: check specific feature only
	ProductID  string
}

type CheckResult struct {
	Licensed bool                     `json:"licensed"`
	Status   string                   `json:"status"`
	PlanID   string                   `json:"plan_id"`
	PlanName string                   `json:"plan_name"`
	Features map[string]FeatureStatus `json:"features"`
}

type FeatureStatus struct {
	Enabled   bool   `json:"enabled"`
	ValueType string `json:"value_type"`
	Value     string `json:"value"`
	Used      *int64 `json:"used,omitempty"`
	Limit     *int64 `json:"limit,omitempty"`
	Remaining *int64 `json:"remaining,omitempty"`
	Period    string `json:"period,omitempty"`
	ResetsAt  string `json:"resets_at,omitempty"`
}

func (s *EntitlementService) Check(ctx context.Context, in CheckInput) (*CheckResult, error) {
	// Public SDK endpoint: a 404 for any inaccessible license closes
	// the existence-oracle. We use usabilityRequired=true so the
	// suspended/revoked/expired status strings never escape this
	// service — callers see "not found" instead.
	lic, err := loadLicenseForSDK(ctx, s.store, in.LicenseKey, in.ProductID, "", true)
	if err != nil {
		return nil, err
	}

	// Only "active" or "trialing" reach this point. The status field
	// is kept in the response for SDK clients that key off it, but
	// it can never reveal a sensitive lifecycle state — those have
	// already been folded into the 404 above.
	planName := ""
	planID := ""
	if lic.Plan != nil {
		planName = lic.Plan.Name
		planID = lic.Plan.ID
	}

	result := &CheckResult{
		Licensed: true,
		Status:   lic.Status,
		PlanID:   planID,
		PlanName: planName,
		Features: make(map[string]FeatureStatus),
	}

	// Plan entitlements with the license's addons merged in. A bare plan
	// still gets its addons' features.
	ents, err := LicenseFeatures(ctx, s.store, lic)
	if err != nil {
		return nil, err
	}

	for _, e := range ents {
		if in.Feature != "" && e.Feature != in.Feature {
			continue
		}

		fs := FeatureStatus{
			ValueType: e.ValueType,
			Value:     e.Value,
			Enabled:   true,
		}

		switch e.ValueType {
		case "bool", "flag":
			fs.Enabled = featureEnabled(e)
		case "quota":
			limit, _ := strconv.ParseInt(e.Value, 10, 64)
			period := e.QuotaPeriod
			if period == "" {
				period = "monthly"
			}
			periodKey := store.CurrentPeriodKey(period)
			counter, err := s.store.GetUsageCounter(ctx, lic.ID, e.Feature, period, periodKey)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, apperr.Internal(err)
			}
			used := int64(0)
			if counter != nil {
				used = counter.Used
			}
			remaining := quotaRemaining(limit, used)
			fs.Used = &used
			fs.Limit = &limit
			fs.Remaining = &remaining
			fs.Period = period
			fs.ResetsAt = nextPeriodReset(period)
		}

		result.Features[e.Feature] = fs
	}

	return result, nil
}

func nextPeriodReset(period string) string {
	now := time.Now().UTC()
	switch period {
	case "hourly":
		return now.Truncate(time.Hour).Add(time.Hour).Format(time.RFC3339)
	case "daily":
		return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	case "monthly":
		return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	case "yearly":
		return time.Date(now.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	default:
		return ""
	}
}
