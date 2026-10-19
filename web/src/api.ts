import { getConfig, login, oidcUser } from "./auth";

export type TenantKind = "provider" | "partner" | "customer";

export interface Resource {
  id: string;
  alias: string;
  kind: "qemu" | "lxc";
  tenant_id?: string;
  tenant_name?: string;
  project_id?: string;
  project_name?: string;
  status: string;
  cpus: number;
  max_mem_bytes: number;
  max_disk_bytes: number;
  deleting?: boolean;
  location: string;
  last_seen_at: string;
}

export type TaskStatus = "queued" | "running" | "succeeded" | "failed";
export type Action = "start" | "shutdown" | "stop" | "reboot";
export type TaskAction = Action | "create" | "delete" | "assign";

export interface Task {
  id: string;
  resource_id?: string;
  project_id?: string;
  label?: string;
  action: TaskAction;
  status: TaskStatus;
  requested_by: string;
  error?: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
}

export interface TaskView extends Task {
  resource_alias: string;
  tenant_name?: string;
}

export interface Tenant {
  id: string;
  parent_id?: string;
  kind: TenantKind;
  name: string;
  status: string;
  created_at: string;
}

// VDC (na API ainda chamado de project): fatia de recursos num único cluster,
// que MSP e cliente só enxergam como localização.
export interface VDCStorage {
  class_id: string;
  class_name: string;
  capacity_gb: number;
  used_gb: number;
}

export interface Project {
  id: string;
  tenant_id: string;
  tenant_name?: string;
  name: string;
  location_id?: string;
  location: string;
  cpu_cores: number;
  memory_mb: number;
  storage: VDCStorage[];
  usage?: { running_cpu: number; running_memory_mb: number; vms: number; disk_gb: number };
  network?: { bridge: string | null; vlan_tag: number | null }; // só para a equipe
  created_at: string;
}

export interface Image {
  id: string;
  location_id: string;
  location: string;
  name: string;
  os_family: "linux" | "windows";
  min_disk_gb: number;
  enabled: boolean;
  available: boolean;
  template_vmid?: number; // só para a equipe
  created_at: string;
}

export interface PVETemplate {
  cluster_id: string;
  vmid: number;
  node: string;
  name: string;
  max_disk_bytes: number;
  last_seen_at: string;
  image_id?: string;
  image_name?: string;
}

export interface StorageClass {
  id: string;
  name: string;
  description?: string;
}

export interface StoragePolicy {
  id: string;
  cluster_id: string;
  class_id: string;
  class_name: string;
  pve_storage: string;
}

export interface Location {
  id: string;
  name: string;
  storage_classes: StorageClass[];
}

// Reserva de um MSP num cluster (vCPU, memória e storage por classe).
export interface Reservation {
  tenant_id: string;
  tenant_name?: string;
  location_id: string;
  location: string;
  cpu_cores: number;
  memory_mb: number;
  storage: Record<string, number>;
  allocated_cpu: number;
  allocated_memory_mb: number;
  allocated_storage: Record<string, number>;
  updated_at: string;
}

// Cota do MSP: vCPU, memória e storage são a soma das reservas por cluster;
// os limites de quantidade são globais.
export interface MSPLimits {
  cpu_cores: number | null;
  memory_mb: number | null;
  max_customers: number | null;
  max_vdcs: number | null;
  max_vms: number | null;
  locations: string[];
  storage: Record<string, number | null>;
  reservations: Reservation[];
}

export interface MSPUsage {
  allocated_cpu: number;
  allocated_memory_mb: number;
  allocated_storage: Record<string, number>;
  customers: number;
  vdcs: number;
  vms: number;
  running_cpu: number;
  running_memory_mb: number;
  locations_in_use: string[];
}

export interface Capacity {
  limits: MSPLimits;
  usage: MSPUsage;
  vdcs: Project[];
  locations: Location[];
}

export interface MembershipView {
  id: string;
  subject: string;
  project_id?: string;
  project_name?: string;
  role: string;
  created_at: string;
  local: boolean;
  name?: string;
  email?: string;
  totp_enabled: boolean;
  last_login_at?: string;
}

export interface Cluster {
  id: string;
  name: string;
  dc: string;
  location: string;
  api_url: string;
  token_id: string;
  default_bridge: string;
  cpu_ratio: number;
  pve_version: string;
  nodes_online: number;
  nodes_total: number;
  cpu_total: number;
  memory_total_mb: number;
  enabled: boolean;
  last_sync_at?: string;
  last_sync_error?: string;
  created_at: string;
  // Só na lista:
  reserved_cpu?: number;
  reserved_memory_mb?: number;
  msps?: number;
  stored_secret?: boolean;
}

export interface ClusterStorage {
  storage: string;
  type: string;
  shared: boolean;
  content: string;
  nodes: number;
  total_gb: number;
  used_gb: number;
}

export interface ClusterBridge {
  name: string;
  kind: "bridge" | "vnet";
  nodes: number;
}

export interface CapacityLine {
  class_id?: string;
  name: string;
  total: number;
  physical: number;
  reserved: number;
  pve_storage?: string;
}

export interface ClusterDetail {
  cluster: Cluster;
  stored_secret: boolean;
  pinned_ca: boolean;
  storages: ClusterStorage[];
  bridges: ClusterBridge[];
  capacity: CapacityLine[];
  storage_classes: StorageClass[];
  reservations: Reservation[];
  templates: number;
  msps: Tenant[];
}

export interface CertInfo {
  subject: string;
  issuer: string;
  not_after: string;
  fingerprint_sha256: string;
  is_ca: boolean;
}

export interface ProbeResult {
  ok: boolean;
  error?: string;
  tls?: { chain: CertInfo[]; trusted_by_system: boolean; suggested_pem: string; pinned_subject: string };
  ca_cert_pem: string;
  version?: string;
  nodes_online: number;
  nodes_total: number;
  cpu_total: number;
  memory_total_mb: number;
  storages: ClusterStorage[];
  bridges: ClusterBridge[];
  vms: number;
  templates: number;
  missing_privileges: string[];
}

export interface Me {
  subject: string;
  name?: string;
  email?: string;
  local: boolean;
  must_change_password: boolean;
  totp_enabled: boolean;
  memberships: { tenant_id: string; tenant_kind: TenantKind; project_id?: string; role: string }[];
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public code?: string,
  ) {
    // A API devolve frases em minúscula; a interface mostra como frase,
    // sem estragar siglas como "vCPU".
    const keep = message.length > 1 && message[1] === message[1].toUpperCase() && /[a-z]/i.test(message[1]);
    super(keep ? message : message.charAt(0).toUpperCase() + message.slice(1));
  }
}

// api chama a API v1. No login local a sessão vai no cookie e toda
// requisição leva o cabeçalho X-MSPCP (proteção contra CSRF); no OIDC vai o
// token Bearer.
export async function api<T>(path: string, init: RequestInit & { json?: unknown } = {}): Promise<T> {
  const cfg = await getConfig();
  const headers = new Headers(init.headers);
  headers.set("X-MSPCP", "1");
  if (cfg.auth === "oidc") {
    const user = await oidcUser();
    if (!user) {
      await login();
      throw new ApiError(401, "Sessão expirada.");
    }
    headers.set("Authorization", `Bearer ${user.access_token}`);
  }
  let body = init.body;
  if (init.json !== undefined) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(init.json);
  }
  const res = await fetch(path, { ...init, headers, body, credentials: "same-origin" });
  const text = await res.text();
  let data: { error?: string; code?: string } | undefined;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    data = undefined;
  }
  if (res.status === 401) {
    await login();
    throw new ApiError(401, data?.error || "Sessão expirada.", data?.code);
  }
  if (!res.ok) {
    throw new ApiError(res.status, data?.error || `Erro ${res.status}`, data?.code);
  }
  return data as T;
}

// ---------- formatação ----------

export function bytes(n: number): string {
  const gb = n / 1024 ** 3;
  if (gb >= 1024) return `${(gb / 1024).toLocaleString("pt-BR", { maximumFractionDigits: 1 })} TB`;
  return `${gb.toLocaleString("pt-BR", { maximumFractionDigits: gb < 10 ? 1 : 0 })} GB`;
}

export function gbText(gb: number): string {
  if (gb >= 1024) return `${(gb / 1024).toLocaleString("pt-BR", { maximumFractionDigits: 1 })} TB`;
  return `${gb.toLocaleString("pt-BR")} GB`;
}

// Busca sem acento e sem caixa: "clinica" encontra "Clínica".
export function fold(s: string | undefined | null): string {
  return (s ?? "").normalize("NFD").replace(/[\u0300-\u036f]/g, "").toLowerCase();
}

export function dateTime(iso?: string): string {
  if (!iso) return "—";
  return new Date(iso).toLocaleString("pt-BR", { dateStyle: "short", timeStyle: "medium" });
}

export function duration(a?: string, b?: string): string {
  if (!a || !b) return "—";
  const s = Math.max(0, Math.round((new Date(b).getTime() - new Date(a).getTime()) / 1000));
  return s < 60 ? `${s} s` : `${Math.floor(s / 60)} min ${s % 60} s`;
}

export function initials(name: string): string {
  const p = name.trim().split(/\s+/);
  return ((p[0]?.[0] ?? "") + (p.length > 1 ? p[p.length - 1][0] : "")).toUpperCase() || "?";
}

export function when(iso?: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  const s = Math.round((Date.now() - d.getTime()) / 1000);
  if (s < 60) return "agora há pouco";
  if (s < 3600) return `há ${Math.round(s / 60)} min`;
  if (s < 86400) return `há ${Math.round(s / 3600)} h`;
  return d.toLocaleString("pt-BR", { dateStyle: "short", timeStyle: "short" });
}

export const actionLabel: Record<TaskAction, { verb: string; doing: string; done: string }> = {
  start: { verb: "Ligar", doing: "Ligando", done: "Ligado" },
  shutdown: { verb: "Desligar", doing: "Desligando", done: "Desligado" },
  stop: { verb: "Forçar desligamento", doing: "Forçando desligamento", done: "Desligado à força" },
  reboot: { verb: "Reiniciar", doing: "Reiniciando", done: "Reiniciado" },
  create: { verb: "Criar", doing: "Criando", done: "Criado" },
  delete: { verb: "Excluir", doing: "Excluindo", done: "Excluído" },
  assign: { verb: "Atribuir a VDC", doing: "Movendo", done: "Movido" },
};

export const roleLabel: Record<string, string> = {
  super_admin: "Administrador da plataforma",
  noc_operator: "Operador NOC",
  partner_admin: "Administrador do MSP",
  customer_admin: "Administrador do cliente",
  operator: "Operador",
  viewer: "Somente leitura",
};

export const kindLabel: Record<TenantKind, string> = {
  provider: "Provedor",
  partner: "MSP",
  customer: "Cliente",
};
