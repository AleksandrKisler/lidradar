// Package infrastructure supplies adapters for Risk domain ports.
package infrastructure

import "lidradar/backend/internal/risk/domain"

// MemoryRepository is a compile-time reference adapter. Production
// persistence will be introduced by the relevant PostgreSQL feature task.
type MemoryRepository struct{}

// Save implements domain.Repository.
func (MemoryRepository) Save(domain.Risk) error { return nil }

var _ domain.Repository = MemoryRepository{}
