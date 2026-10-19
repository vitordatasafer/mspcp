package jobs

// Tasks de ciclo de vida: criar VM a partir de imagem, excluir e atribuir a
// um VDC. Cada passo concluído fica gravado nos params da Task, então um
// retry da River continua de onde parou (não clona duas vezes, não escolhe
// outro vmid).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/mspclouds/mspcp/internal/proxmox"
	"github.com/mspclouds/mspcp/internal/store"
)

// VMArgs carrega só o ID da Task, como PowerArgs.
type VMArgs struct {
	TaskID uuid.UUID `json:"task_id"`
}

func (VMArgs) Kind() string { return "vm_task" }

func (VMArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{MaxAttempts: 8} }

type VMWorker struct {
	river.WorkerDefaults[VMArgs]
	Pool         *pgxpool.Pool
	Clients      *proxmox.Clients
	PollInterval time.Duration
	Log          *slog.Logger
}

func (w *VMWorker) Timeout(*river.Job[VMArgs]) time.Duration { return 45 * time.Minute }

// permanent marca um erro que não adianta repetir.
type permanent struct{ error }

func perm(format string, a ...any) error { return permanent{fmt.Errorf(format, a...)} }

func isPermanent(err error) bool {
	var p permanent
	var tf *proxmox.TaskFailed
	var api *proxmox.APIError
	if errors.As(err, &p) || errors.As(err, &tf) || errors.Is(err, store.ErrNotFound) {
		return true
	}
	return errors.As(err, &api) && api.Status >= 400 && api.Status < 500
}

func (w *VMWorker) Work(ctx context.Context, job *river.Job[VMArgs]) error {
	st := store.New(w.Pool)
	task, err := st.WorkerTask(ctx, job.Args.TaskID)
	if err != nil {
		return river.JobCancel(fmt.Errorf("task %s: %w", job.Args.TaskID, err))
	}
	if task.Status == "succeeded" || task.Status == "failed" {
		return nil
	}
	if err := st.MarkTaskRunning(ctx, task.ID, ""); err != nil {
		return err
	}
	run := &vmRun{w: w, st: st, task: task, params: task.Params}
	switch task.Action {
	case "create":
		err = run.create(ctx)
	case "delete":
		err = run.delete(ctx)
	case "assign":
		err = run.assign(ctx)
	default:
		err = perm("ação desconhecida %q", task.Action)
	}
	if err == nil {
		return nil
	}
	if isPermanent(err) || job.Attempt >= job.MaxAttempts {
		w.Log.Warn("task falhou", "task", task.ID, "action", task.Action, "err", err)
		if ferr := st.FinishTask(ctx, task.ID, false, friendly(err)); ferr != nil {
			return ferr
		}
		run.onFail(ctx)
		return river.JobCancel(err)
	}
	return err // transitório: a River tenta de novo
}

// friendly tira o prefixo técnico das mensagens mais comuns.
func friendly(err error) string {
	msg := err.Error()
	var p permanent
	if errors.As(err, &p) {
		msg = p.error.Error()
	}
	return msg
}

type vmRun struct {
	w      *VMWorker
	st     *store.Store
	task   store.WorkerTask
	params map[string]any
}

func (r *vmRun) save(ctx context.Context, kv map[string]any) error {
	for k, v := range kv {
		r.params[k] = v
	}
	return r.st.MergeTaskParams(ctx, r.task.ID, kv)
}

func (r *vmRun) str(k string) string {
	s, _ := r.params[k].(string)
	return s
}

func (r *vmRun) num(k string) int {
	switch v := r.params[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func (r *vmRun) flag(k string) bool {
	b, _ := r.params[k].(bool)
	return b
}

func (r *vmRun) client(ctx context.Context, clusterID uuid.UUID) (*proxmox.Client, error) {
	c, err := r.st.GetCluster(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	return r.w.Clients.For(ctx, c)
}

// waitUPID grava o UPID antes de esperar: num retry, só volta a acompanhar.
func (r *vmRun) waitUPID(ctx context.Context, cl *proxmox.Client, node, key string, start func() (string, error)) error {
	upid := r.str(key)
	if upid == "" {
		var err error
		if upid, err = start(); err != nil {
			return err
		}
		if upid == "" {
			return nil // operação síncrona
		}
		if err := r.save(ctx, map[string]any{key: upid}); err != nil {
			return err
		}
	}
	return cl.WaitTask(ctx, node, upid, r.w.PollInterval)
}

func (r *vmRun) onFail(ctx context.Context) {
	if r.task.Action == "delete" && r.task.ResourceID != nil {
		_ = r.st.SetResourceDeleting(ctx, *r.task.ResourceID, false)
	}
}

// ensurePool cria o pool do VDC no cluster (idempotente).
func (r *vmRun) ensurePool(ctx context.Context, cl *proxmox.Client, p store.Project) error {
	t, err := r.st.GetTenant(ctx, p.TenantID)
	if err != nil {
		return err
	}
	err = cl.CreatePool(ctx, p.PoolName, "mspcp: "+t.Name+" / "+p.Name)
	if err != nil {
		msg := err.Error()
		_ = r.st.SetPoolState(ctx, p.ID, false, &msg)
		return err
	}
	return r.st.SetPoolState(ctx, p.ID, true, nil)
}

// ---------- criar ----------

func (r *vmRun) create(ctx context.Context) error {
	if r.task.ProjectID == nil {
		return perm("task sem VDC")
	}
	p, err := r.st.GetProject(ctx, *r.task.ProjectID)
	if err != nil {
		return err
	}
	if p.ClusterID == nil {
		return perm("VDC sem localização")
	}
	clusterID := *p.ClusterID
	cl, err := r.client(ctx, clusterID)
	if err != nil {
		return err
	}
	tpl, err := r.st.Template(ctx, clusterID, r.num("template_vmid"))
	if err != nil {
		return perm("o template da imagem não existe mais no cluster")
	}
	classID, err := uuid.Parse(r.str("storage_class_id"))
	if err != nil {
		return perm("classe de storage inválida")
	}
	storage, err := r.st.StoragePolicyFor(ctx, clusterID, classID)
	if err != nil {
		return perm("a classe de storage não está mapeada nesta localização")
	}
	if err := r.ensurePool(ctx, cl, p); err != nil {
		return err
	}

	vmid := r.num("vmid")
	if vmid == 0 {
		if vmid, err = cl.NextID(ctx); err != nil {
			return err
		}
		if err := r.save(ctx, map[string]any{"vmid": vmid}); err != nil {
			return err
		}
	}
	node := tpl.Node
	name := r.str("name")

	if !r.flag("cloned") {
		err := r.waitUPID(ctx, cl, node, "clone_upid", func() (string, error) {
			return cl.Clone(ctx, proxmox.CloneSpec{Node: node, Template: tpl.VMID, NewID: vmid, Name: name, Pool: p.PoolName, Storage: storage})
		})
		if err != nil {
			return err
		}
		if err := r.save(ctx, map[string]any{"cloned": true}); err != nil {
			return err
		}
	}

	if !r.flag("configured") {
		cfg, err := cl.Config(ctx, node, vmid)
		if err != nil {
			return err
		}
		form := url.Values{
			"cores":   {fmt.Sprint(r.num("cpu_cores"))},
			"sockets": {"1"},
			"memory":  {fmt.Sprint(r.num("memory_mb"))},
		}
		bridge := ""
		if p.Bridge != nil && *p.Bridge != "" {
			bridge = *p.Bridge
		} else if bridge, err = r.st.ClusterDefaultBridge(ctx, clusterID); err != nil {
			return err
		}
		if bridge != "" {
			net := "virtio,bridge=" + bridge
			if p.VLANTag != nil {
				net += fmt.Sprintf(",tag=%d", *p.VLANTag)
			}
			if old, ok := cfg["net0"].(string); ok {
				// Mantém o MAC que o clone gerou.
				if mac := macOf(old); mac != "" {
					net = "virtio=" + mac + strings.TrimPrefix(net, "virtio")
				}
			}
			form.Set("net0", net)
		}
		if r.str("os_family") != "windows" {
			hasCI := false
			for _, v := range cfg {
				if s, ok := v.(string); ok && strings.Contains(s, "cloudinit") {
					hasCI = true
				}
			}
			if !hasCI {
				if _, used := cfg["ide2"]; !used {
					form.Set("ide2", storage+":cloudinit")
				}
			}
			if u := r.str("ci_user"); u != "" {
				form.Set("ciuser", u)
			}
			if h := r.str("ci_password_hash"); h != "" {
				form.Set("cipassword", h) // o PVE guarda o hash como veio
			}
			if k := r.str("ssh_keys"); k != "" {
				// O PVE espera o valor já codificado como URL.
				form.Set("sshkeys", strings.ReplaceAll(url.QueryEscape(k), "+", "%20"))
			}
			if ip := r.str("ipconfig0"); ip != "" {
				form.Set("ipconfig0", ip)
			}
		}
		if err := cl.SetConfig(ctx, node, vmid, form); err != nil {
			return err
		}
		if err := r.save(ctx, map[string]any{"configured": true}); err != nil {
			return err
		}
	}

	diskGB := r.num("disk_gb")
	if !r.flag("resized") {
		cfg, err := cl.Config(ctx, node, vmid)
		if err != nil {
			return err
		}
		if d, ok := cfg.BootDisk(); ok && d.SizeGB < diskGB {
			if err := r.waitUPID(ctx, cl, node, "resize_upid", func() (string, error) {
				return cl.Resize(ctx, node, vmid, d.Key, diskGB)
			}); err != nil {
				return err
			}
		}
		if err := r.save(ctx, map[string]any{"resized": true}); err != nil {
			return err
		}
	}

	// Já aparece no portal, sem esperar o sincronizador.
	var resID uuid.UUID
	err = pgx.BeginFunc(ctx, r.w.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		var err error
		resID, err = ts.UpsertResource(ctx, clusterID, store.Snapshot{
			VMID: vmid, Kind: "qemu", Node: node, Name: name, Status: "stopped",
			CPUs: r.num("cpu_cores"), MaxMemBytes: int64(r.num("memory_mb")) << 20, MaxDiskBytes: int64(diskGB) << 30,
		}, &p.TenantID, &p.ID)
		if err != nil {
			return err
		}
		if err := ts.SetResourceDisks(ctx, resID, map[uuid.UUID]int64{classID: int64(diskGB)}); err != nil {
			return err
		}
		return ts.SetTaskResource(ctx, r.task.ID, resID)
	})
	if err != nil {
		return err
	}

	if r.flag("start") {
		if err := r.waitUPID(ctx, cl, node, "start_upid", func() (string, error) {
			return cl.Power(ctx, node, "qemu", vmid, "start")
		}); err != nil {
			return err
		}
		if err := r.st.SetResourceStatus(ctx, resID, "running"); err != nil {
			return err
		}
	}
	return r.st.FinishTask(ctx, r.task.ID, true, "")
}

func macOf(net string) string {
	for _, part := range strings.Split(net, ",") {
		k, v, ok := strings.Cut(part, "=")
		if ok && (k == "virtio" || k == "e1000" || k == "vmxnet3" || k == "rtl8139" || k == "macaddr") && strings.Count(v, ":") == 5 {
			return v
		}
	}
	return ""
}

// ---------- excluir ----------

func (r *vmRun) delete(ctx context.Context) error {
	if r.task.ResourceID == nil {
		return perm("task sem recurso")
	}
	res, err := r.st.ResourceInternal(ctx, *r.task.ResourceID)
	if err != nil {
		return err
	}
	cl, err := r.client(ctx, res.ClusterID)
	if err != nil {
		return err
	}
	if !r.flag("stopped") {
		if res.Status == "running" {
			err := r.waitUPID(ctx, cl, res.Node, "stop_upid", func() (string, error) {
				return cl.Power(ctx, res.Node, res.Kind, res.VMID, "stop")
			})
			if err != nil {
				return err
			}
		}
		if err := r.save(ctx, map[string]any{"stopped": true}); err != nil {
			return err
		}
	}
	err = r.waitUPID(ctx, cl, res.Node, "delete_upid", func() (string, error) {
		u, err := cl.Delete(ctx, res.Node, res.Kind, res.VMID)
		var api *proxmox.APIError
		if errors.As(err, &api) && strings.Contains(api.Body, "does not exist") {
			return "", nil // já não existe: nada a fazer
		}
		return u, err
	})
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, r.w.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		if err := ts.FinishTask(ctx, r.task.ID, true, ""); err != nil {
			return err
		}
		return ts.MarkResourceGone(ctx, res.ResourceID)
	})
}

// ---------- atribuir ----------

func (r *vmRun) assign(ctx context.Context) error {
	if r.task.ResourceID == nil {
		return perm("task sem recurso")
	}
	res, err := r.st.ResourceInternal(ctx, *r.task.ResourceID)
	if err != nil {
		return err
	}
	cl, err := r.client(ctx, res.ClusterID)
	if err != nil {
		return err
	}
	target := r.str("target_project_id")
	if target == "" { // tirar do VDC
		if res.ProjectID != nil {
			cur, err := r.st.GetProject(ctx, *res.ProjectID)
			if err != nil {
				return err
			}
			if err := cl.RemoveFromPool(ctx, cur.PoolName, res.VMID); err != nil {
				return err
			}
		}
		return pgx.BeginFunc(ctx, r.w.Pool, func(tx pgx.Tx) error {
			ts := store.New(tx)
			if err := ts.SetResourceOwner(ctx, res.ResourceID, nil, nil); err != nil {
				return err
			}
			if err := ts.SetResourceDisks(ctx, res.ResourceID, nil); err != nil {
				return err
			}
			return ts.FinishTask(ctx, r.task.ID, true, "")
		})
	}
	pid, err := uuid.Parse(target)
	if err != nil {
		return perm("VDC inválido")
	}
	p, err := r.st.GetProject(ctx, pid)
	if err != nil {
		return err
	}
	if err := r.ensurePool(ctx, cl, p); err != nil {
		return err
	}
	if err := cl.MoveToPool(ctx, p.PoolName, res.VMID); err != nil {
		return err
	}
	// Discos por classe, a partir do storage de cada disco.
	disks := map[uuid.UUID]int64{}
	if res.Kind == "qemu" {
		cfg, err := cl.Config(ctx, res.Node, res.VMID)
		if err != nil {
			return err
		}
		byStorage, err := r.st.ClassesByStorage(ctx, res.ClusterID)
		if err != nil {
			return err
		}
		for _, d := range cfg.Disks() {
			if c, ok := byStorage[d.Storage]; ok {
				disks[c] += int64(d.SizeGB)
			}
		}
	}
	return pgx.BeginFunc(ctx, r.w.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		if err := ts.SetResourceOwner(ctx, res.ResourceID, &p.TenantID, &p.ID); err != nil {
			return err
		}
		if err := ts.SetResourceDisks(ctx, res.ResourceID, disks); err != nil {
			return err
		}
		return ts.FinishTask(ctx, r.task.ID, true, "")
	})
}
