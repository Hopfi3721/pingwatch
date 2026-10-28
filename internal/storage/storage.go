// Package storage описывает хранилище pingwatch и его SQLite-реализацию.
//
// Store — единственная граница между логикой и базой. Благодаря этому
// Postgres-адаптер добавляется без изменения планировщика и HTTP-слоя.
package storage

import (
	"context"

	"github.com/Hopfi3721/pingwatch/internal/monitor"
)

// Store — то, что планировщику и API нужно от базы.
type Store interface {
	CreateTarget(ctx context.Context, target *monitor.Target) error
	GetTarget(ctx context.Context, id int64) (*monitor.Target, error)
	ListTargets(ctx context.Context) ([]monitor.Target, error)
	DeleteTarget(ctx context.Context, id int64) error

	SaveResult(ctx context.Context, result *monitor.Result) error
	ListResults(ctx context.Context, targetID int64, limit int) ([]monitor.Result, error)
	LatestResult(ctx context.Context, targetID int64) (*monitor.Result, error)
	// LatestResults возвращает последний результат для каждой цели одним
	// запросом: список целей не должен порождать N+1 запросов.
	LatestResults(ctx context.Context) (map[int64]monitor.Result, error)
	Stats(ctx context.Context, targetID int64) (*monitor.Stats, error)

	// PruneResults удаляет старые результаты, оставляя keep последних
	// на каждую цель. Без этого база растёт бесконечно.
	PruneResults(ctx context.Context, keep int) (int64, error)
}
