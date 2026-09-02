// Package health определяет небольшой контракт проверки готовности процесса.
package health

import "context"

// Status описывает состояние критических зависимостей. Applied — последняя
// применённая миграция по журналу базы, Latest — последняя миграция сборки;
// готовность подтверждается только при их совпадении (LR-BE-RM-022).
type Status struct {
	DatabaseMigration string
	Applied           string
	Latest            string
}

// Checker подтверждает доступность критических зависимостей и соответствие их
// схем запущенной сборке.
type Checker interface {
	Check(context.Context) (Status, error)
}
