// Ícones de traço, desenhados aqui para não depender de biblioteca.
type P = { title?: string };

function Svg({ children, title }: { children: React.ReactNode; title?: string }) {
  return (
    <svg className="ic" viewBox="0 0 24 24" aria-hidden={title ? undefined : true} role={title ? "img" : undefined}>
      {title && <title>{title}</title>}
      {children}
    </svg>
  );
}

export const IconHome = (p: P) => <Svg {...p}><path d="M3 11l9-7 9 7" /><path d="M5 10v10h14V10" /><path d="M10 20v-6h4v6" /></Svg>;
export const IconServer = (p: P) => <Svg {...p}><rect x="3" y="4" width="18" height="7" rx="1.5" /><rect x="3" y="13" width="18" height="7" rx="1.5" /><path d="M7 7.5h.01M7 16.5h.01" /></Svg>;
export const IconMonitor = (p: P) => <Svg {...p}><rect x="3" y="4" width="18" height="12" rx="1.5" /><path d="M8 20h8M12 16v4" /></Svg>;
export const IconLayers = (p: P) => <Svg {...p}><path d="M12 3l9 5-9 5-9-5 9-5z" /><path d="M3 13l9 5 9-5" /></Svg>;
export const IconUsers = (p: P) => <Svg {...p}><circle cx="9" cy="8" r="3.2" /><path d="M3 20c0-3.3 2.7-6 6-6s6 2.7 6 6" /><path d="M16 4.5a3 3 0 010 6M21 20c0-2.6-1.6-4.8-4-5.6" /></Svg>;
export const IconCluster = (p: P) => <Svg {...p}><rect x="3" y="3" width="7" height="7" rx="1" /><rect x="14" y="3" width="7" height="7" rx="1" /><rect x="3" y="14" width="7" height="7" rx="1" /><rect x="14" y="14" width="7" height="7" rx="1" /></Svg>;
export const IconDisk = (p: P) => <Svg {...p}><ellipse cx="12" cy="6" rx="8" ry="3" /><path d="M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6" /><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3" /></Svg>;
export const IconTasks = (p: P) => <Svg {...p}><path d="M9 6h11M9 12h11M9 18h11" /><path d="M4 6l1 1 2-2M4 12l1 1 2-2M4 18l1 1 2-2" /></Svg>;
export const IconSearch = (p: P) => <Svg {...p}><circle cx="11" cy="11" r="6.5" /><path d="M20 20l-4-4" /></Svg>;
export const IconRefresh = (p: P) => <Svg {...p}><path d="M20 11a8 8 0 10-2.3 5.7" /><path d="M20 4v7h-7" /></Svg>;
export const IconChevron = (p: P) => <Svg {...p}><path d="M6 9l6 6 6-6" /></Svg>;
export const IconChevronRight = (p: P) => <Svg {...p}><path d="M9 6l6 6-6 6" /></Svg>;
export const IconCollapse = (p: P) => <Svg {...p}><path d="M13 6l6 6-6 6M5 6l6 6-6 6" /></Svg>;
export const IconExpand = (p: P) => <Svg {...p}><path d="M11 6l-6 6 6 6M19 6l-6 6 6 6" /></Svg>;
export const IconPower = (p: P) => <Svg {...p}><path d="M12 3v8" /><path d="M6.3 7.5a8 8 0 1011.4 0" /></Svg>;
export const IconStop = (p: P) => <Svg {...p}><rect x="6" y="6" width="12" height="12" rx="1.5" /></Svg>;
export const IconRestart = (p: P) => <Svg {...p}><path d="M4 12a8 8 0 1 0 2.3-5.7" /><path d="M4 4v5h5" /></Svg>;
export const IconPlus = (p: P) => <Svg {...p}><path d="M12 5v14M5 12h14" /></Svg>;
export const IconClock = (p: P) => <Svg {...p}><circle cx="12" cy="12" r="8.5" /><path d="M12 7.5V12l3 2" /></Svg>;
export const IconLogout = (p: P) => <Svg {...p}><path d="M15 4h4v16h-4" /><path d="M10 8l-4 4 4 4M6 12h10" /></Svg>;
export const IconLoader = (p: P) => <Svg {...p}><path d="M12 3a9 9 0 109 9" /></Svg>;
export const IconTrash = (p: P) => <Svg {...p}><path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13" /><path d="M10 11v6M14 11v6" /></Svg>;
export const IconMove = (p: P) => <Svg {...p}><path d="M4 12h13M13 7l5 5-5 5" /><path d="M4 5v14" /></Svg>;
export const IconImage = (p: P) => <Svg {...p}><rect x="3" y="4" width="18" height="16" rx="1.5" /><circle cx="9" cy="10" r="2" /><path d="M21 16l-5-5-8 9" /></Svg>;
export const IconUser = (p: P) => <Svg {...p}><circle cx="12" cy="8" r="4" /><path d="M4 21c0-4.4 3.6-8 8-8s8 3.6 8 8" /></Svg>;
export const IconKey = (p: P) => <Svg {...p}><circle cx="8" cy="15" r="4" /><path d="M11 12l9-9M17 6l3 3M15 8l2 2" /></Svg>;
