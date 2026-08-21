// Package application coordinates Risk use cases through domain ports.
package application

import (
	"context"

	"lidradar/backend/internal/risk/domain"
)

// Register stores a Risk through the domain-owned persistence port.
// It is deliberately small: this reference module demonstrates dependency
// direction without introducing feature behavior ahead of its backlog task.
type Register struct {
	repository domain.Repository
}

// NewRegister constructs the reference use case.
func NewRegister(repository domain.Repository) Register {
	return Register{repository: repository}
}

// Execute stores risk. All public application operations accept a context;
// cancellation handling belongs to concrete feature implementations.
func (r Register) Execute(_ context.Context, risk domain.Risk) error {
	return r.repository.Save(risk)
}
