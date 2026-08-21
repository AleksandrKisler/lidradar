// Package domain owns the Risk aggregate and its business invariants.
package domain

// Risk is the reference aggregate used to demonstrate module boundaries.
// Feature tasks will add its business state and invariants.
type Risk struct{}

// Repository is the persistence port owned by the Risk domain.
type Repository interface {
	Save(Risk) error
}
