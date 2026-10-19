import { createContext, useContext } from "react";
import type { Me, Tenant } from "./api";

// O que a interface pode oferecer ao usuário. A API continua sendo quem
// decide: esconder um botão é conveniência, não segurança.
export interface Session {
  me: Me;
  name: string;
  tenants: Tenant[]; // tenants visíveis (vazio para quem não administra)
  home?: Tenant; // tenant mais alto na árvore em que a pessoa tem vínculo
  isStaff: boolean;
  canPower: boolean;
  canCreateVM: boolean; // criar e excluir VMs nos VDCs
  canConsole: boolean;
  canAssign: boolean; // mover VMs existentes entre VDCs (admin)
  canAdmin: boolean; // gerenciar clientes e usuários
  canCreateTenants: boolean;
  canManageVDCs: boolean; // criar e ajustar VDCs (MSP e admin)
  canSetLimits: boolean; // definir a cota dos MSPs (admin)
  canClusters: boolean;
  canManageClusters: boolean;
  multiTenant: boolean; // enxerga mais de um cliente
}

const roles = (me: Me) => new Set(me.memberships.map((m) => m.role));

export function buildSession(me: Me, name: string, tenants: Tenant[]): Session {
  const r = roles(me);
  const isStaff = me.memberships.some((m) => m.tenant_kind === "provider");
  const homeId = me.memberships.find((m) => m.tenant_kind === "provider")?.tenant_id
    ?? me.memberships.find((m) => m.tenant_kind === "partner")?.tenant_id
    ?? me.memberships[0]?.tenant_id;
  return {
    me,
    name,
    tenants,
    home: tenants.find((t) => t.id === homeId),
    isStaff,
    canPower: [...r].some((x) => x !== "viewer"),
    canCreateVM: r.has("super_admin") || r.has("partner_admin") || r.has("customer_admin"),
    canConsole: [...r].some((x) => x !== "viewer"),
    canAssign: r.has("super_admin"),
    canAdmin: r.has("super_admin") || r.has("partner_admin") || r.has("customer_admin"),
    canCreateTenants: r.has("super_admin") || r.has("partner_admin"),
    canManageVDCs: r.has("super_admin") || r.has("partner_admin"),
    canSetLimits: r.has("super_admin"),
    canClusters: r.has("super_admin") || r.has("noc_operator"),
    canManageClusters: r.has("super_admin"),
    multiTenant: isStaff || me.memberships.some((m) => m.tenant_kind === "partner"),
  };
}

export const SessionContext = createContext<Session | null>(null);

export function useSession(): Session {
  const s = useContext(SessionContext);
  if (!s) throw new Error("useSession fora do SessionContext");
  return s;
}
