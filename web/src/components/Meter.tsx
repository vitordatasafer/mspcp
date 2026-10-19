// Medidor de capacidade. A barra inteira é o limite; a parte clara é o que
// já foi distribuído em VDCs e a parte escura o que está em uso por VMs
// ligadas. Sem limite, a barra fica tracejada e só o texto informa.
export function Meter({
  label,
  limit,
  allocated,
  used,
  fmt = (n) => n.toLocaleString("pt-BR"),
  allocatedWord = "distribuídos",
}: {
  label: string;
  limit: number | null;
  allocated?: number;
  used?: number;
  fmt?: (n: number) => string;
  allocatedWord?: string;
}) {
  const filled = allocated ?? used ?? 0;
  const pct = (n: number) => (limit && limit > 0 ? Math.min(100, (n / limit) * 100) : 0);
  const full = limit !== null && filled >= limit && limit > 0;
  const free = limit === null ? null : Math.max(0, limit - filled);

  let note: string;
  if (allocated !== undefined) {
    note = limit === null
      ? `${fmt(allocated)} ${allocatedWord}, sem limite`
      : `${fmt(free!)} livres`;
    if (used !== undefined) note += `, ${fmt(used)} em uso`;
  } else {
    note = limit === null ? "sem limite" : `${fmt(free!)} livres`;
  }

  return (
    <div className="meter">
      <div className="meter-top">
        <strong>{label}</strong>
        <span className="num">
          {limit === null ? fmt(filled) : `${fmt(filled)} de ${fmt(limit)}`}
        </span>
      </div>
      <div
        className={`meter-bar${full ? " full" : ""}${limit === null ? " unlimited" : ""}`}
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={limit ?? undefined}
        aria-valuenow={filled}
      >
        {allocated !== undefined && <span className="alloc" style={{ width: `${pct(allocated)}%` }} />}
        {used !== undefined && <span className="used" style={{ width: `${pct(used)}%` }} />}
      </div>
      <span className="meter-note">{note}</span>
    </div>
  );
}
