// Package scheduler превращает список целей в поток проверок.
//
// Устройство простое и намеренно скучное: один тикер решает, кому пора
// проверяться, буферизованный канал сглаживает пики, пул воркеров выполняет
// проверки параллельно. Медленная цель не задерживает остальные, а
// переполненный канал приводит к пропуску тика, а не к росту очереди.
package scheduler

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/Hopfi3721/pingwatch/internal/alert"
	"github.com/Hopfi3721/pingwatch/internal/monitor"
	"github.com/Hopfi3721/pingwatch/internal/storage"
)

// Checker — то, что планировщику нужно от проверяльщика. Интерфейс объявлен
// здесь, у потребителя: тесты подставляют фейк, не поднимая сеть.
type Checker interface {
	Check(ctx context.Context, target monitor.Target) monitor.Result
}

type Config struct {
	// Workers — сколько проверок выполняется одновременно.
	Workers int
	// Tick — как часто планировщик смотрит, кому пора проверяться.
	Tick time.Duration
	// Retention — сколько последних результатов хранить на цель.
	Retention int
	// PruneEvery — как часто чистить историю.
	PruneEvery time.Duration
}

func (c *Config) setDefaults() {
	if c.Workers < 1 {
		c.Workers = 1
	}
	if c.Tick <= 0 {
		c.Tick = time.Second
	}
	if c.Retention < 1 {
		c.Retention = 5000
	}
	if c.PruneEvery <= 0 {
		c.PruneEvery = time.Hour
	}
}

type Scheduler struct {
	store    storage.Store
	checker  Checker
	alerts   *alert.StateMachine
	notifier alert.Notifier
	logger   *log.Logger
	cfg      Config
	now      func() time.Time

	mu        sync.Mutex
	inflight  map[int64]bool
	lastCheck map[int64]time.Time
}

func New(
	store storage.Store,
	checker Checker,
	alerts *alert.StateMachine,
	notifier alert.Notifier,
	logger *log.Logger,
	cfg Config,
) *Scheduler {
	cfg.setDefaults()
	if logger == nil {
		logger = log.Default()
	}

	return &Scheduler{
		store:     store,
		checker:   checker,
		alerts:    alerts,
		notifier:  notifier,
		logger:    logger,
		cfg:       cfg,
		now:       time.Now,
		inflight:  make(map[int64]bool),
		lastCheck: make(map[int64]time.Time),
	}
}

// Run блокируется до отмены контекста. Перед возвратом дожидается
// воркеров: нельзя закрывать базу, пока проверка ещё пишет результат.
func (s *Scheduler) Run(ctx context.Context) {
	jobs := make(chan monitor.Target, s.cfg.Workers*2)

	var workers sync.WaitGroup
	for i := 0; i < s.cfg.Workers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for target := range jobs {
				s.process(ctx, target)
			}
		}()
	}

	tick := time.NewTicker(s.cfg.Tick)
	defer tick.Stop()

	prune := time.NewTicker(s.cfg.PruneEvery)
	defer prune.Stop()

	// Первый обход сразу: после старта не нужно ждать целый тик.
	s.dispatch(ctx, jobs)

	for {
		select {
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return
		case <-tick.C:
			s.dispatch(ctx, jobs)
		case <-prune.C:
			s.prune(ctx)
		}
	}
}

// dispatch ставит в очередь цели, которым пора проверяться.
func (s *Scheduler) dispatch(ctx context.Context, jobs chan<- monitor.Target) {
	targets, err := s.store.ListTargets(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Printf("scheduler: list targets: %v", err)
		}
		return
	}

	alive := make(map[int64]struct{}, len(targets))
	now := s.now()

	for _, target := range targets {
		alive[target.ID] = struct{}{}

		if !target.Enabled {
			continue
		}
		if !s.claim(target, now) {
			continue
		}

		select {
		case jobs <- target:
		case <-ctx.Done():
			s.release(target.ID)
			return
		default:
			// Очередь полна: лучше пропустить тик, чем копить долг
			// и потом проверять одно и то же пачкой.
			s.release(target.ID)
		}
	}

	s.forgetMissing(alive)
}

// claim помечает цель как "в работе", если ей действительно пора.
// Проверка "пора или нет" и установка флага — под одним мьютексом,
// иначе два тика подряд могут отправить одну цель дважды.
func (s *Scheduler) claim(target monitor.Target, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.inflight[target.ID] {
		return false
	}
	if last, ok := s.lastCheck[target.ID]; ok && now.Sub(last) < target.Interval() {
		return false
	}

	s.inflight[target.ID] = true
	s.lastCheck[target.ID] = now

	return true
}

func (s *Scheduler) release(targetID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.inflight, targetID)
}

// forgetMissing убирает из памяти состояния удалённых целей.
func (s *Scheduler) forgetMissing(alive map[int64]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id := range s.lastCheck {
		if _, ok := alive[id]; !ok {
			delete(s.lastCheck, id)
			delete(s.inflight, id)
			s.alerts.Forget(id)
		}
	}
}

func (s *Scheduler) process(ctx context.Context, target monitor.Target) {
	defer s.release(target.ID)

	checkCtx, cancel := context.WithTimeout(ctx, target.Timeout())
	defer cancel()

	result := s.checker.Check(checkCtx, target)

	// Отмена во время проверки — это остановка сервиса, а не падение цели.
	// Такие результаты не сохраняем, иначе в истории появится ложный DOWN.
	if ctx.Err() != nil {
		return
	}

	// Результат уже получен — сохраняем его, даже если сервис в этот момент
	// останавливают: терять данные проверки незачем.
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer saveCancel()

	if err := s.store.SaveResult(saveCtx, &result); err != nil {
		s.logger.Printf("scheduler: save result for %q: %v", target.Name, err)
		return
	}

	notification, ok := s.alerts.Observe(target, result)
	if !ok {
		return
	}

	notifyCtx, notifyCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer notifyCancel()

	if err := s.notifier.Notify(notifyCtx, notification); err != nil {
		s.logger.Printf("scheduler: notify %s for %q: %v", notification.Event, target.Name, err)
	}
}

func (s *Scheduler) prune(ctx context.Context) {
	deleted, err := s.store.PruneResults(ctx, s.cfg.Retention)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Printf("scheduler: prune results: %v", err)
		}
		return
	}
	if deleted > 0 {
		s.logger.Printf("scheduler: pruned %d old results", deleted)
	}
}
