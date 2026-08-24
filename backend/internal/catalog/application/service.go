// Package application coordinates tenant-authorized service catalog use cases.
package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"lidradar/backend/internal/catalog/domain"
)

var (
	ErrInvalid   = errors.New("invalid service catalog request")
	ErrForbidden = errors.New("service catalog permission denied")
	ErrNotFound  = errors.New("service catalog item not found")
	ErrConflict  = errors.New("service catalog item conflict")
)

const PermissionManage = "service.manage"

type Authorizer interface {
	Allowed(context.Context, string, string, string) (bool, error)
}

type IDs interface{ NewID() (string, error) }

type Service struct {
	repository domain.Repository
	authorizer Authorizer
	ids        IDs
	now        func() time.Time
}

func NewService(repository domain.Repository, authorizer Authorizer, ids IDs, now func() time.Time) Service {
	return Service{repository: repository, authorizer: authorizer, ids: ids, now: now}
}

type CreateCommand struct {
	Name       string
	LocationID *string
	PriceFrom  *string
	PriceTo    *string
	Currency   string
}

func (service Service) List(ctx context.Context, actorID, tenantID string) ([]domain.ServiceCatalogItem, error) {
	if err := service.requireManage(ctx, actorID, tenantID); err != nil {
		return nil, err
	}
	items, err := service.repository.List(ctx, tenantID)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return items, nil
}

func (service Service) Create(ctx context.Context, actorID, tenantID string, command CreateCommand) (domain.ServiceCatalogItem, error) {
	if err := service.requireManage(ctx, actorID, tenantID); err != nil {
		return domain.ServiceCatalogItem{}, err
	}
	if service.ids == nil || service.now == nil {
		return domain.ServiceCatalogItem{}, ErrInvalid
	}
	priceFrom, err := parseOptionalPrice(command.PriceFrom)
	if err != nil {
		return domain.ServiceCatalogItem{}, ErrInvalid
	}
	priceTo, err := parseOptionalPrice(command.PriceTo)
	if err != nil {
		return domain.ServiceCatalogItem{}, ErrInvalid
	}
	id, err := service.ids.NewID()
	if err != nil {
		return domain.ServiceCatalogItem{}, err
	}
	item, err := domain.NewServiceCatalogItem(
		id, tenantID, command.Name, command.LocationID, priceFrom, priceTo, command.Currency, service.now().UTC(),
	)
	if err != nil {
		return domain.ServiceCatalogItem{}, ErrInvalid
	}
	if err := service.repository.Create(ctx, tenantID, item); err != nil {
		return domain.ServiceCatalogItem{}, mapDomainError(err)
	}
	return item, nil
}

type OptionalString struct {
	Set   bool
	Value *string
}

type UpdateCommand struct {
	Name       *string
	LocationID OptionalString
	PriceFrom  OptionalString
	PriceTo    OptionalString
	Currency   *string
	Active     *bool
}

func (command UpdateCommand) Empty() bool {
	return command.Name == nil && !command.LocationID.Set && !command.PriceFrom.Set &&
		!command.PriceTo.Set && command.Currency == nil && command.Active == nil
}

func (service Service) Update(ctx context.Context, actorID, tenantID, itemID string, command UpdateCommand) (domain.ServiceCatalogItem, error) {
	if err := service.requireManage(ctx, actorID, tenantID); err != nil {
		return domain.ServiceCatalogItem{}, err
	}
	if strings.TrimSpace(itemID) == "" || command.Empty() || service.now == nil {
		return domain.ServiceCatalogItem{}, ErrInvalid
	}
	item, found, err := service.repository.Item(ctx, tenantID, itemID)
	if err != nil {
		return domain.ServiceCatalogItem{}, mapDomainError(err)
	}
	if !found {
		return domain.ServiceCatalogItem{}, ErrNotFound
	}
	if command.Name != nil {
		item.Name = strings.Join(strings.Fields(*command.Name), " ")
		item.NormalizedName = domain.NormalizeName(item.Name)
	}
	if command.LocationID.Set {
		item.LocationID = cleanOptionalID(command.LocationID.Value)
	}
	if command.PriceFrom.Set {
		item.PriceFrom, err = parseOptionalPrice(command.PriceFrom.Value)
		if err != nil {
			return domain.ServiceCatalogItem{}, ErrInvalid
		}
	}
	if command.PriceTo.Set {
		item.PriceTo, err = parseOptionalPrice(command.PriceTo.Value)
		if err != nil {
			return domain.ServiceCatalogItem{}, ErrInvalid
		}
	}
	if command.Currency != nil {
		item.Currency = strings.ToUpper(strings.TrimSpace(*command.Currency))
	}
	if command.Active != nil {
		item.Active = *command.Active
	}
	item.UpdatedAt = service.now().UTC()
	if item.Validate() != nil {
		return domain.ServiceCatalogItem{}, ErrInvalid
	}
	item, found, err = service.repository.Update(ctx, tenantID, itemID, item)
	if err != nil {
		return domain.ServiceCatalogItem{}, mapDomainError(err)
	}
	if !found {
		return domain.ServiceCatalogItem{}, ErrNotFound
	}
	return item, nil
}

func (service Service) Deactivate(ctx context.Context, actorID, tenantID, itemID string) error {
	if err := service.requireManage(ctx, actorID, tenantID); err != nil {
		return err
	}
	if strings.TrimSpace(itemID) == "" || service.now == nil {
		return ErrInvalid
	}
	found, err := service.repository.Deactivate(ctx, tenantID, itemID, service.now().UTC())
	if err != nil {
		return mapDomainError(err)
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

func (service Service) requireManage(ctx context.Context, actorID, tenantID string) error {
	if service.repository == nil || service.authorizer == nil || actorID == "" || tenantID == "" {
		return ErrForbidden
	}
	allowed, err := service.authorizer.Allowed(ctx, actorID, tenantID, PermissionManage)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

func parseOptionalPrice(raw *string) (*domain.Price, error) {
	if raw == nil {
		return nil, nil
	}
	price, err := domain.ParsePrice(*raw)
	if err != nil {
		return nil, err
	}
	return &price, nil
}

func cleanOptionalID(raw *string) *string {
	if raw == nil {
		return nil
	}
	cleaned := strings.TrimSpace(*raw)
	return &cleaned
}

func mapDomainError(err error) error {
	switch {
	case errors.Is(err, domain.ErrInvalid):
		return ErrInvalid
	case errors.Is(err, domain.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, domain.ErrConflict):
		return ErrConflict
	default:
		return err
	}
}
