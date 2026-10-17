package usecase

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"socketfabric/internal/application/ports"
	"socketfabric/internal/domain"
)

// ProjectPolicy separates DB refresh work from the realtime admission path.
// Snapshots are bounded, immutable after publication and age from query START.
type ProjectPolicy struct {
	store                         ports.ManagementStore
	env                           map[string]bool
	maxProjects                   int
	refresh, maxAge, queryTimeout time.Duration
	logger                        *slog.Logger
	mu                            sync.RWMutex
	refreshMu                     sync.Mutex
	states                        map[string]string
	readAt                        time.Time
	dbReady                       bool
	observed                      bool
}

func NewProjectPolicy(store ports.ManagementStore, env []string, maxProjects int, refresh, maxAge, queryTimeout time.Duration, logger *slog.Logger) (*ProjectPolicy, error) {
	if store == nil || logger == nil || maxProjects < 1 || refresh <= 0 || refresh >= maxAge || queryTimeout <= 0 || queryTimeout >= maxAge {
		return nil, domain.ErrInvalid
	}
	p := &ProjectPolicy{store: store, env: map[string]bool{}, maxProjects: maxProjects, refresh: refresh, maxAge: maxAge, queryTimeout: queryTimeout, logger: logger, states: map[string]string{}}
	for _, id := range env {
		p.env[id] = true
	}
	return p, nil
}
func (p *ProjectPolicy) CheckProject(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.env[id] {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.readAt.IsZero() || !time.Now().Before(p.readAt.Add(p.maxAge)) {
		return domain.ErrUnavailable
	}
	status, found := p.states[id]
	if !found {
		return domain.ErrUnauthorized
	}
	if status != domain.ProjectEnabled {
		return domain.ErrForbidden
	}
	return nil
}
func (p *ProjectPolicy) Ready() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.dbReady && !p.readAt.IsZero() && time.Now().Before(p.readAt.Add(p.maxAge))
}
func (p *ProjectPolicy) Snapshot() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var age any
	if !p.readAt.IsZero() {
		age = time.Since(p.readAt).Milliseconds()
	}
	return map[string]any{"databaseReady": p.dbReady, "policyAgeMs": age, "policyMaxAgeMs": p.maxAge.Milliseconds(), "managedProjectCount": len(p.states), "ready": p.dbReady && !p.readAt.IsZero() && time.Now().Before(p.readAt.Add(p.maxAge))}
}
func (p *ProjectPolicy) Refresh(ctx context.Context) error {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, p.queryTimeout)
	defer cancel()
	err := p.store.CheckSchema(ctx)
	var states []domain.ProjectState
	if err == nil {
		states, err = p.store.ProjectStates(ctx, p.maxProjects)
	}
	if len(states) > p.maxProjects {
		err = domain.ErrConflict
	}
	next := make(map[string]string, len(states))
	if err == nil {
		for _, s := range states {
			if domain.ValidateID(s.ID) != nil || p.env[s.ID] || s.Status != domain.ProjectEnabled && s.Status != domain.StatusDisabled {
				err = domain.ErrConflict
				break
			}
			next[s.ID] = s.Status
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	p.mu.Lock()
	wasReady := p.dbReady
	first := !p.observed
	p.observed = true
	p.dbReady = err == nil
	if err == nil {
		p.states = next
		p.readAt = started
	}
	p.mu.Unlock()
	if first || wasReady != (err == nil) {
		if err == nil {
			p.logger.InfoContext(ctx, "management persistence ready")
		} else {
			p.logger.WarnContext(ctx, "management persistence unavailable", "category", "database_schema_or_policy")
		}
	}
	return err
}

// Run owns exactly one refresh worker and one expiry timer. Cancellation joins
// the worker before the caller closes the DB pool. The callback never queries DB.
func (p *ProjectPolicy) Run(ctx context.Context, changed func()) {
	updates := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(p.refresh)
		defer ticker.Stop()
		for {
			_ = p.Refresh(ctx)
			select {
			case updates <- struct{}{}:
			default:
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	timer := time.NewTimer(p.maxAge)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			<-done
			return
		case <-updates:
			if changed != nil {
				changed()
			}
			p.mu.RLock()
			expires := p.readAt.Add(p.maxAge)
			p.mu.RUnlock()
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if wait := time.Until(expires); wait > 0 {
				timer.Reset(wait)
			}
		case <-timer.C:
			if changed != nil {
				changed()
			}
		}
	}
}

type ProjectBroadcaster struct {
	Access ports.ProjectAccess
	Next   ports.EventBroadcaster
}

func (b ProjectBroadcaster) Broadcast(ctx context.Context, event domain.Event) (int, error) {
	if err := b.Access.CheckProject(ctx, event.ProjectID); err != nil {
		return 0, err
	}
	return b.Next.Broadcast(ctx, event)
}
