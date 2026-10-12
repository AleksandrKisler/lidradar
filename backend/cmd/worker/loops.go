package main

import (
	"context"
	"sync"
	"time"
)

// pacing задаёт паузы цепочки: idle — когда очередь пуста, failure — после ошибки шага.
type pacing struct {
	idle, failure time.Duration
}

var workerPacing = pacing{idle: idleInterval, failure: errorInterval}

// errorLogger — часть журнала, нужная цепочкам; *slog.Logger её реализует.
type errorLogger interface {
	Error(message string, arguments ...any)
}

// loop — независимая цепочка воркера (исходящий журнал, фоновые задания, доставка уведомлений, диагностика). Шаг берёт и обрабатывает
// не более одного элемента своей очереди и сообщает, было ли что-то сделано.
//
// Цепочки не ждут друг друга. Раньше три шага шли по очереди в одном цикле, и вызов Telegram с таймаутом 10 с задерживал разбор сообщений и
// вычисление рисков: при недоступном Telegram каждая доставка отнимала у фоновых заданий десять секунд. Теперь медленная доставка
// задерживает только доставки. Арендатор у каждой цепочки свой (ownerID+":outbox", ":jobs", ":notifications"), а элементы берутся
// с арендой из PostgreSQL, поэтому параллельная работа безопасна так же, как работа нескольких воркеров.
type loop struct {
	event   string // событие журнала при ошибке шага
	message string // сообщение журнала при ошибке шага
	step    func(context.Context) (bool, error)
	// every, если задан, задаёт паузу после каждого шага независимо от результата (диагностика раз в минуту).
	every time.Duration
}

// runLoops запускает цепочки и возвращается, когда все они остановились после отмены контекста.
func runLoops(ctx context.Context, logger errorLogger, pace pacing, loops ...loop) {
	var group sync.WaitGroup
	for _, chain := range loops {
		group.Add(1)
		go func(chain loop) {
			defer group.Done()
			chain.run(ctx, logger, pace)
		}(chain)
	}
	group.Wait()
}

func (chain loop) run(ctx context.Context, logger errorLogger, pace pacing) {
	for ctx.Err() == nil {
		progressed, err := chain.step(ctx)
		if err != nil && ctx.Err() == nil {
			logger.Error(chain.message, "event", chain.event, "error", err)
		}
		if ctx.Err() != nil {
			return
		}
		pause := time.Duration(0)
		switch {
		case chain.every > 0:
			pause = chain.every
		case err != nil:
			pause = pace.failure
		case !progressed:
			pause = pace.idle
		}
		if pause > 0 && !wait(ctx, pause) {
			return
		}
	}
}
