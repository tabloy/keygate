package store

import (
	"context"

	"github.com/uptrace/bun"
)

type Setting struct {
	bun.BaseModel `bun:"table:settings"`
	Key           string `bun:",pk" json:"key"`
	Value         string `json:"value"`
}

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	setting := new(Setting)
	err := s.DB.NewSelect().Model(setting).Where("key = ?", key).Scan(ctx)
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
		Where("key IN (?)", bun.In(publicKeys)).Scan(ctx)
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
	setting := &Setting{Key: key, Value: value}
	_, err := s.DB.NewInsert().Model(setting).
		On("CONFLICT (key) DO UPDATE").
		Set("value = EXCLUDED.value").
		Exec(ctx)
	return err
}

func (s *Store) SetSettings(ctx context.Context, settings map[string]string) error {
	for key, value := range settings {
		if err := s.SetSetting(ctx, key, value); err != nil {
			return err
		}
	}
	return nil
}
