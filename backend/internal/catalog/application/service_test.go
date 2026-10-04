package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/catalog/domain"
)

type testRepository struct {
	items map[string]domain.ServiceCatalogItem
}

func newTestRepository() *testRepository {
	return &testRepository{items: map[string]domain.ServiceCatalogItem{}}
}

func (repository *testRepository) List(_ context.Context, tenantID string) ([]domain.ServiceCatalogItem, error) {
	items := make([]domain.ServiceCatalogItem, 0)
	for _, item := range repository.items {
		if item.TenantID == tenantID {
			items = append(items, item)
		}
	}
	return items, nil
}

func (repository *testRepository) Item(_ context.Context, tenantID, itemID string) (domain.ServiceCatalogItem, bool, error) {
	item, found := repository.items[itemID]
	if !found || item.TenantID != tenantID {
		return domain.ServiceCatalogItem{}, false, nil
	}
	return item, true, nil
}

func (repository *testRepository) Create(_ context.Context, tenantID string, item domain.ServiceCatalogItem) error {
	if tenantID != item.TenantID {
		return domain.ErrInvalid
	}
	repository.items[item.ID] = item
	return nil
}

func (repository *testRepository) Update(_ context.Context, tenantID, itemID string, item domain.ServiceCatalogItem) (domain.ServiceCatalogItem, bool, error) {
	if existing, found := repository.items[itemID]; !found || existing.TenantID != tenantID {
		return domain.ServiceCatalogItem{}, false, nil
	}
	repository.items[itemID] = item
	return item, true, nil
}

func (repository *testRepository) Deactivate(_ context.Context, tenantID, itemID string, at time.Time) (bool, error) {
	item, found := repository.items[itemID]
	if !found || item.TenantID != tenantID {
		return false, nil
	}
	item.Active = false
	item.UpdatedAt = at
	repository.items[itemID] = item
	return true, nil
}

type testAuthorizer struct{ allowed bool }

func (authorizer testAuthorizer) Allowed(_ context.Context, actorID, tenantID, permission string) (bool, error) {
	return authorizer.allowed && actorID == "owner" && tenantID == "tenant" && permission == PermissionManage, nil
}

type testIDs struct{}

func (testIDs) NewID() (string, error) { return "service-id", nil }

func TestOwnerCreatesUpdatesClearsPriceAndDeactivates(t *testing.T) {
	repository := newTestRepository()
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	service := NewService(repository, testAuthorizer{allowed: true}, testIDs{}, func() time.Time { return now })
	from, to := "1000", "1500.50"

	item, err := service.Create(context.Background(), "owner", "tenant", CreateCommand{
		Name: "  Ceramic   coating ", PriceFrom: &from, PriceTo: &to, Currency: "rub",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if item.NormalizedName != "ceramic coating" || item.PriceFrom.String() != "1000.00" || item.PriceTo.String() != "1500.50" {
		t.Fatalf("Create() item = %#v", item)
	}

	name := "Premium coating"
	item, err = service.Update(context.Background(), "owner", "tenant", item.ID, UpdateCommand{
		Name: &name, PriceFrom: OptionalString{Set: true}, PriceTo: OptionalString{Set: true},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if item.PriceFrom != nil || item.PriceTo != nil || item.NormalizedName != "premium coating" {
		t.Fatalf("Update() item = %#v", item)
	}
	if err := service.Deactivate(context.Background(), "owner", "tenant", item.ID); err != nil {
		t.Fatalf("Deactivate() error = %v", err)
	}
	if repository.items[item.ID].Active {
		t.Fatal("item remains active")
	}
}

func TestServiceRejectsPermissionAndInvalidRanges(t *testing.T) {
	now := time.Now()
	from, to := "10.01", "10.00"
	denied := NewService(newTestRepository(), testAuthorizer{}, testIDs{}, func() time.Time { return now })
	if _, err := denied.Create(context.Background(), "manager", "tenant", CreateCommand{Name: "Service"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("denied Create() error = %v", err)
	}

	allowed := NewService(newTestRepository(), testAuthorizer{allowed: true}, testIDs{}, func() time.Time { return now })
	if _, err := allowed.Create(context.Background(), "owner", "tenant", CreateCommand{Name: "Service", PriceFrom: &from, PriceTo: &to}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid Create() error = %v", err)
	}
}

func TestServiceNameLengthCountsUnicodeAfterWhitespaceCleanup(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  string // Empty means invalid.
	}{
		{name: "empty"},
		{name: "whitespace", input: " \t\u0085\u00a0\u2003 "},
		{name: "ascii limit", input: strings.Repeat("A", 200), want: strings.Repeat("A", 200)},
		{name: "ascii over limit", input: strings.Repeat("A", 201)},
		{name: "QA-04 cyrillic", input: strings.Repeat("Я", 101), want: strings.Repeat("Я", 101)},
		{name: "cyrillic limit", input: strings.Repeat("Я", 200), want: strings.Repeat("Я", 200)},
		{name: "cyrillic over limit", input: strings.Repeat("Я", 201)},
		{name: "emoji limit", input: strings.Repeat("🚗", 200), want: strings.Repeat("🚗", 200)},
		{name: "emoji over limit", input: strings.Repeat("🚗", 201)},
		{name: "mixed limit", input: strings.Repeat("Я🚗", 100), want: strings.Repeat("Я🚗", 100)},
		{name: "combining code points", input: strings.Repeat("е\u0301", 100), want: strings.Repeat("е\u0301", 100)},
		{name: "combining over limit", input: strings.Repeat("е\u0301", 100) + "Я"},
		{name: "clean before counting", input: "\u0085" + strings.Repeat("Я", 99) + " \t\u00a0\u2003 " + strings.Repeat("Я", 100) + "\n", want: strings.Repeat("Я", 99) + " " + strings.Repeat("Я", 100)},
	} {
		for _, operation := range []string{"create", "update"} {
			t.Run(test.name+"/"+operation, func(t *testing.T) {
				repository := newTestRepository()
				service := NewService(repository, testAuthorizer{allowed: true}, testIDs{}, time.Now)
				ctx := context.Background()
				var err error
				wantStored := test.want
				if operation == "create" {
					_, err = service.Create(ctx, "owner", "tenant", CreateCommand{Name: test.input})
				} else {
					if _, err := service.Create(ctx, "owner", "tenant", CreateCommand{Name: "Мойка"}); err != nil {
						t.Fatal(err)
					}
					_, err = service.Update(ctx, "owner", "tenant", "service-id", UpdateCommand{Name: &test.input})
					if test.want == "" {
						wantStored = "Мойка"
					}
				}
				if test.want == "" {
					if !errors.Is(err, ErrInvalid) {
						t.Fatalf("%s() error = %v, want ErrInvalid", operation, err)
					}
				} else if err != nil {
					t.Fatalf("%s() error = %v", operation, err)
				}
				stored, found := repository.items["service-id"]
				if found != (wantStored != "") || stored.Name != wantStored {
					t.Fatalf("stored name = %q, found %v, want %q", stored.Name, found, wantStored)
				}
				if found && stored.NormalizedName != domain.NormalizeName(wantStored) {
					t.Fatalf("normalized name = %q", stored.NormalizedName)
				}
			})
		}
	}
}
