// Package jobs define as Tasks executadas pela fila River. A Task do portal
// (tabela tasks) e o job da River são criados na mesma transação, então
// nunca existe Task sem job nem job sem Task.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/vitordatasafer/mspvdc/internal/proxmox"
	"github.com/vitordatasafer/mspvdc/internal/store"
)

// PowerArgs carrega só o ID da Task: o resto é lido do banco na execução,
// então nada interno (vmid, node) trafega nos argumentos do job.
type PowerArgs struct {
	TaskID uuid.UUID `json:"task_id"`
}

func (PowerArgs) Kind() string { return "power_action" }

func (PowerArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 5}
}

type PowerWorker struct {
	river.WorkerDefaults[PowerArgs]
	Pool         *pgxpool.Pool
	Clients      *proxmox.Clients
	PollInterval time.Duration
	Log          *slog.Logger
}

func (w *PowerWorker) Timeout(*river.Job[PowerArgs]) time.Duration { return 10 * time.Minute }

var finalStatus = map[string]string{"start": "running", "reboot": "running", "shutdown": "stopped", "stop": "stopped"}

func (w *PowerWorker) Work(ctx context.Context, job *river.Job[PowerArgs]) error {
	st := store.New(w.Pool)
	task, upid, err := st.TaskForWorker(ctx, job.Args.TaskID)
	if err != nil {
		return river.JobCancel(fmt.Errorf("task %s: %w", job.Args.TaskID, err))
	}
	if task.Status == "succeeded" || task.Status == "failed" {
		return nil // já resolvida (retry depois de sucesso parcial)
	}

	fail := func(cause error) error {
		if err := st.FinishTask(ctx, task.ID, false, cause.Error()); err != nil {
			return err
		}
		return river.JobCancel(cause)
	}

	if task.ResourceID == nil {
		return fail(errors.New("task sem recurso"))
	}
	resourceID := *task.ResourceID
	loc, err := st.Location(ctx, resourceID)
	if err != nil {
		return fail(err)
	}
	cluster, err := st.GetCluster(ctx, loc.ClusterID)
	if err != nil {
		return fail(err)
	}
	client, err := w.Clients.For(ctx, cluster)
	if err != nil {
		return fail(err)
	}

	// Num retry com UPID já gravado, não disparamos a ação de novo: só
	// continuamos acompanhando a task existente no PVE.
	if upid == nil {
		u, err := client.Power(ctx, loc.Node, loc.Kind, loc.VMID, task.Action)
		if err != nil {
			var apiErr *proxmox.APIError
			if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
				return fail(err) // erro do pedido: não adianta repetir
			}
			if job.Attempt >= job.MaxAttempts {
				return fail(err)
			}
			return err // transitório: a River tenta de novo com backoff
		}
		if err := st.MarkTaskRunning(ctx, task.ID, u); err != nil {
			return err
		}
		upid = &u
	}

	poll := w.PollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		ts, err := client.TaskStatus(ctx, loc.Node, *upid)
		if err != nil {
			if job.Attempt >= job.MaxAttempts {
				return fail(err)
			}
			return err
		}
		if ts.Done() {
			if !ts.OK() {
				return fail(fmt.Errorf("proxmox: %s", ts.ExitStatus))
			}
			return pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error {
				s := store.New(tx)
				if err := s.FinishTask(ctx, task.ID, true, ""); err != nil {
					return err
				}
				return s.SetResourceStatus(ctx, resourceID, finalStatus[task.Action])
			})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// NewInsertClient cria um cliente River só de inserção (usado pela API).
func NewInsertClient(pool *pgxpool.Pool) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &PowerWorker{}) // registra os tipos para validar inserts
	river.AddWorker(workers, &VMWorker{})
	return river.NewClient(riverpgxv5.New(pool), &river.Config{Workers: workers})
}

// NewWorkerClient cria o cliente River que executa jobs.
func NewWorkerClient(pool *pgxpool.Pool, w *PowerWorker, vw *VMWorker, concurrency int) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	river.AddWorker(workers, vw)
	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		Workers: workers,
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: concurrency}},
		Logger:  w.Log,
	})
}
