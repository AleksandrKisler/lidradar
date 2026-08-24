// Package domain owns the tenant-scoped service catalog and its exact price ranges.
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

var (
	ErrInvalid  = errors.New("invalid service catalog item")
	ErrNotFound = errors.New("service catalog item not found")
	ErrConflict = errors.New("service catalog item conflict")
)

var pricePattern = regexp.MustCompile(`^[0-9]{1,12}(?:\.[0-9]{1,2})?$`)

// Price is a non-negative NUMERIC(14,2)-compatible amount. It never uses
// binary floating point and is serialized as a JSON string.
type Price struct{ value decimal.Decimal }

func ParsePrice(raw string) (Price, error) {
	raw = strings.TrimSpace(raw)
	if !pricePattern.MatchString(raw) {
		return Price{}, ErrInvalid
	}
	value, err := decimal.NewFromString(raw)
	if err != nil || value.IsNegative() {
		return Price{}, ErrInvalid
	}
	return Price{value: value}, nil
}

func (price Price) String() string { return price.value.StringFixed(2) }

func (price Price) Decimal() decimal.Decimal { return price.value }

func (price Price) GreaterThan(other Price) bool { return price.value.GreaterThan(other.value) }

func (price Price) MarshalJSON() ([]byte, error) { return json.Marshal(price.String()) }

type ServiceCatalogItem struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"-"`
	LocationID     *string   `json:"locationId"`
	Name           string    `json:"name"`
	NormalizedName string    `json:"normalizedName"`
	PriceFrom      *Price    `json:"priceFrom"`
	PriceTo        *Price    `json:"priceTo"`
	Currency       string    `json:"currency"`
	Active         bool      `json:"active"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

func NewServiceCatalogItem(
	id, tenantID, name string,
	locationID *string,
	priceFrom, priceTo *Price,
	currency string,
	at time.Time,
) (ServiceCatalogItem, error) {
	item := ServiceCatalogItem{
		ID: id, TenantID: tenantID, LocationID: cleanOptionalID(locationID),
		Name: cleanName(name), PriceFrom: priceFrom, PriceTo: priceTo,
		Currency: strings.ToUpper(strings.TrimSpace(currency)), Active: true,
		CreatedAt: at.UTC(), UpdatedAt: at.UTC(),
	}
	if item.Currency == "" {
		item.Currency = "RUB"
	}
	item.NormalizedName = NormalizeName(item.Name)
	if at.IsZero() || item.Validate() != nil {
		return ServiceCatalogItem{}, ErrInvalid
	}
	return item, nil
}

func (item ServiceCatalogItem) Validate() error {
	if item.ID == "" || item.TenantID == "" || item.Name == "" || len(item.Name) > 200 ||
		item.Name != cleanName(item.Name) || item.NormalizedName != NormalizeName(item.Name) ||
		len(item.Currency) != 3 || item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	for _, character := range item.Currency {
		if character < 'A' || character > 'Z' {
			return ErrInvalid
		}
	}
	if item.LocationID != nil {
		locationID := strings.TrimSpace(*item.LocationID)
		if locationID == "" || locationID != *item.LocationID {
			return ErrInvalid
		}
	}
	if item.PriceFrom != nil && item.PriceTo != nil && item.PriceFrom.GreaterThan(*item.PriceTo) {
		return ErrInvalid
	}
	return nil
}

func NormalizeName(name string) string {
	return strings.ToLower(cleanName(name))
}

func cleanName(name string) string {
	return strings.Join(strings.Fields(name), " ")
}

func cleanOptionalID(id *string) *string {
	if id == nil {
		return nil
	}
	cleaned := strings.TrimSpace(*id)
	return &cleaned
}

// Repository requires a tenant ID for every access to tenant-owned catalog data.
type Repository interface {
	List(context.Context, string) ([]ServiceCatalogItem, error)
	Item(context.Context, string, string) (ServiceCatalogItem, bool, error)
	Create(context.Context, string, ServiceCatalogItem) error
	Update(context.Context, string, string, ServiceCatalogItem) (ServiceCatalogItem, bool, error)
	Deactivate(context.Context, string, string, time.Time) (bool, error)
}
