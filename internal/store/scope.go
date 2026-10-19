package store

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// sqlFilter devolve um predicado SQL para o escopo e seus argumentos.
// pathCol é o path do tenant dono da linha (pode ser NULL via LEFT JOIN),
// tenantCol o tenant_id da linha e projectCol o project_id da linha.
// Os placeholders começam em $argStart.
func (s Scope) sqlFilter(pathCol, tenantCol, projectCol string, argStart int) (string, []any) {
	if len(s) == 0 {
		return "false", nil
	}
	var parts []string
	var args []any
	n := argStart
	for _, g := range s {
		p := fmt.Sprintf("(starts_with(%s, $%d)", pathCol, n)
		args = append(args, g.TenantPath)
		n++
		if g.ProjectID != nil {
			p += fmt.Sprintf(" AND %s = $%d", projectCol, n)
			args = append(args, *g.ProjectID)
			n++
		}
		p += ")"
		if g.SeesUnassigned {
			p = "(" + p + " OR " + tenantCol + " IS NULL)"
		}
		parts = append(parts, p)
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// Contains diz se o escopo cobre um tenant (path) e projeto.
func (s Scope) Contains(tenantPath string, projectID *uuid.UUID) bool {
	for _, g := range s {
		if tenantPath == "" {
			if g.SeesUnassigned {
				return true
			}
			continue
		}
		if !strings.HasPrefix(tenantPath, g.TenantPath) {
			continue
		}
		if g.ProjectID != nil && (projectID == nil || *projectID != *g.ProjectID) {
			continue
		}
		return true
	}
	return false
}
