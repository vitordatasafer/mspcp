package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mspclouds/mspcp/internal/jobs"
	"github.com/mspclouds/mspcp/internal/quota"
	"github.com/mspclouds/mspcp/internal/rbac"
	"github.com/mspclouds/mspcp/internal/store"
)

// ---------- me ----------

func (s *Server) getMe(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	type m struct {
		TenantID  uuid.UUID  `json:"tenant_id"`
		Kind      string     `json:"tenant_kind"`
		ProjectID *uuid.UUID `json:"project_id,omitempty"`
		Role      string     `json:"role"`
	}
	out := struct {
		Subject     string `json:"subject"`
		Name        string `json:"name,omitempty"`
		Email       string `json:"email,omitempty"`
		Local       bool   `json:"local"`
		MustChange  bool   `json:"must_change_password"`
		TOTPEnabled bool   `json:"totp_enabled"`
		Memberships []m    `json:"memberships"`
	}{Subject: p.Subject, Email: p.Email, Name: p.Name, Memberships: []m{}}
	if p.User != nil {
		out.Local, out.MustChange, out.TOTPEnabled = true, p.User.MustChange, p.User.TOTPEnabled
	}
	for _, x := range p.Memberships {
		out.Memberships = append(out.Memberships, m{x.TenantID, string(x.TenantKind), x.ProjectID, x.Role})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------- tenants ----------

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	ts, err := s.store().ListTenants(r.Context(), principal(r).Scope(rbac.TenantRead))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": nonNil(ts)})
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ParentID uuid.UUID        `json:"parent_id"`
		Kind     store.TenantKind `json:"kind"`
		Name     string           `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || (in.Kind != store.KindPartner && in.Kind != store.KindCustomer) {
		writeErr(w, http.StatusBadRequest, "name obrigatório; kind deve ser partner ou customer")
		return
	}
	st := s.store()
	parent, err := st.GetTenant(r.Context(), in.ParentID)
	if isNotFound(err) || (err == nil && !principal(r).Can(rbac.TenantRead, parent.Path, nil)) {
		writeErr(w, http.StatusNotFound, "tenant pai não encontrado")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	if !principal(r).Can(rbac.TenantCreate, parent.Path, nil) {
		writeErr(w, http.StatusForbidden, "sem permissão para criar tenants aqui")
		return
	}
	var t store.Tenant
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := st.WithTx(tx)
		if in.Kind == store.KindCustomer && parent.Kind == store.KindPartner {
			if err := quota.CheckNewCustomer(r.Context(), ts, parent); err != nil {
				return err
			}
		}
		var err error
		if t, err = ts.CreateTenant(r.Context(), &parent.ID, in.Kind, in.Name); err != nil {
			return err
		}
		return s.audit(r, ts, "tenant.create", t.ID.String(), &t.ID, map[string]any{"kind": in.Kind, "parent": parent.ID})
	})
	if err != nil {
		if strings.Contains(err.Error(), "não pode ter filho") {
			writeErr(w, http.StatusBadRequest, "clientes ficam sempre sob um MSP; parceiros só sob o provedor")
			return
		}
		if s.quotaErr(w, err) {
			return
		}
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) tenantInScope(w http.ResponseWriter, r *http.Request) (store.Tenant, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return store.Tenant{}, false
	}
	t, err := s.store().GetTenant(r.Context(), id)
	if isNotFound(err) || (err == nil && !principal(r).Can(rbac.TenantRead, t.Path, nil)) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return store.Tenant{}, false
	} else if err != nil {
		s.internal(w, r, err)
		return store.Tenant{}, false
	}
	return t, true
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tenantInScope(w, r)
	if !ok {
		return
	}
	ps, err := s.store().ListProjects(r.Context(), t.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": nonNil(ps)})
}

func (s *Server) listMemberships(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tenantInScope(w, r)
	if !ok {
		return
	}
	if !principal(r).Can(rbac.MembershipManage, t.Path, nil) && !principal(r).Can(rbac.ClusterRead, t.Path, nil) {
		writeErr(w, http.StatusForbidden, "sem permissão para ver os usuários deste tenant")
		return
	}
	ms, err := s.store().ListMemberships(r.Context(), t.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"memberships": nonNil(ms)})
}

func (s *Server) createMembership(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tenantInScope(w, r)
	if !ok {
		return
	}
	var in struct {
		Subject   string     `json:"subject"`
		Role      string     `json:"role"`
		ProjectID *uuid.UUID `json:"project_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Subject == "" || !rbac.RoleFitsTenant(in.Role, t.Kind) {
		writeErr(w, http.StatusBadRequest, "subject obrigatório e role compatível com o tipo do tenant")
		return
	}
	if !principal(r).CanAssign(in.Role, t.Path, t.Kind) {
		writeErr(w, http.StatusForbidden, "sem permissão para atribuir esse papel")
		return
	}
	st := s.store()
	if in.ProjectID != nil {
		p, err := st.GetProject(r.Context(), *in.ProjectID)
		if err != nil || p.TenantID != t.ID {
			writeErr(w, http.StatusBadRequest, "project_id não pertence a este tenant")
			return
		}
	}
	err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := st.WithTx(tx)
		if err := ts.CreateMembership(r.Context(), in.Subject, t.ID, in.ProjectID, in.Role); err != nil {
			return err
		}
		return s.audit(r, ts, "membership.create", in.Subject, &t.ID, map[string]any{"role": in.Role, "project_id": in.ProjectID})
	})
	if errors.Is(err, store.ErrConflict) {
		writeErr(w, http.StatusConflict, "vínculo já existe")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// ---------- clusters (equipe interna) ----------

func (s *Server) canClusters(r *http.Request, perm rbac.Permission) bool {
	for _, g := range principal(r).Scope(perm) {
		if g.SeesUnassigned { // vínculo de provedor sem restrição de projeto
			return true
		}
	}
	return false
}

// ---------- resources ----------

func (s *Server) listResources(w http.ResponseWriter, r *http.Request) {
	rs, err := s.store().ListResources(r.Context(), principal(r).Scope(rbac.ResourceRead))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resources": nonNil(rs)})
}

func (s *Server) getResource(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	res, err := s.store().GetResource(r.Context(), principal(r).Scope(rbac.ResourceRead), id)
	if isNotFound(err) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

var validActions = map[string]bool{"start": true, "shutdown": true, "stop": true, "reboot": true}

func (s *Server) createAction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Action string `json:"action"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !validActions[in.Action] {
		writeErr(w, http.StatusBadRequest, "action deve ser start, shutdown, stop ou reboot")
		return
	}
	p := principal(r)
	st := s.store()
	// Fora do escopo de leitura: 404 (não revela que existe).
	if _, err := st.GetResource(r.Context(), p.Scope(rbac.ResourceRead), id); isNotFound(err) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	res, err := st.GetResource(r.Context(), p.Scope(rbac.ResourcePower), id)
	if isNotFound(err) {
		writeErr(w, http.StatusForbidden, "sem permissão para esta ação")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}

	var task store.Task
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := st.WithTx(tx)
		// Ligar não pode estourar a cota do VDC (checagem com o VDC travado).
		if in.Action == "start" {
			if err := quota.CheckPowerOn(r.Context(), ts, res.ID); err != nil {
				return err
			}
		}
		var err error
		if task, err = ts.CreateTask(r.Context(), res.ID, res.TenantID, in.Action, p.Subject); err != nil {
			return err
		}
		// Task e job na mesma transação.
		if _, err := s.River.InsertTx(r.Context(), tx, jobs.PowerArgs{TaskID: task.ID}, nil); err != nil {
			return err
		}
		return s.audit(r, ts, "resource."+in.Action, res.ID.String(), res.TenantID,
			map[string]any{"task_id": task.ID, "staff": p.IsProviderStaff()})
	})
	if errors.Is(err, store.ErrConflict) {
		writeErr(w, http.StatusConflict, "já existe uma ação em andamento para este recurso")
		return
	} else if s.quotaErr(w, err) {
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/tasks/"+task.ID.String())
	writeJSON(w, http.StatusAccepted, task)
}

func (s *Server) listResourceTasks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	st := s.store()
	if _, err := st.GetResource(r.Context(), principal(r).Scope(rbac.ResourceRead), id); isNotFound(err) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	ts, err := st.ListTasks(r.Context(), id, 20)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": nonNil(ts)})
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	t, err := s.store().GetTask(r.Context(), principal(r).Scope(rbac.ResourceRead), id)
	if isNotFound(err) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
