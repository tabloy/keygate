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

	licensed := lic.Status == "active" || lic.Status == "trialing"
	planName := ""
	planID := ""
	if lic.Plan != nil {
		planName = lic.Plan.Name
		planID = lic.Plan.ID
	}

	result := &CheckResult{
		Licensed: licensed,
		Status:   lic.Status,
		PlanID:   planID,
		PlanName: planName,
		Features: make(map[string]FeatureStatus),
	}

	if lic.Plan == nil || lic.Plan.Entitlements == nil {
		return result, nil
	}

	for _, e := range lic.Plan.Entitlements {
		if in.Feature != "" && e.Feature != in.Feature {
			continue
		}

		fs := FeatureStatus{
			ValueType: e.ValueType,
			Value:     e.Value,
			Enabled:   true,
		}

		switch e.ValueType {
		case "bool":
			fs.Enabled = e.Value == "true"
		case "quota":
			limit, _ := strconv.ParseInt(e.Value, 10, 64)
			period := e.QuotaPeriod
			if period == "" {
				period = "monthly"
			}
			periodKey := store.CurrentPeriodKey(period)
			counter, _ := s.store.GetUsageCounter(ctx, lic.ID, e.Feature, period, periodKey)
			used := int64(0)
			if counter != nil {
				used = counter.Used
			}
			remaining := limit - used
			if limit == 0 {
				remaining = -1
			}
			fs.Used = &used
			fs.Limit = &limit
			fs.Remaining = &remaining
			fs.Period = period
			fs.ResetsAt = nextPeriodReset(period)
		}

		result.Features[e.Feature] = fs
	}

	// Merge addon features (addons override or add to plan features)
	addons, _ := s.store.ListLicenseAddons(ctx, lic.ID)
	for _, la := range addons {
		if la.Addon == nil {
			continue
		}
		a := la.Addon
		if in.Feature != "" && a.Feature != in.Feature {
			continue
		}
		fs := FeatureStatus{
			ValueType: a.ValueType,
			Value:     a.Value,
			Enabled:   true,
		}
		switch a.ValueType {
		case "bool":
			fs.Enabled = a.Value == "true"
		case "quota":
			limit, _ := strconv.ParseInt(a.Value, 10, 64)
			period := a.QuotaPeriod
			if period == "" {
				period = "monthly"
			}
			periodKey := store.CurrentPeriodKey(period)
			counter, _ := s.store.GetUsageCounter(ctx, lic.ID, a.Feature, period, periodKey)
			used := int64(0)
			if counter != nil {
				used = counter.Used
			}
			remaining := limit - used
			if limit == 0 {
				remaining = -1
			}
			fs.Used = &used
			fs.Limit = &limit
			fs.Remaining = &remaining
			fs.Period = period
			fs.ResetsAt = nextPeriodReset(period)
		}
		result.Features[a.Feature] = fs
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
