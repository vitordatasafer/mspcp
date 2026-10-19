package api

// Console no navegador. O portal pede um ticket (POST), recebe a senha VNC
// de uso único e abre um websocket para /v1/console/{ticket}; a API abre o
// websocket do PVE com o token do cluster e só repassa os bytes. O
// navegador nunca fala com o Proxmox nem vê node, vmid ou endereço.

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mspclouds/mspcp/internal/localauth"
	"github.com/mspclouds/mspcp/internal/proxmox"
	"github.com/mspclouds/mspcp/internal/rbac"
	"github.com/mspclouds/mspcp/internal/store"
)

const (
	consoleTicketTTL = 30 * time.Second
	consoleMaxLife   = 8 * time.Hour
)

type consoleTicket struct {
	subject    string
	resourceID uuid.UUID
	cluster    store.Cluster
	node, kind string
	vmid       int
	vnc        proxmox.VNCTicket
	expires    time.Time
}

type consoleTickets struct {
	mu sync.Mutex
	m  map[string]consoleTicket
}

func (c *consoleTickets) put(t consoleTicket) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]consoleTicket{}
	}
	now := time.Now()
	for k, v := range c.m {
		if now.After(v.expires) {
			delete(c.m, k)
		}
	}
	id := localauth.NewToken()
	c.m[id] = t
	return id
}

// take consome o ticket: cada um abre uma conexão só.
func (c *consoleTickets) take(id string) (consoleTicket, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[id]
	delete(c.m, id)
	if !ok || time.Now().After(t.expires) {
		return consoleTicket{}, false
	}
	return t, true
}

func (s *Server) openConsole(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
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
	if _, err := st.GetResource(r.Context(), p.Scope(rbac.ResourceConsole), id); isNotFound(err) {
		writeErr(w, http.StatusForbidden, "sem permissão para abrir o console")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	if res.Status != "running" {
		writeErr(w, http.StatusConflict, "ligue o servidor para abrir o console")
		return
	}
	if s.Clients == nil {
		writeErr(w, http.StatusServiceUnavailable, "console indisponível neste servidor")
		return
	}
	loc, err := st.Location(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	cluster, err := st.GetCluster(r.Context(), loc.ClusterID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	cl, err := s.Clients.For(r.Context(), cluster)
	if err != nil {
		s.Log.Error("console: cliente do cluster", "err", err, "cluster", cluster.Name)
		writeErr(w, http.StatusBadGateway, "não foi possível falar com a localização deste servidor")
		return
	}
	vnc, err := cl.VNCProxy(r.Context(), loc.Node, loc.Kind, loc.VMID)
	if err != nil {
		s.Log.Error("console: vncproxy", "err", err, "cluster", cluster.Name)
		writeErr(w, http.StatusBadGateway, "o cluster recusou abrir o console")
		return
	}
	ticket := s.consoles.put(consoleTicket{
		subject: p.Subject, resourceID: id, cluster: cluster, node: loc.Node, kind: loc.Kind, vmid: loc.VMID,
		vnc: vnc, expires: time.Now().Add(consoleTicketTTL),
	})
	if err := s.audit(r, st, "resource.console", id.String(), res.TenantID, nil); err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": "/v1/console/" + ticket, "password": vnc.Ticket, "expires_in": int(consoleTicketTTL.Seconds()),
	})
}

func (s *Server) consoleSocket(w http.ResponseWriter, r *http.Request) {
	t, ok := s.consoles.take(chi.URLParam(r, "ticket"))
	if !ok || t.subject != principal(r).Subject {
		writeErr(w, http.StatusNotFound, "ticket de console inválido ou vencido")
		return
	}
	cl, err := s.Clients.For(r.Context(), t.cluster)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "localização indisponível")
		return
	}
	dialCtx, cancelDial := context.WithTimeout(r.Context(), 15*time.Second)
	upstream, err := cl.DialVNC(dialCtx, t.node, t.kind, t.vmid, t.vnc)
	cancelDial()
	if err != nil {
		s.Log.Error("console: websocket do PVE", "err", err)
		writeErr(w, http.StatusBadGateway, "não foi possível conectar ao console")
		return
	}
	defer upstream.CloseNow()

	// Conexão longa: tira os prazos do http.Server.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	// Accept confere que o Origin é o próprio host (nada de outro site
	// abrindo o console com o cookie do usuário).
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
	if err != nil {
		return
	}
	defer client.CloseNow()
	client.SetReadLimit(1 << 20)

	ctx, cancel := context.WithTimeout(r.Context(), consoleMaxLife)
	defer cancel()
	go func() {
		defer cancel()
		pipeWS(ctx, client, upstream)
	}()
	pipeWS(ctx, upstream, client)
	_ = client.Close(websocket.StatusNormalClosure, "")
}

func pipeWS(ctx context.Context, dst, src *websocket.Conn) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return
		}
	}
}
