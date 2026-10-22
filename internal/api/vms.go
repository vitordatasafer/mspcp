package api

// Ciclo de vida das VMs: catálogo de imagens, criar a partir de imagem,
// excluir, atribuir VM existente a um VDC e rede do VDC.

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vitordatasafer/mspvdc/internal/jobs"
	"github.com/vitordatasafer/mspvdc/internal/localauth"
	"github.com/vitordatasafer/mspvdc/internal/proxmox"
	"github.com/vitordatasafer/mspvdc/internal/quota"
	"github.com/vitordatasafer/mspvdc/internal/rbac"
	"github.com/vitordatasafer/mspvdc/internal/store"
)

// ---------- templates e imagens ----------

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	if !s.canClusters(r, rbac.ClusterRead) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ts, err := s.store().ListTemplates(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": nonNil(ts)})
}

type imageOut struct {
	store.Image
	TemplateVMID *int `json:"template_vmid,omitempty"`
}

func (s *Server) listImages(w http.ResponseWriter, r *http.Request) {
	var loc *uuid.UUID
	if v := r.URL.Query().Get("location_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "location_id inválido")
			return
		}
		loc = &id
	}
	staff := s.canClusters(r, rbac.ClusterRead)
	imgs, err := s.store().ListImages(r.Context(), loc, !staff)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := []imageOut{}
	for _, i := range imgs {
		o := imageOut{Image: i}
		if staff {
			v := i.VMID
			o.TemplateVMID = &v
		} else if !i.Available {
			continue
		}
		out = append(out, o)
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": out})
}

func (s *Server) createImage(w http.ResponseWriter, r *http.Request) {
	if !s.canClusters(r, rbac.ClusterManage) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	var in struct {
		LocationID uuid.UUID `json:"location_id"`
		VMID       int       `json:"template_vmid"`
		Name       string    `json:"name"`
		OSFamily   string    `json:"os_family"`
		MinDiskGB  int       `json:"min_disk_gb"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.OSFamily == "" {
		in.OSFamily = "linux"
	}
	if in.Name == "" || len(in.Name) > 80 || (in.OSFamily != "linux" && in.OSFamily != "windows") {
		writeErr(w, http.StatusBadRequest, "informe o nome e o sistema (linux ou windows)")
		return
	}
	st := s.store()
	tpl, err := st.Template(r.Context(), in.LocationID, in.VMID)
	if isNotFound(err) {
		writeErr(w, http.StatusBadRequest, "template não encontrado nesse cluster (aguarde a sincronização)")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	minDisk := int((tpl.MaxDiskBytes + (1<<30 - 1)) >> 30)
	if in.MinDiskGB > minDisk {
		minDisk = in.MinDiskGB
	}
	if minDisk <= 0 {
		minDisk = 10
	}
	var img store.Image
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		var err error
		if img, err = ts.CreateImage(r.Context(), in.LocationID, in.VMID, in.Name, in.OSFamily, minDisk); err != nil {
			return err
		}
		return s.audit(r, ts, "image.create", img.ID.String(), nil, map[string]any{"cluster_id": in.LocationID, "vmid": in.VMID, "name": in.Name})
	})
	if errors.Is(err, store.ErrConflict) {
		writeErr(w, http.StatusConflict, "esse template já está publicado, ou o nome já existe nessa localização")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, img)
}

func (s *Server) deleteImage(w http.ResponseWriter, r *http.Request) {
	if !s.canClusters(r, rbac.ClusterManage) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		if err := ts.DeleteImage(r.Context(), id); err != nil {
			return err
		}
		return s.audit(r, ts, "image.delete", id.String(), nil, nil)
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

// ---------- criar VM ----------

var (
	ciUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	sshKeyRe = regexp.MustCompile(`^(ssh-(rsa|ed25519|dss)|ecdsa-sha2-nistp(256|384|521)|sk-(ssh-ed25519|ecdsa-sha2-nistp256)@openssh\.com) [A-Za-z0-9+/=]+( .*)?$`)
)

// projectInScope devolve o VDC se o usuário tiver `perm` nele; fora do
// escopo de leitura responde 404.
func (s *Server) projectInScope(w http.ResponseWriter, r *http.Request, perm rbac.Permission) (store.Project, store.Tenant, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return store.Project{}, store.Tenant{}, false
	}
	st := s.store()
	p, err := st.GetProject(r.Context(), id)
	if isNotFound(err) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return p, store.Tenant{}, false
	} else if err != nil {
		s.internal(w, r, err)
		return p, store.Tenant{}, false
	}
	t, err := st.GetTenant(r.Context(), p.TenantID)
	if err != nil {
		s.internal(w, r, err)
		return p, t, false
	}
	pr := principal(r)
	if !pr.Can(rbac.ResourceRead, t.Path, &p.ID) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return p, t, false
	}
	if !pr.Can(perm, t.Path, &p.ID) {
		writeErr(w, http.StatusForbidden, "sem permissão para esta ação")
		return p, t, false
	}
	return p, t, true
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	p, t, ok := s.projectInScope(w, r, rbac.ResourceCreate)
	if !ok {
		return
	}
	var in struct {
		Name      string    `json:"name"`
		ImageID   uuid.UUID `json:"image_id"`
		CPUCores  int       `json:"cpu_cores"`
		MemoryMB  int64     `json:"memory_mb"`
		DiskGB    int64     `json:"disk_gb"`
		ClassID   uuid.UUID `json:"storage_class_id"`
		Username  string    `json:"username"`
		Password  string    `json:"password"`
		SSHKeys   string    `json:"ssh_keys"`
		IPMode    string    `json:"ip_mode"` // dhcp | static
		IPAddress string    `json:"ip_address"`
		Gateway   string    `json:"gateway"`
		Start     bool      `json:"start"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.ToLower(strings.TrimSpace(in.Name))
	if !proxmox.ValidVMName(in.Name) {
		writeErr(w, http.StatusBadRequest, "nome: letras minúsculas, números e hífen (como um nome de host), até 63 caracteres")
		return
	}
	if in.CPUCores < 1 || in.CPUCores > 128 || in.MemoryMB < 256 || in.MemoryMB > 4<<20 || in.DiskGB < 1 || in.DiskGB > 64<<10 {
		writeErr(w, http.StatusBadRequest, "vCPU, memória ou disco fora dos limites")
		return
	}
	st := s.store()
	img, err := st.GetImage(r.Context(), in.ImageID)
	if err != nil || !img.Enabled || !img.Available || p.ClusterID == nil || img.LocationID != *p.ClusterID {
		writeErr(w, http.StatusBadRequest, "imagem indisponível na localização deste VDC")
		return
	}
	if in.DiskGB < int64(img.MinDiskGB) {
		writeErr(w, http.StatusUnprocessableEntity, fmt.Sprintf("esta imagem precisa de pelo menos %d GB de disco", img.MinDiskGB))
		return
	}
	params := map[string]any{
		"name": in.Name, "image_id": img.ID, "template_vmid": img.VMID, "os_family": img.OSFamily,
		"cpu_cores": in.CPUCores, "memory_mb": in.MemoryMB, "disk_gb": in.DiskGB,
		"storage_class_id": in.ClassID, "start": in.Start,
	}
	if img.OSFamily == "linux" {
		in.Username = strings.TrimSpace(in.Username)
		if !ciUserRe.MatchString(in.Username) || in.Username == "root" {
			writeErr(w, http.StatusBadRequest, "usuário da VM: letras minúsculas, números, - ou _ (não pode ser root)")
			return
		}
		keys := []string{}
		for _, line := range strings.Split(strings.ReplaceAll(in.SSHKeys, "\r", ""), "\n") {
			if line = strings.TrimSpace(line); line == "" {
				continue
			}
			if !sshKeyRe.MatchString(line) {
				writeErr(w, http.StatusBadRequest, "chave SSH inválida: use o conteúdo do arquivo .pub (ssh-ed25519 AAAA…)")
				return
			}
			keys = append(keys, line)
		}
		if len(in.SSHKeys) > 16<<10 || len(keys) > 10 {
			writeErr(w, http.StatusBadRequest, "chaves SSH demais")
			return
		}
		if in.Password == "" && len(keys) == 0 {
			writeErr(w, http.StatusBadRequest, "informe uma senha ou uma chave SSH para acessar a VM")
			return
		}
		if in.Password != "" {
			if len(in.Password) < 8 || len(in.Password) > 128 {
				writeErr(w, http.StatusBadRequest, "a senha da VM precisa ter de 8 a 128 caracteres")
				return
			}
			params["ci_password_hash"] = localauth.SHA512Crypt(in.Password)
		}
		params["ci_user"] = in.Username
		if len(keys) > 0 {
			params["ssh_keys"] = strings.Join(keys, "\n")
		}
		switch in.IPMode {
		case "", "dhcp":
			params["ipconfig0"] = "ip=dhcp"
		case "static":
			pfx, err := netip.ParsePrefix(strings.TrimSpace(in.IPAddress))
			gw, gerr := netip.ParseAddr(strings.TrimSpace(in.Gateway))
			if err != nil || !pfx.Addr().Is4() || pfx.Bits() < 8 || pfx.Bits() > 32 {
				writeErr(w, http.StatusBadRequest, "IP no formato 10.0.0.10/24")
				return
			}
			if gerr != nil || !pfx.Masked().Contains(gw) || gw == pfx.Addr() {
				writeErr(w, http.StatusBadRequest, "o gateway precisa estar na mesma rede do IP")
				return
			}
			params["ipconfig0"] = fmt.Sprintf("ip=%s,gw=%s", pfx, gw)
		default:
			writeErr(w, http.StatusBadRequest, "ip_mode deve ser dhcp ou static")
			return
		}
	}

	var task store.Task
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		cur, err := ts.GetProject(r.Context(), p.ID)
		if err != nil {
			return err
		}
		if err := quota.CheckCreate(r.Context(), ts, cur, t, quota.NewVM{
			CPUCores: in.CPUCores, MemoryMB: in.MemoryMB, DiskGB: in.DiskGB, ClassID: in.ClassID, StartAfter: in.Start,
		}); err != nil {
			return err
		}
		if task, err = ts.InsertTask(r.Context(), store.NewTask{
			TenantID: &t.ID, ProjectID: &p.ID, Action: "create", RequestedBy: principal(r).Subject, Label: in.Name, Params: params,
		}); err != nil {
			return err
		}
		if _, err := s.River.InsertTx(r.Context(), tx, jobs.VMArgs{TaskID: task.ID}, nil); err != nil {
			return err
		}
		return s.audit(r, ts, "resource.create", in.Name, &t.ID, map[string]any{
			"task_id": task.ID, "project_id": p.ID, "image": img.Name, "cpu_cores": in.CPUCores, "memory_mb": in.MemoryMB, "disk_gb": in.DiskGB,
		})
	})
	if s.quotaErr(w, err) {
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/tasks/"+task.ID.String())
	writeJSON(w, http.StatusAccepted, task)
}

// ---------- excluir VM ----------

func (s *Server) deleteVM(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Confirm string `json:"confirm_name"`
	}
	if !decode(w, r, &in) {
		return
	}
	p := principal(r)
	st := s.store()
	res, err := st.GetResource(r.Context(), p.Scope(rbac.ResourceRead), id)
	if isNotFound(err) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	if _, err := st.GetResource(r.Context(), p.Scope(rbac.ResourceDelete), id); isNotFound(err) {
		writeErr(w, http.StatusForbidden, "sem permissão para excluir")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	if in.Confirm != res.Alias {
		writeErr(w, http.StatusBadRequest, "para confirmar, digite o nome exato do servidor")
		return
	}
	var task store.Task
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		var err error
		if task, err = ts.InsertTask(r.Context(), store.NewTask{
			ResourceID: &res.ID, TenantID: res.TenantID, ProjectID: res.ProjectID, Action: "delete", RequestedBy: p.Subject, Label: res.Alias,
		}); err != nil {
			return err
		}
		if err := ts.SetResourceDeleting(r.Context(), res.ID, true); err != nil {
			return err
		}
		if _, err := s.River.InsertTx(r.Context(), tx, jobs.VMArgs{TaskID: task.ID}, nil); err != nil {
			return err
		}
		return s.audit(r, ts, "resource.delete", res.ID.String(), res.TenantID, map[string]any{"task_id": task.ID, "alias": res.Alias})
	})
	if errors.Is(err, store.ErrConflict) {
		writeErr(w, http.StatusConflict, "já existe uma ação em andamento para este servidor")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

// ---------- atribuir VM existente a um VDC ----------

func (s *Server) assignVM(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		ProjectID *uuid.UUID `json:"project_id"` // nil = tirar do VDC
	}
	if !decode(w, r, &in) {
		return
	}
	p := principal(r)
	st := s.store()
	if _, err := st.GetResource(r.Context(), p.Scope(rbac.ResourceRead), id); isNotFound(err) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	// Mover VM entre contas é da equipe da plataforma.
	if !s.canClusters(r, rbac.ResourceAssign) {
		writeErr(w, http.StatusForbidden, "só a administração da plataforma atribui VMs")
		return
	}
	var task store.Task
	err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		res, err := ts.ResourceInternal(r.Context(), id)
		if err != nil {
			return err
		}
		params := map[string]any{}
		var tenantID, projectID *uuid.UUID
		label := res.Alias
		if in.ProjectID != nil {
			proj, err := ts.GetProject(r.Context(), *in.ProjectID)
			if isNotFound(err) {
				return &quota.Error{Msg: "VDC não encontrado"}
			} else if err != nil {
				return err
			}
			owner, err := ts.GetTenant(r.Context(), proj.TenantID)
			if err != nil {
				return err
			}
			if err := quota.CheckAssign(r.Context(), ts, proj, owner, res); err != nil {
				return err
			}
			params["target_project_id"] = proj.ID.String()
			tenantID, projectID = &proj.TenantID, &proj.ID
		} else {
			if res.ProjectID == nil {
				return &quota.Error{Msg: "a VM já está sem VDC"}
			}
			tenantID, projectID = res.TenantID, res.ProjectID
		}
		if task, err = ts.InsertTask(r.Context(), store.NewTask{
			ResourceID: &res.ResourceID, TenantID: tenantID, ProjectID: projectID, Action: "assign",
			RequestedBy: p.Subject, Label: label, Params: params,
		}); err != nil {
			return err
		}
		if _, err := s.River.InsertTx(r.Context(), tx, jobs.VMArgs{TaskID: task.ID}, nil); err != nil {
			return err
		}
		return s.audit(r, ts, "resource.assign", res.ResourceID.String(), tenantID, map[string]any{"task_id": task.ID, "project_id": in.ProjectID})
	})
	if errors.Is(err, store.ErrConflict) {
		writeErr(w, http.StatusConflict, "já existe uma ação em andamento para este servidor")
		return
	} else if s.quotaErr(w, err) {
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

// ---------- rede do VDC (equipe) ----------

var bridgeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,14}$`)

func (s *Server) setProjectNetwork(w http.ResponseWriter, r *http.Request) {
	if !s.canClusters(r, rbac.ClusterManage) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	var in struct {
		Bridge  string `json:"bridge"`
		VLANTag *int   `json:"vlan_tag"`
	}
	if !decode(w, r, &in) {
		return
	}
	var bridge *string
	if in.Bridge = strings.TrimSpace(in.Bridge); in.Bridge != "" {
		if !bridgeRe.MatchString(in.Bridge) {
			writeErr(w, http.StatusBadRequest, "bridge inválida (ex.: vmbr0 ou o nome de uma VNet)")
			return
		}
		bridge = &in.Bridge
	}
	if in.VLANTag != nil && (*in.VLANTag < 1 || *in.VLANTag > 4094) {
		writeErr(w, http.StatusBadRequest, "VLAN entre 1 e 4094")
		return
	}
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		p, err := ts.GetProject(r.Context(), id)
		if err != nil {
			return err
		}
		if err := ts.SetProjectNetwork(r.Context(), id, bridge, in.VLANTag); err != nil {
			return err
		}
		return s.audit(r, ts, "vdc.network", id.String(), &p.TenantID, map[string]any{"bridge": in.Bridge, "vlan_tag": in.VLANTag})
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
