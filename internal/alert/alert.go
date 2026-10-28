// Package alert решает, когда о проблеме стоит сообщать человеку.
//
// Проверки идут каждые N секунд, но уведомление о падении должно приходить
// один раз, а не каждые N секунд. Эту задачу решает маленькая машина
// состояний: она не знает ни про HTTP, ни про Telegram, и поэтому
// тестируется без сети и без таймеров.
package alert

import (
	"context"
	"sync"

	"github.com/Hopfi3721/pingwatch/internal/monitor"
)

// Event — переход состояния, о котором нужно уведомить.
type Event string

const (
	EventDown Event = "down"
	EventUp   Event = "up"
)

// Notification — готовое к отправке сообщение о переходе.
type Notification struct {
	Event               Event
	Target              monitor.Target
	Result              monitor.Result
	PreviousStatus      monitor.Status
	ConsecutiveFailures int
}

// Notifier отправляет уведомление. Реализации: LogNotifier, TelegramNotifier,
// MultiNotifier.
type Notifier interface {
	Notify(ctx context.Context, notification Notification) error
}

type state struct {
	status   monitor.Status
	failures int
}

// StateMachine помнит состояние каждой цели и выдаёт уведомление только
// на переходе. Порог failures защищает от ложных тревог: один случайный
// таймаут не должен будить дежурного.
type StateMachine struct {
	threshold int

	mu     sync.Mutex
	states map[int64]*state
}

func NewStateMachine(threshold int) *StateMachine {
	if threshold < 1 {
		threshold = 1
	}

	return &StateMachine{
		threshold: threshold,
		states:    make(map[int64]*state),
	}
}

// Seed восстанавливает состояние после перезапуска. Если сервис упал,
// когда цель уже была down, после старта не нужно присылать "DOWN" заново:
// люди уже знают. Поэтому down-состояние сразу считается "порог набран".
func (m *StateMachine) Seed(targetID int64, status monitor.Status) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch status {
	case monitor.StatusDown:
		m.states[targetID] = &state{status: monitor.StatusDown, failures: m.threshold}
	case monitor.StatusUp:
		m.states[targetID] = &state{status: monitor.StatusUp}
	}
}

// Observe принимает результат проверки и сообщает, нужно ли уведомление.
// Второе значение false означает "ничего не произошло, шуметь незачем".
func (m *StateMachine) Observe(target monitor.Target, result monitor.Result) (Notification, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, ok := m.states[target.ID]
	if !ok {
		current = &state{status: monitor.StatusUnknown}
		m.states[target.ID] = current
	}

	previous := current.status

	if result.Status == monitor.StatusUp {
		current.status = monitor.StatusUp
		current.failures = 0

		if previous == monitor.StatusDown {
			return Notification{
				Event:          EventUp,
				Target:         target,
				Result:         result,
				PreviousStatus: previous,
			}, true
		}

		return Notification{}, false
	}

	current.failures++
	if current.status == monitor.StatusDown {
		return Notification{}, false
	}

	if current.failures < m.threshold {
		return Notification{}, false
	}

	current.status = monitor.StatusDown

	return Notification{
		Event:               EventDown,
		Target:              target,
		Result:              result,
		PreviousStatus:      previous,
		ConsecutiveFailures: current.failures,
	}, true
}

// Status возвращает текущее состояние цели (для тестов и диагностики).
func (m *StateMachine) Status(targetID int64) monitor.Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	if current, ok := m.states[targetID]; ok {
		return current.status
	}

	return monitor.StatusUnknown
}

// Forget удаляет цель из памяти. Вызывается, когда цель удалили,
// иначе карта состояний растёт вместе с числом удалённых целей.
func (m *StateMachine) Forget(targetID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.states, targetID)
}
