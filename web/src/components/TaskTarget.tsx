import { Link } from "react-router-dom";
import type { TaskView } from "../api";

// Nome do servidor de uma tarefa: com link enquanto ele existir.
export function TaskTarget({ t }: { t: TaskView }) {
  const gone = !t.resource_id || (t.action === "delete" && t.status === "succeeded");
  return gone ? <span>{t.resource_alias}</span> : <Link to={`/servidores/${t.resource_id}`}>{t.resource_alias}</Link>;
}
