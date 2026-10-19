import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, gbText, type Image, type Project, type Resource, type Task } from "../api";
import { Modal } from "./Modal";
import { useSession } from "../session";

const projectsQuery = {
  queryKey: ["projects", "all"],
  queryFn: () => api<{ projects: Project[] }>("/v1/projects").then((r) => r.projects),
};

function defaultUser(image?: Image) {
  const n = (image?.name ?? "").toLowerCase();
  if (n.includes("ubuntu")) return "ubuntu";
  if (n.includes("debian")) return "debian";
  if (n.includes("rocky") || n.includes("alma")) return "rocky";
  return "admin";
}

// ---------- Nova VM ----------

export function NewVMModal({ onClose, projectId }: { onClose: () => void; projectId?: string }) {
  const qc = useQueryClient();
  const { multiTenant } = useSession();
  const projects = useQuery(projectsQuery);
  const images = useQuery({ queryKey: ["images"], queryFn: () => api<{ images: Image[] }>("/v1/images").then((r) => r.images) });
  const [vdcId, setVdcId] = useState(projectId ?? "");
  const vdc = projects.data?.find((p) => p.id === vdcId) ?? (projects.data?.length === 1 ? projects.data[0] : undefined);
  const imgs = useMemo(
    () => (images.data ?? []).filter((i) => i.enabled && i.available && vdc && i.location_id === vdc.location_id),
    [images.data, vdc],
  );
  const [imageId, setImageId] = useState("");
  const image = imgs.find((i) => i.id === imageId) ?? imgs[0];
  const linux = image?.os_family !== "windows";

  const [name, setName] = useState("");
  const [cpu, setCpu] = useState("2");
  const [mem, setMem] = useState("2");
  const [disk, setDisk] = useState("");
  const [classId, setClassId] = useState("");
  const [user, setUser] = useState("");
  const [password, setPassword] = useState("");
  const [keys, setKeys] = useState("");
  const [ipMode, setIpMode] = useState<"dhcp" | "static">("dhcp");
  const [ip, setIp] = useState("");
  const [gw, setGw] = useState("");
  const [start, setStart] = useState(true);

  const cls = vdc?.storage.find((x) => x.class_id === classId) ?? vdc?.storage[0];
  const diskGB = disk === "" ? Math.max(image?.min_disk_gb ?? 10, 20) : Number(disk);
  const freeDisk = cls ? cls.capacity_gb - cls.used_gb : 0;
  const freeCPU = vdc ? vdc.cpu_cores - (vdc.usage?.running_cpu ?? 0) : 0;
  const freeMem = vdc ? Math.floor((vdc.memory_mb - (vdc.usage?.running_memory_mb ?? 0)) / 1024) : 0;
  const nameOk = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(name);

  const create = useMutation({
    mutationFn: () =>
      api<Task>(`/v1/projects/${vdc!.id}/vms`, {
        method: "POST",
        json: {
          name, image_id: image!.id, cpu_cores: Number(cpu), memory_mb: Math.round(Number(mem) * 1024), disk_gb: diskGB,
          storage_class_id: cls!.class_id, username: user || defaultUser(image), password, ssh_keys: keys,
          ip_mode: ipMode, ip_address: ip, gateway: gw, start,
        },
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["tasks"] });
      qc.invalidateQueries({ queryKey: ["resources"] });
      qc.invalidateQueries({ queryKey: ["projects"] });
      onClose();
    },
  });

  const noVDC = projects.data && projects.data.length === 0;
  const ready = !!vdc && !!image && !!cls && nameOk && Number(cpu) > 0 && Number(mem) > 0 && diskGB > 0 &&
    (!linux || password.length >= 8 || keys.trim() !== "") && (ipMode === "dhcp" || (ip && gw));

  return (
    <Modal title="Nova VM" onClose={onClose} wide
      footer={<>
        <button className="btn" onClick={onClose}>Cancelar</button>
        <button className="btn btn-primary" disabled={!ready || create.isPending} onClick={() => create.mutate()}>
          {create.isPending ? "Enviando…" : "Criar VM"}
        </button>
      </>}>
      {noVDC ? (
        <p className="muted">Você ainda não tem VDC. Peça ao seu provedor para reservar recursos.</p>
      ) : (
        <div className="stack" style={{ gap: 18 }}>
          <div className="form-grid">
            <label className="field">
              VDC
              <select className="select" value={vdc?.id ?? ""} onChange={(e) => { setVdcId(e.target.value); setImageId(""); setClassId(""); }}>
                {!vdc && <option value="">Escolha…</option>}
                {projects.data?.map((p) => (
                  <option key={p.id} value={p.id}>{multiTenant && p.tenant_name ? `${p.tenant_name} / ` : ""}{p.name} ({p.location})</option>
                ))}
              </select>
            </label>
            <label className="field">
              Imagem
              <select className="select" value={image?.id ?? ""} onChange={(e) => setImageId(e.target.value)} disabled={!vdc || imgs.length === 0}>
                {imgs.length === 0 && <option value="">Nenhuma imagem nesta localização</option>}
                {imgs.map((i) => <option key={i.id} value={i.id}>{i.name}</option>)}
              </select>
            </label>
            <label className="field">
              Nome
              <input className="input" value={name} onChange={(e) => setName(e.target.value.toLowerCase())} placeholder="web-01" aria-invalid={name !== "" && !nameOk} />
              <span className={`hint${name !== "" && !nameOk ? " err" : ""}`}>Letras minúsculas, números e hífen, como um nome de host.</span>
            </label>
          </div>

          <fieldset className="fs">
            <legend>Tamanho</legend>
            <div className="form-grid">
              <label className="field">
                vCPU
                <input className="input" type="number" min={1} max={vdc?.cpu_cores} value={cpu} onChange={(e) => setCpu(e.target.value)} />
                {vdc && <span className="hint">{freeCPU} de {vdc.cpu_cores} livres para ligar</span>}
              </label>
              <label className="field">
                Memória
                <span className="input-suffix"><input className="input" type="number" min={0.5} step={0.5} value={mem} onChange={(e) => setMem(e.target.value)} /><span>GB</span></span>
                {vdc && <span className="hint">{gbText(freeMem)} de {gbText(Math.round(vdc.memory_mb / 1024))} livres</span>}
              </label>
              <label className="field">
                Disco
                <span className="input-suffix">
                  <input className="input" type="number" min={image?.min_disk_gb ?? 1} value={disk === "" ? diskGB : disk} onChange={(e) => setDisk(e.target.value)} />
                  <span>GB</span>
                </span>
                {cls && <span className={`hint${diskGB > freeDisk ? " err" : ""}`}>{gbText(freeDisk)} livres em {cls.class_name}{image ? `; mínimo ${image.min_disk_gb} GB` : ""}</span>}
              </label>
              {(vdc?.storage.length ?? 0) > 1 && (
                <label className="field">
                  Storage
                  <select className="select" value={cls?.class_id ?? ""} onChange={(e) => setClassId(e.target.value)}>
                    {vdc!.storage.map((x) => <option key={x.class_id} value={x.class_id}>{x.class_name}</option>)}
                  </select>
                </label>
              )}
            </div>
          </fieldset>

          {linux && (
            <fieldset className="fs">
              <legend>Acesso</legend>
              <div className="form-grid">
                <label className="field">
                  Usuário
                  <input className="input" value={user} placeholder={defaultUser(image)} onChange={(e) => setUser(e.target.value.toLowerCase())} autoComplete="off" />
                </label>
                <label className="field">
                  Senha
                  <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
                  <span className="hint">Mínimo 8 caracteres. Opcional se informar uma chave SSH.</span>
                </label>
              </div>
              <label className="field" style={{ marginTop: 12 }}>
                Chaves SSH públicas
                <textarea className="textarea" rows={3} value={keys} onChange={(e) => setKeys(e.target.value)} placeholder="ssh-ed25519 AAAA… usuario@maquina" />
              </label>
            </fieldset>
          )}

          {linux && (
            <fieldset className="fs">
              <legend>Rede</legend>
              <div className="seg" role="group" aria-label="Endereço IP">
                <button type="button" aria-pressed={ipMode === "dhcp"} onClick={() => setIpMode("dhcp")}>DHCP</button>
                <button type="button" aria-pressed={ipMode === "static"} onClick={() => setIpMode("static")}>IP fixo</button>
              </div>
              {ipMode === "static" && (
                <div className="form-grid" style={{ marginTop: 12 }}>
                  <label className="field">Endereço<input className="input" value={ip} onChange={(e) => setIp(e.target.value)} placeholder="10.0.0.10/24" /></label>
                  <label className="field">Gateway<input className="input" value={gw} onChange={(e) => setGw(e.target.value)} placeholder="10.0.0.1" /></label>
                </div>
              )}
            </fieldset>
          )}

          <label className="checks"><span><input type="checkbox" checked={start} onChange={(e) => setStart(e.target.checked)} /> Ligar quando estiver pronta</span></label>
          {create.error && <div className="alert">{create.error.message}</div>}
        </div>
      )}
    </Modal>
  );
}

// ---------- Excluir ----------

export function DeleteVMModal({ r, onClose, onDone }: { r: Resource; onClose: () => void; onDone?: () => void }) {
  const qc = useQueryClient();
  const [typed, setTyped] = useState("");
  const del = useMutation({
    mutationFn: () => api<Task>(`/v1/resources/${r.id}`, { method: "DELETE", json: { confirm_name: typed } }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["resources"] });
      qc.invalidateQueries({ queryKey: ["tasks"] });
      onClose();
      onDone?.();
    },
  });
  return (
    <Modal title={`Excluir ${r.alias}?`} onClose={onClose}
      footer={<>
        <button className="btn" onClick={onClose}>Cancelar</button>
        <button className="btn btn-danger-solid" disabled={typed !== r.alias || del.isPending} onClick={() => del.mutate()}>Excluir definitivamente</button>
      </>}>
      <p>
        O servidor é desligado e apagado junto com os discos. <b>Não dá para desfazer.</b>
      </p>
      <label className="field" style={{ marginTop: 14 }}>
        Para confirmar, digite <b>{r.alias}</b>
        <input className="input" value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" spellCheck={false} />
      </label>
      {del.error && <div className="alert" style={{ marginTop: 12 }}>{del.error.message}</div>}
    </Modal>
  );
}

// ---------- Atribuir a VDC ----------

export function AssignVMModal({ r, onClose }: { r: Resource; onClose: () => void }) {
  const qc = useQueryClient();
  const projects = useQuery(projectsQuery);
  const options = (projects.data ?? []).filter((p) => p.location === r.location && p.id !== r.project_id);
  const [target, setTarget] = useState<string>("");
  const assign = useMutation({
    mutationFn: () => api<Task>(`/v1/resources/${r.id}/assign`, { method: "POST", json: { project_id: target === "none" ? null : target } }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["tasks"] });
      onClose();
    },
  });
  return (
    <Modal title={`Atribuir ${r.alias} a um VDC`} onClose={onClose}
      footer={<>
        <button className="btn" onClick={onClose}>Cancelar</button>
        <button className="btn btn-primary" disabled={!target || assign.isPending} onClick={() => assign.mutate()}>Mover</button>
      </>}>
      <p className="muted small" style={{ marginBottom: 12 }}>
        A VM passa para o pool do VDC no Proxmox e começa a contar na cota dele. Só aparecem VDCs da mesma localização ({r.location}).
      </p>
      <label className="field">
        VDC de destino
        <select className="select" value={target} onChange={(e) => setTarget(e.target.value)}>
          <option value="">Escolha…</option>
          {options.map((p) => <option key={p.id} value={p.id}>{p.tenant_name ? `${p.tenant_name} / ` : ""}{p.name}</option>)}
          {r.project_id && <option value="none">Tirar do VDC (fica sem dono)</option>}
        </select>
      </label>
      {options.length === 0 && !r.project_id && <p className="hint" style={{ marginTop: 8 }}>Nenhum VDC nesta localização.</p>}
      {assign.error && <div className="alert" style={{ marginTop: 12 }}>{assign.error.message}</div>}
    </Modal>
  );
}
