package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// SettingMaintenanceFeatures is the operator's confirmation that every
// replica runs a version that understands the maintenance period.
// Elapsed time cannot prove that and replicas on the previous version
// leave no trace in the database, so everything that depends on it —
// gated feeds, bounded update periods, renewals on sale, manual
// cutoffs — stays refused until an admin turns this on. It is theirs
// to turn off again before rolling back to a version without it: a
// replica that predates the feature reads none of this, so nothing
// here can notice one.
const SettingMaintenanceFeatures = "maintenance_features_enabled"

// SettingFeedURLTTLBound is the longest lifetime this install is
// known to have signed public feed links with. Every start raises it
// to the replica's own STORAGE_FEED_URL_TTL and every drain check
// reads it, so a replica configured with a shorter one cannot decide
// that links another replica signed for longer have expired — nor can
// a restart after the value was lowered.
//
// It is deliberately not a guess about the past: an install upgrading
// into this build records what it runs now, and what an earlier build
// signed links with is a question only the operator can answer (see
// the migration).
//
// Lowering it is allowed and is an ordered operation: put the shorter
// STORAGE_FEED_URL_TTL on every replica first, let the rollout
// finish, wait out the links signed under the old bound, and only
// then lower this. While a replica with the longer TTL is still
// running, its next start raises it again — which is the point.
const SettingFeedURLTTLBound = "feed_url_ttl_bound"

type Setting struct {
	bun.BaseModel `bun:"table:settings"`
	Key           string `bun:",pk" json:"key"`
	Value         string `json:"value"`
}

// MaintenanceFeaturesEnabled reports whether the switch is on. A
// missing row reads as off; a read failure is an error, never an
// open gate.
func (s *Store) MaintenanceFeaturesEnabled(ctx context.Context) (bool, error) {
	v, err := s.GetSetting(ctx, SettingMaintenanceFeatures)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "true", nil
}

// MaintenanceFeaturesEnabledForWriteIn reads the switch on the
// caller's transaction and holds the row until it commits, so a write
// that depends on the answer and an operator switching it off cannot
// both go through. FOR SHARE, not FOR UPDATE: any number of writes
// may read it at once, while the settings update — an ordinary UPDATE
// of that row — waits for them.
//
// On the caller's transaction for a second reason: a read through the
// pool while a transaction is open needs a second connection, and
// enough concurrent writers doing that exhaust the pool with each
// holding one and waiting for another.
func MaintenanceFeaturesEnabledForWriteIn(ctx context.Context, tx bun.IDB) (bool, error) {
	var v string
	err := tx.NewRaw("SELECT value FROM settings WHERE key = ? FOR SHARE", SettingMaintenanceFeatures).Scan(ctx, &v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "true", nil
}

// MaxDurationSetting bounds a duration an operator may record. For
// the feed URL bound it is the seven days S3 and compatible stores
// cap a presigned URL at; anything beyond is a typo, and a typo here
// stalls every update period the install sells.
const MaxDurationSetting = 7 * 24 * time.Hour

// DurationSettingIn reads a duration setting on a caller's connection
// or transaction. Only a missing row reads as fallback — that is "not
// recorded yet". A row that is there but says nothing usable (empty,
// not a duration, zero, negative, longer than MaxDurationSetting) is
// an error, and so is any read failure: this is what a drain is
// measured against, and falling back to the caller's own, possibly
// shorter, value would end that wait early without anyone noticing.
func DurationSettingIn(ctx context.Context, db bun.IDB, key string, fallback time.Duration) (time.Duration, error) {
	v, err := getSettingIn(ctx, db, key)
	if errors.Is(err, sql.ErrNoRows) {
		return fallback, nil
	}
	if err != nil {
		return 0, err
	}
	return ParseDurationSetting(key, v)
}

// ParseDurationSetting is the one rule for what a duration setting may
// say, shared by the readers above and the admin API that writes it.
func ParseDurationSetting(key, v string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(v))
	switch {
	case err != nil:
		return 0, fmt.Errorf("setting %s is not a duration (%q): %w", key, v, err)
	case d <= 0:
		return 0, fmt.Errorf("setting %s must be a positive duration, got %s", key, d)
	case d > MaxDurationSetting:
		return 0, fmt.Errorf("setting %s is longer than %s: %s", key, MaxDurationSetting, d)
	}
	return d, nil
}

// RaiseDurationSetting records d unless a longer value is already
// recorded, and returns whichever is longer. Two replicas starting at
// once must not lose the longer one, so the read and the write happen
// in one transaction with the row locked between them: the second
// writer waits, then compares against what the first committed. It
// only ever raises — an operator lowering the value by hand is left
// alone until the next start of a replica configured higher.
//
// A recorded value that is not a usable duration is an error, not
// something to replace: it may stand for a lifetime longer than this
// replica's, so the caller stops instead of starting with a shorter
// one.
func (s *Store) RaiseDurationSetting(ctx context.Context, key string, d time.Duration) (time.Duration, error) {
	if _, err := ParseDurationSetting(key, d.String()); err != nil {
		return 0, err
	}
	out := d
	err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		// Claim the row if it is missing. A concurrent inserter of the
		// same key blocks here and finds its row below.
		if _, err := tx.NewInsert().Model(&Setting{Key: key, Value: d.String()}).
			On("CONFLICT (key) DO NOTHING").Exec(ctx); err != nil {
			return err
		}
		var current string
		if err := tx.NewRaw("SELECT value FROM settings WHERE key = ? FOR UPDATE", key).
			Scan(ctx, &current); err != nil {
			return err
		}
		// A row that is there but unusable is not something to
		// overwrite: it may have stood for a lifetime longer than
		// this replica's, and replacing it would quietly shorten the
		// wait. An operator fixes the value; until then no replica
		// starts.
		have, err := ParseDurationSetting(key, current)
		if err != nil {
			return err
		}
		if have >= d {
			out = have
			return nil
		}
		_, err = tx.NewRaw("UPDATE settings SET value = ? WHERE key = ?", d.String(), key).Exec(ctx)
		return err
	})
	if err != nil {
		return 0, err
	}
	return out, nil
}

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	return getSettingIn(ctx, s.DB, key)
}

func getSettingIn(ctx context.Context, db bun.IDB, key string) (string, error) {
	setting := new(Setting)
	err := db.NewSelect().Model(setting).Where("key = ?", key).Scan(ctx)
	if err != nil {
		return "", err
	}
	return setting.Value, nil
}

func (s *Store) GetSettings(ctx context.Context) (map[string]string, error) {
	var settings []Setting
	err := s.DB.NewSelect().Model(&settings).Scan(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(settings))
	for _, s := range settings {
		result[s.Key] = s.Value
	}
	return result, nil
}

// GetPublicSettings returns settings safe for unauthenticated access (site branding).
func (s *Store) GetPublicSettings(ctx context.Context) (map[string]string, error) {
	publicKeys := []string{"site_name", "timezone", "brand_color", "language", "logo_url"}
	var settings []Setting
	err := s.DB.NewSelect().Model(&settings).
		Where("key IN (?)", bun.List(publicKeys)).Scan(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(settings))
	for _, s := range settings {
		result[s.Key] = s.Value
	}
	return result, nil
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	encoded, err := s.encodeSettingValue(key, value)
	if err != nil {
		return err
	}
	setting := &Setting{Key: key, Value: encoded}
	_, err = s.DB.NewInsert().Model(setting).
		On("CONFLICT (key) DO UPDATE").
		Set("value = EXCLUDED.value").
		Exec(ctx)
	return err
}

// DeleteSetting removes a row outright. Used to clear stored secrets:
// blanking the value would leave a row that reads as "configured but
// empty", which is a different state from "never set".
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.DB.NewDelete().Model((*Setting)(nil)).Where("key = ?", key).Exec(ctx)
	return err
}

// SetSettings writes all keys in one transaction: callers that store
// related state (an endpoint id with its secret, a rotation with the
// previous secret) never leave half of it behind.
func (s *Store) SetSettings(ctx context.Context, settings map[string]string) error {
	if len(settings) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range settings {
		encoded, encErr := s.encodeSettingValue(key, value)
		if encErr != nil {
			return encErr
		}
		setting := &Setting{Key: key, Value: encoded}
		if _, err := tx.NewInsert().Model(setting).
			On("CONFLICT (key) DO UPDATE").
			Set("value = EXCLUDED.value").
			Exec(ctx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ─── Secret settings ───
//
// A few settings hold credentials (mail API keys, the SMTP password).
// They are stored encrypted at rest with the same master key that
// protects license keys, and the settings API never returns them to
// the UI (see settingsSecret in the handler). This keeps a DB dump or
// a read of the settings table from handing over live credentials.

// ErrSecretEncryptionUnavailable is returned when a secret setting is
// saved on an install with no master key. Storing it plaintext would
// leak a long-lived credential, so the write is refused instead.
var ErrSecretEncryptionUnavailable = errors.New(
	"cannot store a secret without an encryption key: set RELEASE_KEY_ENCRYPTION_KEY")

var settingSecretKeys = map[string]bool{
	"cloudflare_api_token": true,
	"smtp_password":        true,
}

const settingEncPrefix = "enc:v1:"

// encodeSettingValue encrypts a secret setting's value for storage.
// Non-secret keys, empty values, and installs without an encryption
// key pass through unchanged; the value is still redacted from the UI.
func (s *Store) encodeSettingValue(key, value string) (string, error) {
	if value == "" || !settingSecretKeys[key] {
		return value, nil
	}
	// A secret must never be written in the clear. Without a master key
	// there is nowhere safe to put it, so refuse rather than silently
	// store plaintext an admin believes is encrypted. Same if the
	// encryption itself fails.
	if s.LicenseKeyAEAD == nil {
		return "", ErrSecretEncryptionUnavailable
	}
	ct, err := s.LicenseKeyAEAD.Encrypt([]byte(value), []byte("setting:"+key))
	if err != nil {
		return "", fmt.Errorf("encrypt %s: %w", key, err)
	}
	return settingEncPrefix + base64.StdEncoding.EncodeToString(ct), nil
}

// GetSecretSetting returns the decrypted plaintext of a secret setting,
// or "" if it is not set. A value stored before encryption was enabled
// (no prefix) is returned as-is, so turning encryption on does not
// strand existing config.
func (s *Store) GetSecretSetting(ctx context.Context, key string) (string, error) {
	raw, err := s.GetSetting(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil // not set
	}
	if err != nil {
		return "", err // a real read error must not look like "unset"
	}
	if raw == "" {
		return "", nil
	}
	if !strings.HasPrefix(raw, settingEncPrefix) {
		return raw, nil // stored before encryption was enabled
	}
	// A prefixed value is ciphertext. Without the key it cannot be
	// decrypted, and returning the ciphertext as if it were the secret
	// would silently feed "enc:v1:..." to SMTP AUTH or Cloudflare and
	// fail every send while the status still reads "configured". Refuse.
	if s.LicenseKeyAEAD == nil {
		return "", ErrSecretEncryptionUnavailable
	}
	ct, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, settingEncPrefix))
	if err != nil {
		return "", err
	}
	pt, err := s.LicenseKeyAEAD.Decrypt(ct, []byte("setting:"+key))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
