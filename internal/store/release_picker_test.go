package store_test

import (
	"context"
	"fmt"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"testing"
	"time"
)

func TestReleasePickerSearchAndMembership(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)
	for i := 0; i < 62; i++ {
		version := fmt.Sprintf("1.0.%d", 100+i)
		if i == 0 {
			version = "1.0.1"
		}
		r := &model.Release{ProductID: lic.ProductID, Version: version, Channel: "stable", Status: "published", CreatedAt: time.Now().Add(time.Duration(i) * time.Second)}
		if err := s.CreateRelease(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	f := store.ReleaseFilter{ProductID: lic.ProductID, Channel: "stable", Status: "published", Search: "1.0.1", Limit: 50}
	rows, err := s.ListReleases(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 50 || rows[0].Version != "1.0.1" {
		t.Fatalf("exact match must precede a full prefix page: %d %v", len(rows), rows[0].Version)
	}
	count, err := s.CountReleases(ctx, f)
	if err != nil || count != 62 {
		t.Fatalf("filtered count %d %v", count, err)
	}
	f.Search = ""
	f.Version = "1.0.1"
	f.Limit = 1
	rows, err = s.ListReleases(ctx, f)
	if err != nil || len(rows) != 1 || rows[0].Version != "1.0.1" {
		t.Fatalf("exact membership: %v %v", rows, err)
	}
	f.Version = "9.9.9"
	count, err = s.CountReleases(ctx, f)
	if err != nil || count != 0 {
		t.Fatalf("negative membership: %d %v", count, err)
	}
	f.Version = ""
	f.Search = "%"
	count, err = s.CountReleases(ctx, f)
	if err != nil || count != 0 {
		t.Fatalf("literal search: %d %v", count, err)
	}
	latest, err := s.LatestPublishedVersions(ctx, lic.ProductID)
	if err != nil || len(latest) != 1 || latest[0].Version != "1.0.161" {
		t.Fatalf("complete semver max: %v %v", latest, err)
	}
}
