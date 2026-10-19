package api

// Área de clusters (equipe da plataforma): cadastrar um Proxmox pelo portal
// (testando a conexão e fixando o certificado), ver capacidade, storages e
// bridges, configurar e reservar recursos do cluster para cada MSP.

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mspclouds/mspcp/internal/inventory"
	"github.com/mspclouds/mspcp/internal/proxmox"
	"github.com/mspclouds/mspcp/internal/quota"
	"github.com/mspclouds/mspcp/internal/rbac"
	"github.com/mspclouds/mspcp/internal/store"
)

// Privilégios que o portal usa; os que faltarem aparecem no teste.
var wantPrivs = []string{"VM.Audit", "VM.PowerMgmt", "VM.Console", "VM.Clone", "VM.Allocate", "VM.Config.CPU",
	"VM.Config.Memory", "VM.Config.Disk", "VM.Config.Network", "VM.Config.Cloudinit", "VM.Config.Options",
	"Datastore.Audit", "Datastore.AllocateSpace", "Pool.Allocate", "Pool.Audit", "Sys.Audit"}

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request) {
	if !s.canClusters(r, rbac.ClusterRead) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	st := s.store()
	cs, err := st.ListClusters(r.Context(), false)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	type row struct {
		store.Cluster
		ReservedCPU      int   `json:"reserved_cpu"`
		ReservedMemoryMB int64 `json:"reserved_memory_mb"`
		MSPs             int   `json:"msps"`
		HasStoredSecret  bool  `json:"stored_secret"`
	}
	out := make([]row, 0, len(cs))
	for _, c := range cs {
		cpu, mem, _, err := st.ClusterReserved(r.Context(), c.ID, nil)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		rs, err := st.ListReservations(r.Context(), nil, &c.ID)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		out = append(out, row{c, cpu, mem, len(rs), len(c.TokenSecretEnc) > 0})
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": out})
}

// clusterIn é o formulário de cadastro/edição.
type clusterIn struct {
	Name           *string  `json:"name"`
	DC             *string  `json:"dc"`
	Location       *string  `json:"location"`
	APIURL         *string  `json:"api_url"`
	TokenID        *string  `json:"token_id"`
	TokenSecret    *string  `json:"token_secret"`
	TokenSecretRef *string  `json:"token_secret_ref"` // legado: file:/env:
	CACertPEM      *string  `json:"ca_cert_pem"`
	DefaultBridge  *string  `json:"default_bridge"`
	CPURatio       *float64 `json:"cpu_ratio"`
	Enabled        *bool    `json:"enabled"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// probeResult é o que a tela de cadastro mostra antes de salvar.
type probeResult struct {
	OK                bool                   `json:"ok"`
	Error             string                 `json:"error,omitempty"`
	TLS               *proxmox.TLSProbe      `json:"tls,omitempty"`
	CACertPEM         string                 `json:"ca_cert_pem"`
	Version           string                 `json:"version,omitempty"`
	NodesOnline       int                    `json:"nodes_online"`
	NodesTotal        int                    `json:"nodes_total"`
	CPUTotal          int                    `json:"cpu_total"`
	MemoryTotalMB     int64                  `json:"memory_total_mb"`
	Storages          []store.ClusterStorage `json:"storages"`
	Bridges           []store.ClusterBridge  `json:"bridges"`
	VMs               int                    `json:"vms"`
	Templates         int                    `json:"templates"`
	MissingPrivileges []string               `json:"missing_privileges"`
}

// probe conecta ao cluster com o que foi informado. Sem CA informada: usa as
// CAs do sistema se o certificado for público, senão fixa o certificado
// apresentado (o admin confere a impressão digital antes de salvar).
func (s *Server) probe(ctx context.Context, apiURL, caPEM, tokenID, secret string) probeResult {
	res := probeResult{Storages: []store.ClusterStorage{}, Bridges: []store.ClusterBridge{}, MissingPrivileges: []string{}}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	tlsInfo, err := proxmox.ProbeTLS(ctx, apiURL, s.Allow)
	if err != nil {
		res.Error = "não foi possível conectar: " + err.Error()
		return res
	}
	res.TLS = &tlsInfo
	switch {
	case caPEM != "":
		res.CACertPEM = caPEM
	case tlsInfo.TrustedBySystem:
		res.CACertPEM = ""
	default:
		res.CACertPEM = tlsInfo.SuggestedPEM
	}
	cl, err := proxmox.NewClient(apiURL, res.CACertPEM, tokenID, secret, s.Allow)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	snap, err := cl.Read(ctx, true)
	if err != nil {
		var api *proxmox.APIError
		if errors.As(err, &api) && api.Status == http.StatusUnauthorized {
			res.Error = "o Proxmox recusou o token (confira o ID e o segredo)"
		} else {
			res.Error = err.Error()
		}
		return res
	}
	inv := snap.Inventory
	res.OK, res.Version = true, inv.Version
	res.NodesOnline, res.NodesTotal, res.CPUTotal, res.MemoryTotalMB = inv.NodesOnline, inv.NodesTotal, inv.CPUTotal, inv.MemoryTotalMB
	if inv.Storages != nil {
		res.Storages = inv.Storages
	}
	if inv.Bridges != nil {
		res.Bridges = inv.Bridges
	}
	res.VMs, res.Templates = len(snap.VMs), len(snap.Templates)
	if perms, err := cl.Permissions(ctx); err == nil {
		have := perms["/"]
		for _, p := range wantPrivs {
			if have[p] == 0 {
				res.MissingPrivileges = append(res.MissingPrivileges, p)
			}
		}
	}
	return res
}

func (s *Server) probeCluster(w http.ResponseWriter, r *http.Request) {
	if !s.canClusters(r, rbac.ClusterManage) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	var in struct {
		clusterIn
		ClusterID *uuid.UUID `json:"cluster_id"` // edição: usa o segredo guardado se não vier outro
	}
	if !decode(w, r, &in) {
		return
	}
	apiURL, tokenID, secret, ca := str(in.APIURL), str(in.TokenID), str(in.TokenSecret), str(in.CACertPEM)
	if in.ClusterID != nil {
		c, err := s.store().GetCluster(r.Context(), *in.ClusterID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "cluster não encontrado")
			return
		}
		if apiURL == "" {
			apiURL = c.APIURL
		}
		if tokenID == "" {
			tokenID = c.TokenID
		}
		if secret == "" {
			if secret, err = s.clusterSecret(r.Context(), c); err != nil {
				writeErr(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
		}
	}
	if apiURL == "" || tokenID == "" || secret == "" {
		writeErr(w, http.StatusBadRequest, "informe o endereço, o token e o segredo")
		return
	}
	writeJSON(w, http.StatusOK, s.probe(r.Context(), apiURL, ca, tokenID, secret))
}

func (s *Server) clusterSecret(ctx context.Context, c store.Cluster) (string, error) {
	if len(c.TokenSecretEnc) > 0 {
		return s.Clients.Box().Open(c.TokenSecretEnc)
	}
	return "", errors.New("este cluster usa segredo em arquivo; informe o segredo para testar")
}

func (s *Server) createCluster(w http.ResponseWriter, r *http.Request) {
	if !s.canClusters(r, rbac.ClusterManage) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	var in clusterIn
	if !decode(w, r, &in) {
		return
	}
	c := store.Cluster{Name: str(in.Name), DC: str(in.DC), Location: str(in.Location), APIURL: str(in.APIURL),
		TokenID: str(in.TokenID), CACertPEM: str(in.CACertPEM), DefaultBridge: str(in.DefaultBridge), TokenSecretRef: str(in.TokenSecretRef)}
	if in.CPURatio != nil {
		c.CPURatio = *in.CPURatio
	}
	if c.Name == "" || c.DC == "" || c.APIURL == "" || c.TokenID == "" {
		writeErr(w, http.StatusBadRequest, "informe nome, datacenter, endereço e token")
		return
	}
	if c.Location == "" {
		c.Location = c.DC // o rótulo de localização é o que MSPs e clientes veem
	}
	if c.DefaultBridge != "" && !bridgeRe.MatchString(c.DefaultBridge) {
		writeErr(w, http.StatusBadRequest, "bridge padrão inválida")
		return
	}
	if c.CPURatio != 0 && (c.CPURatio < 1 || c.CPURatio > 20) {
		writeErr(w, http.StatusBadRequest, "razão de vCPU entre 1 e 20")
		return
	}
	secret := str(in.TokenSecret)
	switch {
	case secret != "":
		if s.Clients == nil || s.Clients.Box() == nil {
			writeErr(w, http.StatusServiceUnavailable, "o servidor não tem chave mestra (MASTER_KEY_FILE) para guardar o segredo")
			return
		}
		// Testa antes de gravar: nada de cluster cadastrado que não conecta.
		p := s.probe(r.Context(), c.APIURL, c.CACertPEM, c.TokenID, secret)
		if !p.OK {
			writeErr(w, http.StatusUnprocessableEntity, p.Error)
			return
		}
		c.CACertPEM = p.CACertPEM
		enc, err := s.Clients.Box().Seal(secret)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		c.TokenSecretEnc = enc
	case c.TokenSecretRef != "":
		// Legado: segredo fora do banco (file:/env:). Só valida o formato.
		if _, err := proxmox.NewClient(c.APIURL, c.CACertPEM, c.TokenID, "validacao", s.Allow); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "informe o segredo do token")
		return
	}
	var out store.Cluster
	err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		var err error
		if out, err = ts.CreateCluster(r.Context(), c); err != nil {
			return err
		}
		return s.audit(r, ts, "cluster.create", out.ID.String(), nil, map[string]any{"name": c.Name, "dc": c.DC, "api_url": c.APIURL})
	})
	if errors.Is(err, store.ErrConflict) {
		writeErr(w, http.StatusConflict, "já existe um cluster com esse nome")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	s.syncNow(r.Context(), out)
	if c2, err := s.store().GetCluster(r.Context(), out.ID); err == nil {
		out = c2
	}
	writeJSON(w, http.StatusCreated, out)
}

// syncNow lê o cluster na hora (cadastro e botão "Sincronizar"), para a tela
// já mostrar capacidade, storages e templates.
func (s *Server) syncNow(ctx context.Context, c store.Cluster) error {
	if s.Clients == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	sy := &inventory.Syncer{Pool: s.Pool, Clients: s.Clients, Log: s.Log}
	_, err := sy.SyncCluster(ctx, c)
	return err
}

func (s *Server) syncCluster(w http.ResponseWriter, r *http.Request) {
	c, ok := s.clusterForStaff(w, r, rbac.ClusterRead)
	if !ok {
		return
	}
	if err := s.syncNow(r.Context(), c); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) clusterForStaff(w http.ResponseWriter, r *http.Request, perm rbac.Permission) (store.Cluster, bool) {
	if !s.canClusters(r, perm) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return store.Cluster{}, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return store.Cluster{}, false
	}
	c, err := s.store().GetCluster(r.Context(), id)
	if isNotFound(err) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return c, false
	} else if err != nil {
		s.internal(w, r, err)
		return c, false
	}
	return c, true
}

type capacityLine struct {
	ClassID    *uuid.UUID `json:"class_id,omitempty"`
	Name       string     `json:"name"`
	Total      int64      `json:"total"`    // vCPU (com a razão), MB ou GB
	Physical   int64      `json:"physical"` // sem a razão
	Reserved   int64      `json:"reserved"`
	PVEStorage string     `json:"pve_storage,omitempty"`
}

func (s *Server) getCluster(w http.ResponseWriter, r *http.Request) {
	c, ok := s.clusterForStaff(w, r, rbac.ClusterRead)
	if !ok {
		return
	}
	st := s.store()
	ctx := r.Context()
	storages, err := st.ListClusterStorages(ctx, c.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	bridges, err := st.ListClusterBridges(ctx, c.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	reservations, err := st.ListReservations(ctx, nil, &c.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	cpu, mem, rst, err := st.ClusterReserved(ctx, c.ID, nil)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	classes, err := st.ListStorageClasses(ctx)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	policies, err := st.ListStoragePolicies(ctx)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	capGB, err := st.ClassCapacityGB(ctx, c.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	templates, err := st.ListTemplates(ctx, c.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	msps, err := st.ListTenants(ctx, principal(r).Scope(rbac.TenantRead))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	lines := []capacityLine{
		{Name: "vCPU", Total: int64(float64(c.CPUTotal) * c.CPURatio), Physical: int64(c.CPUTotal), Reserved: int64(cpu)},
		{Name: "Memória", Total: c.MemoryTotalMB, Physical: c.MemoryTotalMB, Reserved: mem},
	}
	mapped := map[uuid.UUID]string{}
	for _, p := range policies {
		if p.ClusterID == c.ID {
			mapped[p.ClassID] = p.PVEStorage
		}
	}
	for _, cl := range classes {
		pve, ok := mapped[cl.ID]
		if !ok {
			continue
		}
		id := cl.ID
		lines = append(lines, capacityLine{ClassID: &id, Name: cl.Name, Total: capGB[cl.ID], Physical: capGB[cl.ID], Reserved: rst[cl.ID], PVEStorage: pve})
	}
	partners := []store.Tenant{}
	for _, t := range msps {
		if t.Kind == store.KindPartner {
			partners = append(partners, t)
		}
	}
	sort.Slice(partners, func(i, j int) bool { return partners[i].Name < partners[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{
		"cluster": c, "stored_secret": len(c.TokenSecretEnc) > 0, "pinned_ca": c.CACertPEM != "",
		"storages": nonNil(storages), "bridges": nonNil(bridges), "capacity": lines, "storage_classes": nonNil(classes),
		"reservations": reservations, "templates": len(templates), "msps": partners,
	})
}

func (s *Server) updateCluster(w http.ResponseWriter, r *http.Request) {
	c, ok := s.clusterForStaff(w, r, rbac.ClusterManage)
	if !ok {
		return
	}
	var in clusterIn
	if !decode(w, r, &in) {
		return
	}
	u := store.ClusterUpdate{Name: trimPtr(in.Name), DC: trimPtr(in.DC), Location: trimPtr(in.Location),
		DefaultBridge: trimPtr(in.DefaultBridge), CPURatio: in.CPURatio, Enabled: in.Enabled}
	for _, p := range []*string{u.Name, u.DC, u.Location} {
		if p != nil && *p == "" {
			writeErr(w, http.StatusBadRequest, "nome, datacenter e localização não podem ficar vazios")
			return
		}
	}
	if u.DefaultBridge != nil && !bridgeRe.MatchString(*u.DefaultBridge) {
		writeErr(w, http.StatusBadRequest, "bridge padrão inválida")
		return
	}
	if u.CPURatio != nil && (*u.CPURatio < 1 || *u.CPURatio > 20) {
		writeErr(w, http.StatusBadRequest, "razão de vCPU entre 1 e 20")
		return
	}
	// Conexão: qualquer mudança é testada antes de gravar.
	if in.APIURL != nil || in.TokenID != nil || in.TokenSecret != nil || in.CACertPEM != nil {
		apiURL, tokenID, ca := c.APIURL, c.TokenID, c.CACertPEM
		if v := str(in.APIURL); v != "" {
			apiURL = v
		}
		if v := str(in.TokenID); v != "" {
			tokenID = v
		}
		if in.CACertPEM != nil {
			ca = str(in.CACertPEM)
		}
		secret := str(in.TokenSecret)
		var err error
		if secret == "" {
			if secret, err = s.clusterSecret(r.Context(), c); err != nil {
				writeErr(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
		}
		// Endereço novo sem CA informada: fixa de novo o que ele apresentar.
		if in.APIURL != nil && in.CACertPEM == nil {
			ca = ""
		}
		p := s.probe(r.Context(), apiURL, ca, tokenID, secret)
		if !p.OK {
			writeErr(w, http.StatusUnprocessableEntity, p.Error)
			return
		}
		u.APIURL, u.TokenID, u.CACertPEM = &apiURL, &tokenID, &p.CACertPEM
		if in.TokenSecret != nil && secret != "" {
			enc, err := s.Clients.Box().Seal(secret)
			if err != nil {
				writeErr(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			u.TokenSecretEnc = enc
		}
	}
	var out store.Cluster
	err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		if u.CPURatio != nil && c.CPUTotal > 0 {
			if err := ts.LockCluster(r.Context(), c.ID); err != nil {
				return err
			}
			cpu, _, _, err := ts.ClusterReserved(r.Context(), c.ID, nil)
			if err != nil {
				return err
			}
			if capCPU := int(float64(c.CPUTotal) * *u.CPURatio); cpu > capCPU {
				return &quota.Error{Msg: "os MSPs já reservam " + itoa(cpu) + " vCPU; com essa razão o cluster teria só " + itoa(capCPU)}
			}
		}
		var err error
		if out, err = ts.UpdateCluster(r.Context(), c.ID, u); err != nil {
			return err
		}
		return s.audit(r, ts, "cluster.update", c.ID.String(), nil, map[string]any{
			"name": in.Name, "location": in.Location, "cpu_ratio": in.CPURatio, "enabled": in.Enabled,
			"default_bridge": in.DefaultBridge, "connection_changed": u.APIURL != nil, "token_rotated": u.TokenSecretEnc != nil,
		})
	})
	if errors.Is(err, store.ErrConflict) {
		writeErr(w, http.StatusConflict, "já existe um cluster com esse nome")
		return
	} else if s.quotaErr(w, err) {
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	if u.APIURL != nil {
		_ = s.syncNow(r.Context(), out)
	}
	writeJSON(w, http.StatusOK, out)
}

func trimPtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := strings.TrimSpace(*p)
	return &v
}

// ---------- reservas do MSP no cluster ----------

func (s *Server) reservationTarget(w http.ResponseWriter, r *http.Request) (store.Cluster, store.Tenant, bool) {
	c, ok := s.clusterForStaff(w, r, rbac.ClusterManage)
	if !ok {
		return c, store.Tenant{}, false
	}
	tid, err := uuid.Parse(chi.URLParam(r, "tenant"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "MSP não encontrado")
		return c, store.Tenant{}, false
	}
	t, err := s.store().GetTenant(r.Context(), tid)
	if err != nil || t.Kind != store.KindPartner || !principal(r).Can(rbac.LimitsManage, t.Path, nil) {
		writeErr(w, http.StatusNotFound, "MSP não encontrado")
		return c, t, false
	}
	return c, t, true
}

func (s *Server) putReservation(w http.ResponseWriter, r *http.Request) {
	c, t, ok := s.reservationTarget(w, r)
	if !ok {
		return
	}
	var in struct {
		CPUCores int         `json:"cpu_cores"`
		MemoryMB int64       `json:"memory_mb"`
		Storage  []storageIn `json:"storage"`
	}
	if !decode(w, r, &in) {
		return
	}
	next := store.Reservation{CPUCores: in.CPUCores, MemoryMB: in.MemoryMB, Storage: map[uuid.UUID]int64{}}
	for _, x := range in.Storage {
		if _, dup := next.Storage[x.ClassID]; dup {
			writeErr(w, http.StatusBadRequest, "classe de storage repetida")
			return
		}
		next.Storage[x.ClassID] = x.CapacityGB
	}
	err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		cur, err := ts.GetCluster(r.Context(), c.ID) // relido com a transação
		if err != nil {
			return err
		}
		if err := quota.SetReservation(r.Context(), ts, t, cur, next); err != nil {
			return err
		}
		return s.audit(r, ts, "msp.reservation", c.ID.String(), &t.ID, map[string]any{
			"cluster": c.Name, "cpu_cores": in.CPUCores, "memory_mb": in.MemoryMB, "storage": in.Storage,
		})
	})
	if s.quotaErr(w, err) {
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteReservation(w http.ResponseWriter, r *http.Request) {
	c, t, ok := s.reservationTarget(w, r)
	if !ok {
		return
	}
	err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		if err := quota.DeleteReservation(r.Context(), ts, t, c.ID); err != nil {
			return err
		}
		return s.audit(r, ts, "msp.reservation_delete", c.ID.String(), &t.ID, map[string]any{"cluster": c.Name})
	})
	if s.quotaErr(w, err) {
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
