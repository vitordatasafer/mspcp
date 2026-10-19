// Package inventory mantém a tabela resources em dia com os clusters. O
// portal responde do banco, não do Proxmox: uma chamada a
// /cluster/resources por cluster e por ciclo, qualquer que seja o número de
// usuários olhando o portal.
package inventory

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mspclouds/mspcp/internal/proxmox"
	"github.com/mspclouds/mspcp/internal/store"
)

type Syncer struct {
	Pool     *pgxpool.Pool
	Clients  *proxmox.Clients
	Interval time.Duration
	Log      *slog.Logger

	mu          sync.Mutex
	bridgesRead map[string]time.Time // cluster -> última leitura de bridges
}

// Bridges mudam pouco: relidas a cada 5 minutos.
const bridgesEvery = 5 * time.Minute

func (s *Syncer) wantBridges(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bridgesRead == nil {
		s.bridgesRead = map[string]time.Time{}
	}
	if time.Since(s.bridgesRead[id]) < bridgesEvery {
		return false
	}
	s.bridgesRead[id] = time.Now()
	return true
}

// Run sincroniza todos os clusters a cada Interval até o contexto acabar.
func (s *Syncer) Run(ctx context.Context) {
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		s.SyncAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SyncAll roda os clusters em paralelo; a falha de um não afeta os outros.
func (s *Syncer) SyncAll(ctx context.Context) {
	clusters, err := store.New(s.Pool).ListClusters(ctx, true)
	if err != nil {
		s.Log.Error("sync: listar clusters", "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, c := range clusters {
		wg.Add(1)
		go func(c store.Cluster) {
			defer wg.Done()
			res, err := s.SyncCluster(ctx, c)
			if err != nil {
				s.Log.Warn("sync: cluster falhou", "cluster", c.Name, "dc", c.DC, "err", err)
				return
			}
			s.Log.Debug("sync: ok", "cluster", c.Name, "seen", res.Seen, "unassigned", res.Unassigned, "orphaned", res.Orphaned)
		}(c)
	}
	wg.Wait()
}

func (s *Syncer) SyncCluster(ctx context.Context, c store.Cluster) (store.SyncResult, error) {
	st := store.New(s.Pool)
	res, err := s.syncCluster(ctx, c)
	// Em erro, nada é marcado como órfão: só registramos a falha.
	if mErr := st.MarkClusterSynced(ctx, c.ID, err); mErr != nil && err == nil {
		err = mErr
	}
	return res, err
}

func (s *Syncer) syncCluster(ctx context.Context, c store.Cluster) (store.SyncResult, error) {
	cl, err := s.Clients.For(ctx, c)
	if err != nil {
		return store.SyncResult{}, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	snap, err := cl.Read(reqCtx, s.wantBridges(c.ID.String()))
	if err != nil {
		return store.SyncResult{}, err
	}
	vms := snap.VMs
	templates := make([]store.PVETemplate, 0, len(snap.Templates))
	for _, t := range snap.Templates {
		templates = append(templates, store.PVETemplate{VMID: t.VMID, Node: t.Node, Name: t.Name, MaxDiskBytes: t.MaxDisk})
	}
	snaps := make([]store.Snapshot, 0, len(vms))
	for _, v := range vms {
		snaps = append(snaps, store.Snapshot{
			VMID: v.VMID, Kind: v.Type, Node: v.Node, Name: v.Name, Pool: v.Pool,
			Status: v.Status, CPUs: int(v.MaxCPU), MaxMemBytes: v.MaxMem, MaxDiskBytes: v.MaxDisk,
		})
	}
	var res store.SyncResult
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		ts := store.New(tx)
		if err := ts.SaveTemplates(ctx, c.ID, templates); err != nil {
			return err
		}
		if err := ts.SaveClusterInventory(ctx, c.ID, snap.Inventory); err != nil {
			return err
		}
		res, err = ts.ApplySnapshots(ctx, c.ID, snaps)
		return err
	})
	return res, err
}
