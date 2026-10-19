import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, bytes, when, type Cluster, type Image, type PVETemplate } from "../api";
import { useSession } from "../session";
import { Modal } from "../components/Modal";
import { IconRefresh } from "../components/Icons";

// Catálogo de imagens: a equipe publica templates do Proxmox com um nome
// amigável; MSPs e clientes criam VMs a partir delas.
export function ImagesPage() {
  const s = useSession();
  const qc = useQueryClient();
  const clusters = useQuery({ queryKey: ["clusters"], queryFn: () => api<{ clusters: Cluster[] }>("/v1/clusters").then((r) => r.clusters) });
  const [sel, setSel] = useState<string>("");
  const cluster = clusters.data?.find((c) => c.id === sel) ?? clusters.data?.[0];
  const images = useQuery({ queryKey: ["images"], queryFn: () => api<{ images: Image[] }>("/v1/images").then((r) => r.images) });
  const templates = useQuery({
    queryKey: ["templates", cluster?.id],
    queryFn: () => api<{ templates: PVETemplate[] }>(`/v1/clusters/${cluster!.id}/templates`).then((r) => r.templates),
    enabled: !!cluster,
  });
  const [publishing, setPublishing] = useState<PVETemplate | null>(null);
  const remove = useMutation({
    mutationFn: (id: string) => api(`/v1/images/${id}`, { method: "DELETE" }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["images"] }); qc.invalidateQueries({ queryKey: ["templates"] }); },
  });

  const imgs = (images.data ?? []).filter((i) => !cluster || i.location_id === cluster.id);
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Imagens</h1>
          <p>Templates do Proxmox publicados para criação de VMs. Quem cria vê o nome da imagem, nunca o template.</p>
        </div>
      </div>
      <div className="toolbar">
        <button className="btn btn-icon" aria-label="Atualizar" title="Atualizar" onClick={() => qc.invalidateQueries({ queryKey: ["templates"] })}><IconRefresh /></button>
        {(clusters.data?.length ?? 0) > 1 && (
          <label className="inline-field">
            Cluster
            <select className="select" value={cluster?.id ?? ""} onChange={(e) => setSel(e.target.value)}>
              {clusters.data?.map((c) => <option key={c.id} value={c.id}>{c.location} ({c.name})</option>)}
            </select>
          </label>
        )}
      </div>
      {clusters.data?.length === 0 && <div className="panel empty"><strong>Nenhum cluster cadastrado</strong>Cadastre um cluster para ver os templates dele.</div>}
      {cluster && (
        <div className="two-col">
          <section className="panel">
            <div className="panel-head"><h2>Publicadas em {cluster.location}</h2></div>
            {imgs.length === 0 ? (
              <p className="muted small">Nenhuma imagem publicada aqui. Publique um template ao lado.</p>
            ) : (
              <table className="table">
                <thead><tr><th>Imagem</th><th>Sistema</th><th className="num">Disco mín.</th><th className="actions" /></tr></thead>
                <tbody>
                  {imgs.map((i) => (
                    <tr key={i.id}>
                      <td>
                        <b>{i.name}</b>
                        <div className="kind">template {i.template_vmid}{!i.available && <span className="state-fail">O template sumiu do cluster</span>}</div>
                      </td>
                      <td>{i.os_family === "windows" ? "Windows" : "Linux"}</td>
                      <td className="num">{i.min_disk_gb} GB</td>
                      <td className="actions">
                        {s.canManageClusters && (
                          <button className="btn btn-sm btn-danger" disabled={remove.isPending} onClick={() => remove.mutate(i.id)}>Despublicar</button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {remove.error && <div className="alert" style={{ marginTop: 10 }}>{remove.error.message}</div>}
          </section>
          <section className="panel">
            <div className="panel-head"><h2>Templates no cluster</h2><span className="muted small">sincronizado {when(cluster.last_sync_at)}</span></div>
            {templates.data?.length === 0 && (
              <p className="muted small">
                Nenhum template encontrado. No Proxmox, prepare uma VM com cloud-init e converta em template
                (veja o guia de instalação). Ela aparece aqui na próxima sincronização.
              </p>
            )}
            {(templates.data?.length ?? 0) > 0 && (
              <table className="table">
                <thead><tr><th>Template</th><th className="num">Disco</th><th className="actions" /></tr></thead>
                <tbody>
                  {templates.data!.map((t) => (
                    <tr key={t.vmid}>
                      <td><b>{t.name || `vm ${t.vmid}`}</b><div className="kind">{t.vmid} · {t.node}</div></td>
                      <td className="num">{bytes(t.max_disk_bytes)}</td>
                      <td className="actions">
                        {t.image_id ? (
                          <span className="pill pill-ok">{t.image_name}</span>
                        ) : s.canManageClusters ? (
                          <button className="btn btn-sm" onClick={() => setPublishing(t)}>Publicar</button>
                        ) : null}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </section>
        </div>
      )}
      {publishing && cluster && <PublishModal cluster={cluster} t={publishing} onClose={() => setPublishing(null)} />}
    </>
  );
}

function PublishModal({ cluster, t, onClose }: { cluster: Cluster; t: PVETemplate; onClose: () => void }) {
  const qc = useQueryClient();
  const baseGB = Math.max(1, Math.ceil(t.max_disk_bytes / 1024 ** 3));
  const guess = t.name.replace(/^tpl[-_]?/i, "").replace(/[-_]/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
  const [name, setName] = useState(guess || t.name);
  const [os, setOs] = useState<"linux" | "windows">(/win/i.test(t.name) ? "windows" : "linux");
  const [minDisk, setMinDisk] = useState(String(Math.max(10, baseGB)));
  const save = useMutation({
    mutationFn: () => api("/v1/images", { method: "POST", json: { location_id: cluster.id, template_vmid: t.vmid, name: name.trim(), os_family: os, min_disk_gb: Number(minDisk) } }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["images"] }); qc.invalidateQueries({ queryKey: ["templates"] }); onClose(); },
  });
  return (
    <Modal title={`Publicar ${t.name || t.vmid}`} onClose={onClose}
      footer={<>
        <button className="btn" onClick={onClose}>Cancelar</button>
        <button className="btn btn-primary" disabled={!name.trim() || save.isPending} onClick={() => save.mutate()}>Publicar imagem</button>
      </>}>
      <div className="stack" style={{ gap: 12 }}>
        <label className="field">Nome da imagem<input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="Debian 12" /></label>
        <label className="field">
          Sistema
          <select className="select" value={os} onChange={(e) => setOs(e.target.value as "linux" | "windows")}>
            <option value="linux">Linux (cloud-init: usuário, senha, chave SSH e IP)</option>
            <option value="windows">Windows (só CPU, memória e disco)</option>
          </select>
        </label>
        <label className="field">
          Disco mínimo
          <span className="input-suffix"><input className="input" type="number" min={baseGB} value={minDisk} onChange={(e) => setMinDisk(e.target.value)} /><span>GB</span></span>
          <span className="hint">O disco do template tem {baseGB} GB; a VM nova é ampliada para o tamanho pedido.</span>
        </label>
        {save.error && <div className="alert">{save.error.message}</div>}
      </div>
    </Modal>
  );
}
