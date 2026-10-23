package service

import (
	"context"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// licenseFeatures returns the features a license actually has: its
// plan's entitlements, with each enabled addon adding a feature or
// replacing the plan's value for a feature of the same name. An addon
// replaces the limit, never the plan's billing (its Stripe meter). Verify,
// the offline token, the entitlements endpoint and usage limits all
// read this, so a license cannot have a feature in one and not another.
func licenseFeatures(ctx context.Context, st *store.Store, lic *model.License) ([]*model.Entitlement, error) {
	var out []*model.Entitlement
	index := map[string]int{}
	if lic.Plan != nil {
		for _, e := range lic.Plan.Entitlements {
			index[e.Feature] = len(out)
			out = append(out, e)
		}
	}
	if st == nil || lic.ID == "" {
		return out, nil
	}
	addons, _, err := st.ListLicenseAddons(ctx, lic.ID, store.All)
	if err != nil {
		return nil, err
	}
	for _, la := range addons {
		// An addon of another product grants nothing here. Attaching
		// one is refused now, but it once went through.
		if la.Addon == nil || la.Addon.ProductID != lic.ProductID {
			continue
		}
		a := la.Addon
		e := &model.Entitlement{
			Feature: a.Feature, ValueType: a.ValueType, Value: a.Value,
			QuotaPeriod: a.QuotaPeriod, QuotaUnit: a.QuotaUnit,
		}
		if i, ok := index[a.Feature]; ok {
			// The addon changes what the feature allows, not how it is
			// billed: the plan's Stripe meter stays, or usage of the
			// feature would stop reaching Stripe once the addon is on.
			plan := out[i]
			e.PlanID = plan.PlanID
			e.StripeMeterEventName = plan.StripeMeterEventName
			if e.QuotaUnit == "" {
				e.QuotaUnit = plan.QuotaUnit
			}
			out[i] = e
		} else {
			index[a.Feature] = len(out)
			out = append(out, e)
		}
	}
	return out, nil
}

// featureValues is the feature map sent in verify answers and tokens:
// booleans as true/false, everything else as its string value.
func featureValues(ents []*model.Entitlement) map[string]any {
	m := make(map[string]any, len(ents))
	for _, e := range ents {
		if e.ValueType == "bool" {
			m[e.Feature] = e.Value == "true"
		} else {
			m[e.Feature] = e.Value
		}
	}
	return m
}
