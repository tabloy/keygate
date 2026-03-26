package store

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
)

type RetentionData struct {
	Period       string  `json:"period"`
	StartCount   int     `json:"start_count"`
	EndCount     int     `json:"end_count"`
	RetentionPct float64 `json:"retention_pct"`
	ChurnPct     float64 `json:"churn_pct"`
}

type GrowthMetrics struct {
	NetGrowthRate        float64 `json:"net_growth_rate"`
	TrialConversion      float64 `json:"trial_conversion"`
	AvgLicenseAgeDays    float64 `json:"avg_license_age_days"`
	MedianLicenseAgeDays float64 `json:"median_license_age_days"`
	TotalTrials          int     `json:"total_trials"`
	ConvertedTrials      int     `json:"converted_trials"`
	NewLast30d           int     `json:"new_last_30d"`
	ChurnedLast30d       int     `json:"churned_last_30d"`
}

type LicenseAgeDistribution struct {
	Bucket string `json:"bucket"`
	Count  int    `json:"count"`
}

type RecentActivity struct {
	ID        string    `json:"id"`
	Entity    string    `json:"entity"`
	EntityID  string    `json:"entity_id"`
	Action    string    `json:"action"`
	ActorType string    `json:"actor_type"`
	CreatedAt time.Time `json:"created_at"`
}

type TopUser struct {
	Email           string `json:"email"`
	UserID          string `json:"user_id"`
	LicenseCount    int    `json:"license_count"`
	ActiveCount     int    `json:"active_count"`
	TotalUsage      int64  `json:"total_usage"`
	ActivationCount int    `json:"activation_count"`
}

type AnalyticsSummary struct {
	TotalLicenses            int     `json:"total_licenses"`
	ActiveLicenses           int     `json:"active_licenses"`
	TrialingLicenses         int     `json:"trialing_licenses"`
	ExpiredLicenses          int     `json:"expired_licenses"`
	CanceledLicenses         int     `json:"canceled_licenses"`
	SuspendedLicenses        int     `json:"suspended_licenses"`
	RevokedLicenses          int     `json:"revoked_licenses"`
	PastDueLicenses          int     `json:"past_due_licenses"`
	TotalActivations         int     `json:"total_activations"`
	TotalSeats               int     `json:"total_seats"`
	AvgActivationsPerLicense float64 `json:"avg_activations_per_license"`
}

type BreakdownItem struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type FeatureUsageItem struct {
	Feature     string `json:"feature"`
	TotalUsage  int64  `json:"total_usage"`
	UniqueUsers int    `json:"unique_users"`
}

type TrendPoint struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type AggregatedSnapshot struct {
	Period           string `json:"period"`
	TotalLicenses    int    `json:"total_licenses"`
	ActiveLicenses   int    `json:"active_licenses"`
	NewLicenses      int    `json:"new_licenses"`
	Churned          int    `json:"churned"`
	TotalActivations int    `json:"total_activations"`
	TotalSeats       int    `json:"total_seats"`
	TotalUsage       int64  `json:"total_usage"`
}

func (s *Store) CreateAnalyticsSnapshot(ctx context.Context, snap *model.AnalyticsSnapshot) error {
	if snap.ID == "" {
		snap.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(snap).
		On("CONFLICT (date, product_id) DO UPDATE").
		Set("total_licenses = EXCLUDED.total_licenses, active_licenses = EXCLUDED.active_licenses, new_licenses = EXCLUDED.new_licenses, churned = EXCLUDED.churned, total_activations = EXCLUDED.total_activations, total_seats = EXCLUDED.total_seats, total_usage = EXCLUDED.total_usage").
		Exec(ctx)
	return err
}

func (s *Store) ListAnalyticsSnapshots(ctx context.Context, productID string, from, to time.Time) ([]*model.AnalyticsSnapshot, error) {
	// When filtered by product, each date has exactly one row — return as-is.
	if productID != "" {
		var out []*model.AnalyticsSnapshot
		q := s.DB.NewSelect().Model(&out).OrderExpr("date ASC").
			Where("product_id = ?", productID)
		if !from.IsZero() {
			q = q.Where("date >= ?", from)
		}
		if !to.IsZero() {
			q = q.Where("date <= ?", to)
		}
		err := q.Scan(ctx)
		return out, err
	}

	// No product filter: aggregate across all products per date to avoid
	// duplicate date entries (one row per product per day).
	var out []*model.AnalyticsSnapshot
	q := s.DB.NewSelect().
		TableExpr("analytics_snapshots").
		ColumnExpr("'' AS id").
		ColumnExpr("date").
		ColumnExpr("'' AS product_id").
		ColumnExpr("SUM(total_licenses)::int AS total_licenses").
		ColumnExpr("SUM(active_licenses)::int AS active_licenses").
		ColumnExpr("SUM(new_licenses)::int AS new_licenses").
		ColumnExpr("SUM(churned)::int AS churned").
		ColumnExpr("SUM(total_activations)::int AS total_activations").
		ColumnExpr("SUM(total_seats)::int AS total_seats").
		ColumnExpr("SUM(total_usage) AS total_usage").
		GroupExpr("date").
		OrderExpr("date ASC")
	if !from.IsZero() {
		q = q.Where("date >= ?", from)
	}
	if !to.IsZero() {
		q = q.Where("date <= ?", to)
	}
	err := q.Scan(ctx, &out)
	return out, err
}

func (s *Store) ListAnalyticsSnapshotsAggregated(ctx context.Context, productID string, from, to time.Time, granularity string) ([]*AggregatedSnapshot, error) {
	// When a single product is selected, each date has one row, so AVG across
	// the period bucket gives the average daily value. When no product filter
	// is set, we first need to SUM across products for each date, then AVG
	// across dates in the bucket. We handle this by always SUMming per date
	// (via a subquery) and then AVG-ing the period.
	var out []*AggregatedSnapshot

	inner := s.DB.NewSelect().
		TableExpr("analytics_snapshots").
		ColumnExpr("date").
		ColumnExpr("SUM(total_licenses)::int AS total_licenses").
		ColumnExpr("SUM(active_licenses)::int AS active_licenses").
		ColumnExpr("SUM(new_licenses)::int AS new_licenses").
		ColumnExpr("SUM(churned)::int AS churned").
		ColumnExpr("SUM(total_activations)::int AS total_activations").
		ColumnExpr("SUM(total_seats)::int AS total_seats").
		ColumnExpr("SUM(total_usage) AS total_usage").
		GroupExpr("date")

	if productID != "" {
		inner = inner.Where("product_id = ?", productID)
	}
	if !from.IsZero() {
		inner = inner.Where("date >= ?", from)
	}
	if !to.IsZero() {
		inner = inner.Where("date <= ?", to)
	}

	// Build outer truncation expression referencing the subquery's date column.
	var outerTrunc string
	switch granularity {
	case "weekly":
		outerTrunc = "date_trunc('week', daily.date)"
	case "monthly":
		outerTrunc = "date_trunc('month', daily.date)"
	default:
		outerTrunc = "date_trunc('day', daily.date)"
	}

	q := s.DB.NewSelect().
		TableExpr("(?) AS daily", inner).
		ColumnExpr(fmt.Sprintf("%s::text AS period", outerTrunc)).
		ColumnExpr("ROUND(AVG(total_licenses))::int AS total_licenses").
		ColumnExpr("ROUND(AVG(active_licenses))::int AS active_licenses").
		ColumnExpr("SUM(new_licenses)::int AS new_licenses").
		ColumnExpr("SUM(churned)::int AS churned").
		ColumnExpr("ROUND(AVG(total_activations))::int AS total_activations").
		ColumnExpr("ROUND(AVG(total_seats))::int AS total_seats").
		ColumnExpr("SUM(total_usage) AS total_usage").
		GroupExpr(outerTrunc).
		OrderExpr(fmt.Sprintf("%s ASC", outerTrunc))

	err := q.Scan(ctx, &out)
	return out, err
}

// AnalyticsFilter holds optional filters shared across analytics queries.
type AnalyticsFilter struct {
	ProductID   string
	PlanID      string
	LicenseType string
	Status      string
	From        time.Time
	To          time.Time
}

// applyLicenseFilters adds WHERE clauses for the common filter fields.
// tableAlias should be the alias used for the licenses table (e.g. "" or "l").
func applyLicenseFilters(q *bun.SelectQuery, f AnalyticsFilter, tableAlias string) *bun.SelectQuery {
	col := func(name string) string {
		if tableAlias != "" {
			return tableAlias + "." + name
		}
		return name
	}
	if f.ProductID != "" {
		q = q.Where(col("product_id")+" = ?", f.ProductID)
	}
	if f.PlanID != "" {
		// Specific plan takes precedence over license_type
		q = q.Where(col("plan_id")+" = ?", f.PlanID)
	} else if f.LicenseType != "" {
		q = q.Where(col("plan_id")+" IN (SELECT id FROM plans WHERE license_type = ?)", f.LicenseType)
	}
	if f.Status != "" {
		q = q.Where(col("status")+" = ?", f.Status)
	}
	if !f.From.IsZero() {
		q = q.Where(col("created_at")+" >= ?", f.From)
	}
	if !f.To.IsZero() {
		q = q.Where(col("created_at")+" <= ?", f.To)
	}
	return q
}

// licenseSubquery returns "SELECT id FROM licenses WHERE ..." for joining related tables.
func licenseSubquery(s *Store, f AnalyticsFilter) *bun.SelectQuery {
	sq := s.DB.NewSelect().TableExpr("licenses").ColumnExpr("id")
	return applyLicenseFilters(sq, f, "")
}

func (s *Store) GetAnalyticsSummary(ctx context.Context, f AnalyticsFilter) (*AnalyticsSummary, error) {
	var result struct {
		Total     int `bun:"total"`
		Active    int `bun:"active"`
		Trialing  int `bun:"trialing"`
		Expired   int `bun:"expired"`
		Canceled  int `bun:"canceled"`
		Suspended int `bun:"suspended"`
		Revoked   int `bun:"revoked"`
		PastDue   int `bun:"past_due"`
	}

	// Summary shows per-status distribution, so skip the status filter here
	// (filtering by status would make all other status counts 0, which is useless).
	summaryFilter := f
	summaryFilter.Status = ""

	q := s.DB.NewSelect().
		TableExpr("licenses").
		ColumnExpr("COUNT(*) AS total").
		ColumnExpr("COUNT(*) FILTER (WHERE status = 'active') AS active").
		ColumnExpr("COUNT(*) FILTER (WHERE status = 'trialing') AS trialing").
		ColumnExpr("COUNT(*) FILTER (WHERE status = 'expired') AS expired").
		ColumnExpr("COUNT(*) FILTER (WHERE status = 'canceled') AS canceled").
		ColumnExpr("COUNT(*) FILTER (WHERE status = 'suspended') AS suspended").
		ColumnExpr("COUNT(*) FILTER (WHERE status = 'revoked') AS revoked").
		ColumnExpr("COUNT(*) FILTER (WHERE status = 'past_due') AS past_due")
	q = applyLicenseFilters(q, summaryFilter, "")

	if err := q.Scan(ctx, &result); err != nil {
		return nil, err
	}

	aq := s.DB.NewSelect().TableExpr("activations").
		Where("license_id IN (?)", licenseSubquery(s, f))
	activations, err := aq.Count(ctx)
	if err != nil {
		return nil, err
	}

	sq := s.DB.NewSelect().TableExpr("seats").Where("removed_at IS NULL").
		Where("license_id IN (?)", licenseSubquery(s, f))
	seats, err := sq.Count(ctx)
	if err != nil {
		return nil, err
	}

	var avgActivations float64
	if result.Total > 0 {
		avgActivations = float64(activations) / float64(result.Total)
	}

	return &AnalyticsSummary{
		TotalLicenses:            result.Total,
		ActiveLicenses:           result.Active,
		TrialingLicenses:         result.Trialing,
		ExpiredLicenses:          result.Expired,
		CanceledLicenses:         result.Canceled,
		SuspendedLicenses:        result.Suspended,
		RevokedLicenses:          result.Revoked,
		PastDueLicenses:          result.PastDue,
		TotalActivations:         activations,
		TotalSeats:               seats,
		AvgActivationsPerLicense: avgActivations,
	}, nil
}

func (s *Store) GetLicenseBreakdown(ctx context.Context, f AnalyticsFilter, dimension string) ([]BreakdownItem, error) {
	var out []BreakdownItem

	switch dimension {
	case "status":
		q := s.DB.NewSelect().
			TableExpr("licenses").
			ColumnExpr("status AS key").
			ColumnExpr("status AS label").
			ColumnExpr("COUNT(*)::int AS count").
			GroupExpr("status").
			OrderExpr("count DESC")
		q = applyLicenseFilters(q, f, "")
		err := q.Scan(ctx, &out)
		return out, err

	case "plan":
		q := s.DB.NewSelect().
			TableExpr("licenses AS l").
			Join("JOIN plans AS p ON p.id = l.plan_id").
			Join("JOIN products AS pr ON pr.id = l.product_id").
			ColumnExpr("p.id AS key").
			ColumnExpr("pr.name || ' / ' || p.name AS label").
			ColumnExpr("COUNT(*)::int AS count").
			GroupExpr("p.id, p.name, pr.name").
			OrderExpr("count DESC")
		q = applyLicenseFilters(q, f, "l")
		err := q.Scan(ctx, &out)
		return out, err

	case "license_type":
		q := s.DB.NewSelect().
			TableExpr("licenses AS l").
			Join("JOIN plans AS p ON p.id = l.plan_id").
			ColumnExpr("p.license_type AS key").
			ColumnExpr("p.license_type AS label").
			ColumnExpr("COUNT(*)::int AS count").
			GroupExpr("p.license_type").
			OrderExpr("count DESC")
		q = applyLicenseFilters(q, f, "l")
		err := q.Scan(ctx, &out)
		return out, err

	default:
		return nil, fmt.Errorf("unsupported dimension: %s", dimension)
	}
}

func (s *Store) GetTopFeatureUsage(ctx context.Context, productID string, from, to time.Time, limit int) ([]FeatureUsageItem, error) {
	if limit <= 0 {
		limit = 10
	}

	var out []FeatureUsageItem
	q := s.DB.NewSelect().
		TableExpr("usage_events AS ue").
		ColumnExpr("ue.feature AS feature").
		ColumnExpr("COALESCE(SUM(ue.quantity), 0) AS total_usage").
		ColumnExpr("COUNT(DISTINCT ue.license_id)::int AS unique_users").
		GroupExpr("ue.feature").
		OrderExpr("total_usage DESC").
		Limit(limit)

	if productID != "" {
		q = q.Join("JOIN licenses AS l ON l.id = ue.license_id").
			Where("l.product_id = ?", productID)
	}
	if !from.IsZero() {
		q = q.Where("ue.recorded_at >= ?", from)
	}
	if !to.IsZero() {
		q = q.Where("ue.recorded_at <= ?", to)
	}

	err := q.Scan(ctx, &out)
	return out, err
}

func (s *Store) GetActivationTrend(ctx context.Context, productID string, from, to time.Time) ([]TrendPoint, error) {
	var out []TrendPoint
	q := s.DB.NewSelect().
		TableExpr("activations AS a").
		ColumnExpr("DATE(a.created_at)::text AS date").
		ColumnExpr("COUNT(*)::int AS count").
		GroupExpr("DATE(a.created_at)").
		OrderExpr("DATE(a.created_at) ASC")

	if productID != "" {
		q = q.Join("JOIN licenses AS l ON l.id = a.license_id").
			Where("l.product_id = ?", productID)
	}
	if !from.IsZero() {
		q = q.Where("a.created_at >= ?", from)
	}
	if !to.IsZero() {
		q = q.Where("a.created_at <= ?", to)
	}

	err := q.Scan(ctx, &out)
	return out, err
}

func (s *Store) TakeSnapshot(ctx context.Context, productID string, date time.Time) error {
	dateOnly := date.Truncate(24 * time.Hour)
	nextDay := dateOnly.Add(24 * time.Hour)

	total, _ := s.DB.NewSelect().Model((*model.License)(nil)).
		Where("product_id = ?", productID).Count(ctx)
	active, _ := s.DB.NewSelect().Model((*model.License)(nil)).
		Where("product_id = ? AND status = 'active'", productID).Count(ctx)
	newLic, _ := s.DB.NewSelect().Model((*model.License)(nil)).
		Where("product_id = ? AND created_at >= ? AND created_at < ?", productID, dateOnly, nextDay).Count(ctx)
	churned, _ := s.DB.NewSelect().Model((*model.License)(nil)).
		Where("product_id = ? AND status IN ('canceled','expired') AND updated_at >= ? AND updated_at < ?", productID, dateOnly, nextDay).Count(ctx)
	activations, _ := s.DB.NewSelect().Model((*model.Activation)(nil)).
		Where("license_id IN (SELECT id FROM licenses WHERE product_id = ?)", productID).Count(ctx)
	seats, _ := s.DB.NewSelect().Model((*model.Seat)(nil)).
		Where("license_id IN (SELECT id FROM licenses WHERE product_id = ?) AND removed_at IS NULL", productID).Count(ctx)

	var usageResult struct {
		Total int64 `bun:"total"`
	}
	_ = s.DB.NewSelect().Model((*model.UsageEvent)(nil)).
		ColumnExpr("COALESCE(SUM(quantity), 0) AS total").
		Where("license_id IN (SELECT id FROM licenses WHERE product_id = ?) AND recorded_at >= ? AND recorded_at < ?", productID, dateOnly, nextDay).
		Scan(ctx, &usageResult)

	snap := &model.AnalyticsSnapshot{
		Date:             dateOnly,
		ProductID:        productID,
		TotalLicenses:    total,
		ActiveLicenses:   active,
		NewLicenses:      newLic,
		Churned:          churned,
		TotalActivations: activations,
		TotalSeats:       seats,
		TotalUsage:       usageResult.Total,
	}
	return s.CreateAnalyticsSnapshot(ctx, snap)
}

func (s *Store) TakeAllSnapshots(ctx context.Context, date time.Time) error {
	products, err := s.ListProducts(ctx, "")
	if err != nil {
		return err
	}
	for _, p := range products {
		if err := s.TakeSnapshot(ctx, p.ID, date); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetGrowthMetrics(ctx context.Context, productID string) (*GrowthMetrics, error) {
	gm := &GrowthMetrics{}

	tq := s.DB.NewSelect().TableExpr("licenses")
	if productID != "" {
		tq = tq.Where("product_id = ?", productID)
	}
	total, err := tq.Count(ctx)
	if err != nil {
		return nil, err
	}

	nq := s.DB.NewSelect().TableExpr("licenses").
		Where("created_at >= now() - interval '30 days'")
	if productID != "" {
		nq = nq.Where("product_id = ?", productID)
	}
	newCount, err := nq.Count(ctx)
	if err != nil {
		return nil, err
	}
	gm.NewLast30d = newCount

	cq := s.DB.NewSelect().TableExpr("licenses").
		Where("status IN ('canceled','expired')").
		Where("updated_at >= now() - interval '30 days'")
	if productID != "" {
		cq = cq.Where("product_id = ?", productID)
	}
	churnedCount, err := cq.Count(ctx)
	if err != nil {
		return nil, err
	}
	gm.ChurnedLast30d = churnedCount

	if total > 0 {
		gm.NetGrowthRate = float64(newCount-churnedCount) / float64(total) * 100
	}

	ttq := s.DB.NewSelect().TableExpr("licenses AS l").
		Join("JOIN plans AS p ON p.id = l.plan_id").
		Where("p.license_type = 'trial'")
	if productID != "" {
		ttq = ttq.Where("l.product_id = ?", productID)
	}
	totalTrials, err := ttq.Count(ctx)
	if err != nil {
		return nil, err
	}
	gm.TotalTrials = totalTrials

	ctq := s.DB.NewSelect().TableExpr("licenses AS l").
		Join("JOIN plans AS p ON p.id = l.plan_id").
		Where("p.license_type = 'trial'").
		Where("l.status = 'active'")
	if productID != "" {
		ctq = ctq.Where("l.product_id = ?", productID)
	}
	convertedTrials, err := ctq.Count(ctx)
	if err != nil {
		return nil, err
	}
	gm.ConvertedTrials = convertedTrials

	if totalTrials > 0 {
		gm.TrialConversion = float64(convertedTrials) / float64(totalTrials) * 100
	}

	var avgResult struct {
		Avg float64 `bun:"avg"`
	}
	aq := s.DB.NewSelect().TableExpr("licenses").
		ColumnExpr("COALESCE(EXTRACT(EPOCH FROM AVG(now() - created_at))/86400, 0) AS avg")
	if productID != "" {
		aq = aq.Where("product_id = ?", productID)
	}
	if err := aq.Scan(ctx, &avgResult); err != nil {
		return nil, err
	}
	gm.AvgLicenseAgeDays = avgResult.Avg

	var medianResult struct {
		Median float64 `bun:"median"`
	}
	mq := s.DB.NewSelect().TableExpr("licenses").
		ColumnExpr("COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (now() - created_at))/86400), 0) AS median")
	if productID != "" {
		mq = mq.Where("product_id = ?", productID)
	}
	if err := mq.Scan(ctx, &medianResult); err != nil {
		return nil, err
	}
	gm.MedianLicenseAgeDays = medianResult.Median

	return gm, nil
}

func (s *Store) GetLicenseAgeDistribution(ctx context.Context, productID string) ([]LicenseAgeDistribution, error) {
	var out []LicenseAgeDistribution
	q := s.DB.NewSelect().
		TableExpr("licenses").
		ColumnExpr(`CASE
			WHEN now() - created_at < interval '8 days' THEN '0-7d'
			WHEN now() - created_at < interval '31 days' THEN '8-30d'
			WHEN now() - created_at < interval '91 days' THEN '31-90d'
			WHEN now() - created_at < interval '181 days' THEN '91-180d'
			WHEN now() - created_at < interval '366 days' THEN '181-365d'
			ELSE '365d+'
		END AS bucket`).
		ColumnExpr("COUNT(*)::int AS count").
		GroupExpr("bucket").
		OrderExpr("MIN(now() - created_at)")
	if productID != "" {
		q = q.Where("product_id = ?", productID)
	}
	err := q.Scan(ctx, &out)
	return out, err
}

func (s *Store) GetRecentActivity(ctx context.Context, productID string, limit int) ([]RecentActivity, error) {
	if limit <= 0 {
		limit = 20
	}
	var out []RecentActivity
	q := s.DB.NewSelect().
		TableExpr("audit_logs").
		ColumnExpr("id, entity, entity_id, action, actor_type, created_at").
		OrderExpr("created_at DESC").
		Limit(limit)
	err := q.Scan(ctx, &out)
	return out, err
}

// GetTopUsers returns the top customers by license count and usage.
// Excludes admin/owner users — they are platform operators, not customers.
func (s *Store) GetTopUsers(ctx context.Context, productID string, limit int) ([]TopUser, error) {
	if limit <= 0 {
		limit = 10
	}
	var out []TopUser
	q := s.DB.NewSelect().
		TableExpr("licenses AS l").
		ColumnExpr("l.email").
		ColumnExpr("COALESCE(l.user_id, '') AS user_id").
		ColumnExpr("COUNT(DISTINCT l.id)::int AS license_count").
		ColumnExpr("COUNT(DISTINCT l.id) FILTER (WHERE l.status = 'active')::int AS active_count").
		ColumnExpr("COALESCE(SUM(uc.total_usage), 0) AS total_usage").
		ColumnExpr("COUNT(DISTINCT a.id)::int AS activation_count").
		Join("LEFT JOIN (SELECT license_id, SUM(quantity) AS total_usage FROM usage_events GROUP BY license_id) uc ON uc.license_id = l.id").
		Join("LEFT JOIN activations a ON a.license_id = l.id").
		// Exclude admin users from customer rankings
		Where("l.email NOT IN (SELECT email FROM users WHERE role IN ('owner', 'admin'))").
		GroupExpr("l.email, l.user_id").
		OrderExpr("license_count DESC, total_usage DESC").
		Limit(limit)
	if productID != "" {
		q = q.Where("l.product_id = ?", productID)
	}
	err := q.Scan(ctx, &out)
	return out, err
}

func (s *Store) GetRetentionData(ctx context.Context, productID string, months int) ([]RetentionData, error) {
	if months <= 0 {
		months = 6
	}
	var out []RetentionData
	q := s.DB.NewSelect().
		TableExpr("analytics_snapshots").
		ColumnExpr("date_trunc('month', date)::text AS period").
		ColumnExpr("MAX(total_licenses)::int AS start_count").
		ColumnExpr("MAX(active_licenses)::int AS end_count").
		ColumnExpr(`CASE WHEN MAX(total_licenses) > 0
			THEN ROUND(MAX(active_licenses)::numeric / MAX(total_licenses) * 100, 1)
			ELSE 0
		END AS retention_pct`).
		ColumnExpr(`CASE WHEN MAX(total_licenses) > 0
			THEN ROUND((1 - MAX(active_licenses)::numeric / MAX(total_licenses)) * 100, 1)
			ELSE 0
		END AS churn_pct`).
		Where(fmt.Sprintf("date >= now() - interval '%d months'", months)).
		GroupExpr("date_trunc('month', date)").
		OrderExpr("period ASC")
	if productID != "" {
		q = q.Where("product_id = ?", productID)
	}
	err := q.Scan(ctx, &out)
	return out, err
}

type UserDetail struct {
	User          *model.User           `json:"user"`
	Licenses      []*model.License      `json:"licenses"`
	Subscriptions []*model.Subscription `json:"subscriptions"`
	TotalUsage    int64                 `json:"total_usage"`
	ActiveSeats   int                   `json:"active_seats"`
	Activations   int                   `json:"activations"`
	AuditLogs     []*model.AuditLog     `json:"recent_audit_logs"`
}

func (s *Store) GetUserDetail(ctx context.Context, userID string) (*UserDetail, error) {
	user, err := s.FindUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}

	detail := &UserDetail{User: user}

	var licenses []*model.License
	err = s.DB.NewSelect().Model(&licenses).
		Relation("Plan").Relation("Plan.Entitlements").
		Relation("Product").Relation("Activations").Relation("Seats").
		Where("license.user_id = ? OR license.email = ?", userID, user.Email).
		OrderExpr("license.created_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	detail.Licenses = licenses

	subs, err := s.ListSubscriptionsByUser(ctx, userID)
	if err == nil {
		detail.Subscriptions = subs
	}

	licenseIDs := make([]string, len(licenses))
	for i, l := range licenses {
		licenseIDs[i] = l.ID
	}

	if len(licenseIDs) > 0 {
		var usageResult struct {
			Total int64 `bun:"total"`
		}
		_ = s.DB.NewSelect().TableExpr("usage_events").
			ColumnExpr("COALESCE(SUM(quantity), 0) AS total").
			Where("license_id IN (?)", bun.In(licenseIDs)).
			Scan(ctx, &usageResult)
		detail.TotalUsage = usageResult.Total

		activations, _ := s.DB.NewSelect().TableExpr("activations").
			Where("license_id IN (?)", bun.In(licenseIDs)).
			Count(ctx)
		detail.Activations = activations
	}

	sq := s.DB.NewSelect().TableExpr("seats").
		Where("(user_id = ? OR email = ?)", userID, user.Email).
		Where("removed_at IS NULL")
	activeSeats, _ := sq.Count(ctx)
	detail.ActiveSeats = activeSeats

	var auditLogs []*model.AuditLog
	_ = s.DB.NewSelect().Model(&auditLogs).
		Where("actor_id = ?", userID).
		OrderExpr("created_at DESC").
		Limit(20).
		Scan(ctx)
	detail.AuditLogs = auditLogs

	if detail.Licenses == nil {
		detail.Licenses = []*model.License{}
	}
	if detail.Subscriptions == nil {
		detail.Subscriptions = []*model.Subscription{}
	}
	if detail.AuditLogs == nil {
		detail.AuditLogs = []*model.AuditLog{}
	}

	return detail, nil
}
