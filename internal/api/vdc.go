package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mspclouds/mspcp/internal/quota"
	"github.com/mspclouds/mspcp/internal/rbac"
	"github.com/mspclouds/mspcp/internal/store"
)

// quotaErr responde 422 para violações de regra de cota.
func (s *Server) quotaErr(w http.ResponseWriter, err error) bool {
	var q *quota.Error
	if errors.As(err, &q) {
		writeErr(w, http.StatusUnprocessableEntity, q.Msg)
		return true
	}
	return false
}

type storageIn struct {
	ClassID    uuid.UUID `json:"class_id"`
	CapacityGB int64     `json:"capacity_gb"`
}

type vdcIn struct {
	Name       string      `json:"name"`
	LocationID uuid.UUID   `json:"location_id"`
	CPUCores   int         `json:"cpu_cores"`
	MemoryMB   int64       `json:"memory_mb"`
	Storage    []storageIn `json:"storage"`
}

func (in vdcIn) spec(tenant uuid.UUID) (store.VDCSpec, error) {
	sp := store.VDCSpec{TenantID: tenant, Name: strings.TrimSpace(in.Name), ClusterID: in.LocationID,
		CPUCores: in.CPUCores, MemoryMB: in.MemoryMB, Storage: map[uuid.UUID]int64{}}
	for _, st := range in.Storage {
		if _, dup := sp.Storage[st.ClassID]; dup {
			return sp, errors.New("classe de storage repetida")
		}
		sp.Storage[st.ClassID] = st.CapacityGB
	}
	return sp, nil
}

// ---------- VDCs ----------

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tenantInScope(w, r)
	if !ok {
		return
	}
	var in vdcIn
	if !decode(w, r, &in) {
		return
	}
	if t.Kind == store.KindProvider || !principal(r).Can(rbac.VDCManage, t.Path, nil) {
		writeErr(w, http.StatusForbidden, "só o MSP ou a administração da plataforma criam VDCs")
		return
	}
	spec, err := in.spec(t.ID)
	if err != nil || spec.Name == "" || in.LocationID == uuid.Nil {
		writeErr(w, http.StatusBadRequest, "informe nome, localização e recursos do VDC")
		return
	}
	var p store.Project
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		if err := quota.CheckVDC(r.Context(), ts, t, spec, nil); err != nil {
			return err
		}
		var err error
		if p, err = ts.CreateProject(r.Context(), spec); err != nil {
			return err
		}
		return s.audit(r, ts, "vdc.create", p.ID.String(), &t.ID,
			map[string]any{"cpu_cores": spec.CPUCores, "memory_mb": spec.MemoryMB, "storage": in.Storage, "location_id": in.LocationID})
	})
	if errors.Is(err, store.ErrConflict) {
		writeErr(w, http.StatusConflict, "já existe um VDC com esse nome neste cliente")
		return
	} else if s.quotaErr(w, err) {
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	// O pool do Proxmox é exposto só a quem administra clusters.
	out := map[string]any{"project": p}
	if principal(r).Can(rbac.ClusterManage, t.Path, nil) {
		out["proxmox_pool"] = p.PoolName
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) updateProjectQuota(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in vdcIn
	if !decode(w, r, &in) {
		return
	}
	st := s.store()
	p, err := st.GetProject(r.Context(), id)
	if isNotFound(err) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	t, err := st.GetTenant(r.Context(), p.TenantID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !principal(r).Can(rbac.TenantRead, t.Path, nil) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	if !principal(r).Can(rbac.VDCManage, t.Path, nil) {
		writeErr(w, http.StatusForbidden, "só o MSP ou a administração da plataforma ajustam VDCs")
		return
	}
	spec, err := in.spec(t.ID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	spec.Name, spec.ClusterID = p.Name, *p.ClusterID
	var out store.Project
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		cur, err := ts.GetProject(r.Context(), id) // relido dentro da transação
		if err != nil {
			return err
		}
		if err := quota.CheckVDC(r.Context(), ts, t, spec, &cur); err != nil {
			return err
		}
		if out, err = ts.UpdateProjectQuota(r.Context(), id, spec); err != nil {
			return err
		}
		return s.audit(r, ts, "vdc.resize", id.String(), &t.ID,
			map[string]any{"cpu_cores": spec.CPUCores, "memory_mb": spec.MemoryMB, "storage": in.Storage})
	})
	if s.quotaErr(w, err) {
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------- capacidade e limites do MSP ----------

// getCapacity mostra a cota do MSP, quanto já foi distribuído e os VDCs da
// subárvore. Clientes finais não chegam aqui (o MSP está fora do escopo deles).
func (s *Server) getCapacity(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tenantInScope(w, r)
	if !ok {
		return
	}
	if t.Kind != store.KindPartner {
		writeErr(w, http.StatusBadRequest, "capacidade existe só para MSPs")
		return
	}
	st := s.store()
	limits, err := st.GetMSPLimits(r.Context(), t.ID, false)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	usage, err := st.MSPUsage(r.Context(), t.Path, nil)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	vdcs, err := st.ListProjectsUnder(r.Context(), t.Path)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	locs, err := s.locationsFor(r, st, limits)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"limits": limits, "usage": usage, "vdcs": nonNil(vdcs), "locations": locs,
	})
}

func (s *Server) putLimits(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tenantInScope(w, r)
	if !ok {
		return
	}
	if !principal(r).Can(rbac.LimitsManage, t.Path, nil) {
		writeErr(w, http.StatusForbidden, "só a administração da plataforma define os limites dos MSPs")
		return
	}
	// vCPU, memória e storage agora são reservados por cluster
	// (PUT /v1/clusters/{id}/reservations/{tenant}); aqui ficam as quantidades.
	var in struct {
		MaxCustomers *int `json:"max_customers"`
		MaxVDCs      *int `json:"max_vdcs"`
		MaxVMs       *int `json:"max_vms"`
	}
	if !decode(w, r, &in) {
		return
	}
	for _, v := range []*int{in.MaxCustomers, in.MaxVDCs, in.MaxVMs} {
		if v != nil && *v < 0 {
			writeErr(w, http.StatusBadRequest, "valores não podem ser negativos")
			return
		}
	}
	next := store.MSPLimits{MaxCustomers: in.MaxCustomers, MaxVDCs: in.MaxVDCs, MaxVMs: in.MaxVMs}
	err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		if err := quota.SetLimits(r.Context(), ts, t, next); err != nil {
			return err
		}
		return s.audit(r, ts, "msp.limits", t.ID.String(), &t.ID, map[string]any{"limits": in})
	})
	if s.quotaErr(w, err) {
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- localizações (o que MSP e cliente veem dos clusters) ----------

type location struct {
	ID      uuid.UUID            `json:"id"`
	Name    string               `json:"name"`
	Classes []store.StorageClass `json:"storage_classes"`
}

// locationsFor devolve os clusters liberados para o MSP só como localização:
// rótulo e classes de storage disponíveis, nada de nome, URL ou nodes.
func (s *Server) locationsFor(r *http.Request, st *store.Store, limits store.MSPLimits) ([]location, error) {
	classes, err := st.ListStorageClasses(r.Context())
	if err != nil {
		return nil, err
	}
	name := map[uuid.UUID]string{}
	for _, c := range classes {
		name[c.ID] = c.Name
	}
	out := []location{}
	for _, res := range limits.Reservations {
		loc := location{ID: res.ClusterID, Name: res.Location, Classes: []store.StorageClass{}}
		for c := range res.Storage {
			loc.Classes = append(loc.Classes, store.StorageClass{ID: c, Name: name[c]})
		}
		out = append(out, loc)
	}
	return out, nil
}

// ---------- classes e políticas de storage (equipe) ----------

func (s *Server) listStorageClasses(w http.ResponseWriter, r *http.Request) {
	cs, err := s.store().ListStorageClasses(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"storage_classes": nonNil(cs)})
}

func (s *Server) createStorageClass(w http.ResponseWriter, r *http.Request) {
	if !s.canClusters(r, rbac.ClusterManage) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name obrigatório")
		return
	}
	c, err := s.store().CreateStorageClass(r.Context(), in.Name, in.Description)
	if errors.Is(err, store.ErrConflict) {
		writeErr(w, http.StatusConflict, "classe já existe")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) listStoragePolicies(w http.ResponseWriter, r *http.Request) {
	if !s.canClusters(r, rbac.ClusterRead) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	ps, err := s.store().ListStoragePolicies(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"storage_policies": nonNil(ps)})
}

func (s *Server) setStoragePolicy(w http.ResponseWriter, r *http.Request) {
	if !s.canClusters(r, rbac.ClusterManage) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	clusterID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	var in struct {
		ClassID    uuid.UUID `json:"class_id"`
		PVEStorage string    `json:"pve_storage"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.PVEStorage = strings.TrimSpace(in.PVEStorage); in.PVEStorage == "" {
		writeErr(w, http.StatusBadRequest, "pve_storage obrigatório")
		return
	}
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		if _, err := ts.GetCluster(r.Context(), clusterID); err != nil {
			return err
		}
		classes, err := ts.ListStorageClasses(r.Context())
		if err != nil {
			return err
		}
		found := false
		for _, c := range classes {
			found = found || c.ID == in.ClassID
		}
		if !found {
			return store.ErrNotFound
		}
		if err := ts.SetStoragePolicy(r.Context(), clusterID, in.ClassID, in.PVEStorage); err != nil {
			return err
		}
		return s.audit(r, ts, "storage_policy.set", clusterID.String(), nil, map[string]any{"class_id": in.ClassID, "pve_storage": in.PVEStorage})
	})
	if isNotFound(err) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listAllProjects devolve os VDCs que o usuário enxerga, em qualquer conta.
func (s *Server) listAllProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := s.store().ListProjectsScope(r.Context(), principal(r).Scope(rbac.ResourceRead))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !s.canClusters(r, rbac.ClusterRead) {
		writeJSON(w, http.StatusOK, map[string]any{"projects": nonNil(ps)})
		return
	}
	// Equipe: inclui a rede de cada VDC.
	type net struct {
		Bridge  *string `json:"bridge"`
		VLANTag *int    `json:"vlan_tag"`
	}
	type withNet struct {
		store.Project
		Network net `json:"network"`
	}
	out := make([]withNet, 0, len(ps))
	for _, p := range ps {
		out = append(out, withNet{p, net{p.Bridge, p.VLANTag}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

// listTasks devolve as tarefas recentes dos servidores visíveis.
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	ts, err := s.store().ListTasksScope(r.Context(), principal(r).Scope(rbac.ResourceRead), limit)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": nonNil(ts)})
}
