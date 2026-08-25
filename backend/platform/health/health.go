// Package health определяет небольшой контракт проверки готовности процесса.
package health

import "context"

type Status struct {
	DatabaseMigration string
}

// Checker подтверждает доступность критических зависимостей и соответствие их
// схем запущенной сборке.
type Checker interface {
	Check(context.Context) (Status, error)
}
