// Package quota aplica as regras de recursos (modelo de alocação):
//
//   - No cluster, o admin reserva recursos para cada MSP (vCPU, RAM e
//     storage por classe). A soma das reservas não passa a capacidade do
//     cluster (vCPU pode ter uma razão de overcommit definida no cluster).
//   - O MSP tem também limites globais de quantidade (clientes, VDCs, VMs).
//   - O MSP divide cada reserva em VDCs, para uso próprio ou de clientes.
//     A soma dos VDCs num cluster nunca passa a reserva ali (sem overcommit).
//   - Ligar uma VM é recusado se o VDC passar da cota de vCPU ou RAM.
//   - Nenhuma cota pode ser reduzida abaixo do que já está distribuído/em uso.
//
// Toda função roda dentro da transação do chamador e trava a linha do MSP
// (ou do VDC) para que pedidos simultâneos não estourem a cota juntos.
package quota

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/vitordatasafer/mspvdc/internal/store"
)

// Error é uma violação de regra de negócio, com mensagem para o usuário.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

func IsQuotaError(err error) bool {
	var q *Error
	return errors.As(err, &q)
}

// MSPOf devolve o MSP responsável por um tenant (ele mesmo, se for MSP).
func MSPOf(ctx context.Context, st *store.Store, t store.Tenant) (store.Tenant, error) {
	switch t.Kind {
	case store.KindPartner:
		return t, nil
	case store.KindCustomer:
		if t.ParentID == nil {
			return store.Tenant{}, fail("cliente sem MSP")
		}
		return st.GetTenant(ctx, *t.ParentID)
	}
	return store.Tenant{}, fail("o provedor não recebe recursos; crie um MSP")
}

func gb(n int64) string {
	if n >= 1024 && n%1024 == 0 {
		return fmt.Sprintf("%d TB", n/1024)
	}
	return fmt.Sprintf("%d GB", n)
}

// ---------- VDC ----------

// CheckVDC valida um VDC novo (current == nil) ou a nova cota de um VDC
// existente contra a reserva do MSP no cluster do VDC.
func CheckVDC(ctx context.Context, st *store.Store, tenant store.Tenant, spec store.VDCSpec, current *store.Project) error {
	if spec.CPUCores <= 0 || spec.MemoryMB <= 0 {
		return fail("informe vCPU e memória maiores que zero")
	}
	msp, err := MSPOf(ctx, st, tenant)
	if err != nil {
		return err
	}
	limits, err := st.GetMSPLimits(ctx, msp.ID, true) // trava o MSP
	if err != nil {
		return err
	}
	clusterID := spec.ClusterID
	if current != nil && current.ClusterID != nil {
		clusterID = *current.ClusterID // VDC não muda de cluster
	}
	res, ok, err := st.GetReservation(ctx, msp.ID, clusterID)
	if err != nil {
		return err
	}
	if !ok {
		return fail("o MSP %s não tem recursos reservados nesta localização", msp.Name)
	}
	inCluster, err := st.ClassesInCluster(ctx, clusterID)
	if err != nil {
		return err
	}
	classes, err := st.ListStorageClasses(ctx)
	if err != nil {
		return err
	}
	className := map[uuid.UUID]string{}
	for _, c := range classes {
		className[c.ID] = c.Name
	}
	for class, size := range spec.Storage {
		name, ok := className[class]
		if !ok {
			return fail("classe de storage desconhecida")
		}
		if size < 0 {
			return fail("capacidade de %s não pode ser negativa", name)
		}
		if !inCluster[class] {
			return fail("%s não está disponível nesta localização", name)
		}
		if _, ok := res.Storage[class]; !ok && size > 0 {
			return fail("o MSP %s não tem %s reservado em %s", msp.Name, name, res.Location)
		}
	}

	if current == nil && limits.MaxVDCs != nil {
		used, err := st.MSPUsage(ctx, msp.Path, nil)
		if err != nil {
			return err
		}
		if used.VDCs+1 > *limits.MaxVDCs {
			return fail("o MSP %s já atingiu o limite de %d VDCs", msp.Name, *limits.MaxVDCs)
		}
	}
	// Distribuído no cluster, sem contar o próprio VDC (ao ajustar).
	allocCPU, allocMem := res.AllocatedCPU, res.AllocatedMemoryMB
	allocSt := map[uuid.UUID]int64{}
	for c, v := range res.AllocatedStorage {
		allocSt[c] = v
	}
	if current != nil {
		allocCPU -= current.CPUCores
		allocMem -= current.MemoryMB
		for _, s := range current.Storage {
			allocSt[s.ClassID] -= s.CapacityGB
		}
	}
	if allocCPU+spec.CPUCores > res.CPUCores {
		return fail("vCPU insuficiente na reserva do MSP em %s: %d de %d já distribuídos, pedido de %d",
			res.Location, allocCPU, res.CPUCores, spec.CPUCores)
	}
	if allocMem+spec.MemoryMB > res.MemoryMB {
		return fail("memória insuficiente na reserva do MSP em %s: %s de %s já distribuídos, pedido de %s",
			res.Location, gb(allocMem/1024), gb(res.MemoryMB/1024), gb(spec.MemoryMB/1024))
	}
	for class, size := range spec.Storage {
		limit := res.Storage[class]
		if allocSt[class]+size > limit {
			return fail("%s insuficiente na reserva do MSP em %s: %s de %s já distribuídos, pedido de %s",
				className[class], res.Location, gb(allocSt[class]), gb(limit), gb(size))
		}
	}

	// Reduzir a cota não pode deixar VMs ligadas ou discos acima dela.
	if current != nil && current.Usage != nil {
		if spec.CPUCores < current.Usage.RunningCPU {
			return fail("o VDC tem %d vCPU em VMs ligadas; a cota não pode ficar abaixo disso", current.Usage.RunningCPU)
		}
		if spec.MemoryMB < current.Usage.RunningMemoryMB {
			return fail("o VDC tem %s de memória em VMs ligadas; a cota não pode ficar abaixo disso", gb(current.Usage.RunningMemoryMB/1024))
		}
		for _, s := range current.Storage {
			if spec.Storage[s.ClassID] < s.UsedGB {
				return fail("o VDC tem %s em discos de %s; a cota não pode ficar abaixo disso", gb(s.UsedGB), s.ClassName)
			}
		}
	}
	return nil
}

// ---------- clientes ----------

// CheckNewCustomer aplica o limite de quantidade de clientes do MSP.
func CheckNewCustomer(ctx context.Context, st *store.Store, msp store.Tenant) error {
	limits, err := st.GetMSPLimits(ctx, msp.ID, true)
	if err != nil {
		return err
	}
	if limits.MaxCustomers == nil {
		return nil
	}
	used, err := st.MSPUsage(ctx, msp.Path, nil)
	if err != nil {
		return err
	}
	if used.Customers+1 > *limits.MaxCustomers {
		return fail("o MSP %s já atingiu o limite de %d clientes", msp.Name, *limits.MaxCustomers)
	}
	return nil
}

// ---------- limites do MSP ----------

// SetLimits troca os limites de quantidade do MSP, recusando valores abaixo
// do que já existe.
func SetLimits(ctx context.Context, st *store.Store, msp store.Tenant, next store.MSPLimits) error {
	if msp.Kind != store.KindPartner {
		return fail("limites só se aplicam a MSPs")
	}
	if _, err := st.GetMSPLimits(ctx, msp.ID, true); err != nil {
		return err
	}
	used, err := st.MSPUsage(ctx, msp.Path, nil)
	if err != nil {
		return err
	}
	var problems []string
	if next.MaxCustomers != nil && *next.MaxCustomers < used.Customers {
		problems = append(problems, fmt.Sprintf("clientes: já existem %d", used.Customers))
	}
	if next.MaxVDCs != nil && *next.MaxVDCs < used.VDCs {
		problems = append(problems, fmt.Sprintf("VDCs: já existem %d", used.VDCs))
	}
	if next.MaxVMs != nil && *next.MaxVMs < used.VMs {
		problems = append(problems, fmt.Sprintf("servidores: já existem %d", used.VMs))
	}
	if len(problems) > 0 {
		return fail("os limites não podem ficar abaixo do uso atual (%s)", strings.Join(problems, "; "))
	}
	next.TenantID = msp.ID
	return st.SaveMSPCounts(ctx, next)
}

// ---------- reserva do MSP num cluster ----------

// SetReservation cria ou troca a reserva de um MSP num cluster. Não pode
// ficar abaixo do que o MSP já distribuiu em VDCs ali, e a soma das
// reservas do cluster não passa a capacidade dele.
func SetReservation(ctx context.Context, st *store.Store, msp store.Tenant, cluster store.Cluster, next store.Reservation) error {
	if msp.Kind != store.KindPartner {
		return fail("reservas são feitas para MSPs")
	}
	if next.CPUCores < 0 || next.MemoryMB < 0 {
		return fail("valores não podem ser negativos")
	}
	if err := st.LockCluster(ctx, cluster.ID); err != nil {
		return err
	}
	if _, err := st.GetMSPLimits(ctx, msp.ID, true); err != nil {
		return err
	}
	inCluster, err := st.ClassesInCluster(ctx, cluster.ID)
	if err != nil {
		return err
	}
	classes, err := st.ListStorageClasses(ctx)
	if err != nil {
		return err
	}
	className := map[uuid.UUID]string{}
	for _, c := range classes {
		className[c.ID] = c.Name
	}
	for class, size := range next.Storage {
		name, ok := className[class]
		if !ok {
			return fail("classe de storage desconhecida")
		}
		if size < 0 {
			return fail("capacidade de %s não pode ser negativa", name)
		}
		if !inCluster[class] {
			return fail("%s não tem storage mapeado neste cluster", name)
		}
	}

	var problems []string
	if cur, ok, err := st.GetReservation(ctx, msp.ID, cluster.ID); err != nil {
		return err
	} else if ok {
		if next.CPUCores < cur.AllocatedCPU {
			problems = append(problems, fmt.Sprintf("vCPU: %d já distribuídos em VDCs", cur.AllocatedCPU))
		}
		if next.MemoryMB < cur.AllocatedMemoryMB {
			problems = append(problems, fmt.Sprintf("memória: %s já distribuídos", gb(cur.AllocatedMemoryMB/1024)))
		}
		for class, alloc := range cur.AllocatedStorage {
			if alloc > 0 && next.Storage[class] < alloc {
				problems = append(problems, fmt.Sprintf("%s: %s já distribuídos", className[class], gb(alloc)))
			}
		}
	}
	if len(problems) > 0 {
		return fail("a reserva não pode ficar abaixo do que o MSP já distribuiu (%s)", strings.Join(problems, "; "))
	}

	// Capacidade do cluster (só quando o sincronizador já leu os nodes).
	otherCPU, otherMem, otherSt, err := st.ClusterReserved(ctx, cluster.ID, &msp.ID)
	if err != nil {
		return err
	}
	if cluster.CPUTotal > 0 {
		capCPU := int(float64(cluster.CPUTotal) * cluster.CPURatio)
		if otherCPU+next.CPUCores > capCPU {
			return fail("o cluster %s tem %d vCPU (%d threads × %.2g); outros MSPs já reservam %d, sobram %d",
				cluster.Name, capCPU, cluster.CPUTotal, cluster.CPURatio, otherCPU, max(0, capCPU-otherCPU))
		}
	}
	if cluster.MemoryTotalMB > 0 && otherMem+next.MemoryMB > cluster.MemoryTotalMB {
		return fail("o cluster %s tem %s de memória; outros MSPs já reservam %s, sobram %s",
			cluster.Name, gb(cluster.MemoryTotalMB/1024), gb(otherMem/1024), gb(max(0, cluster.MemoryTotalMB-otherMem)/1024))
	}
	capSt, err := st.ClassCapacityGB(ctx, cluster.ID)
	if err != nil {
		return err
	}
	for class, size := range next.Storage {
		if c := capSt[class]; c > 0 && otherSt[class]+size > c {
			return fail("%s no cluster %s tem %s; outros MSPs já reservam %s, sobram %s",
				className[class], cluster.Name, gb(c), gb(otherSt[class]), gb(max(0, c-otherSt[class])))
		}
	}
	next.TenantID, next.ClusterID = msp.ID, cluster.ID
	return st.SaveReservation(ctx, next)
}

// DeleteReservation tira a reserva, desde que o MSP não tenha VDC ali.
func DeleteReservation(ctx context.Context, st *store.Store, msp store.Tenant, clusterID uuid.UUID) error {
	if _, err := st.GetMSPLimits(ctx, msp.ID, true); err != nil {
		return err
	}
	used, err := st.MSPUsage(ctx, msp.Path, nil)
	if err != nil {
		return err
	}
	for _, c := range used.ClustersInUse {
		if c == clusterID {
			return fail("o MSP %s tem VDCs neste cluster; remova os VDCs antes", msp.Name)
		}
	}
	return st.DeleteReservation(ctx, msp.ID, clusterID)
}

// ---------- ligar VM ----------

// CheckPowerOn recusa ligar uma VM se o VDC passar da cota de vCPU ou RAM.
// Conta as VMs ligadas e os "start" ainda na fila, com o VDC travado.
func CheckPowerOn(ctx context.Context, st *store.Store, resourceID uuid.UUID) error {
	projectID, cpus, memMB, err := st.ResourceQuotaInfo(ctx, resourceID)
	if err != nil {
		return err
	}
	if projectID == nil {
		return nil // VM sem VDC: só a equipe enxerga
	}
	if err := st.LockProject(ctx, *projectID); err != nil {
		return err
	}
	p, err := st.GetProject(ctx, *projectID)
	if err != nil {
		return err
	}
	usedCPU, usedMem, err := st.ProjectCommitted(ctx, *projectID, resourceID)
	if err != nil {
		return err
	}
	if usedCPU+cpus > p.CPUCores {
		return fail("ligar esta VM passaria a cota de vCPU do VDC %s (%d em uso + %d > %d)", p.Name, usedCPU, cpus, p.CPUCores)
	}
	if usedMem+memMB > p.MemoryMB {
		return fail("ligar esta VM passaria a cota de memória do VDC %s (%s em uso + %s > %s)",
			p.Name, gb(usedMem/1024), gb(memMB/1024), gb(p.MemoryMB/1024))
	}
	return nil
}

// ---------- criar e atribuir VM ----------

// NewVM é o pedido de criação, já validado quanto ao formato.
type NewVM struct {
	CPUCores   int
	MemoryMB   int64
	DiskGB     int64
	ClassID    uuid.UUID
	StartAfter bool
}

// CheckCreate trava o VDC e o MSP e confere: quantidade de VMs do MSP,
// disco da classe no VDC (incluindo criações na fila) e, se a VM vai ligar
// no fim, CPU e memória do VDC.
func CheckCreate(ctx context.Context, st *store.Store, p store.Project, owner store.Tenant, vm NewVM) error {
	if err := st.LockProject(ctx, p.ID); err != nil {
		return err
	}
	msp, err := MSPOf(ctx, st, owner)
	if err != nil {
		return err
	}
	limits, err := st.GetMSPLimits(ctx, msp.ID, true)
	if err != nil {
		return err
	}
	if limits.MaxVMs != nil {
		n, err := st.MSPVMCount(ctx, msp.Path)
		if err != nil {
			return err
		}
		if n+1 > *limits.MaxVMs {
			return fail("o MSP %s já atingiu o limite de %d VMs", msp.Name, *limits.MaxVMs)
		}
	}
	var capGB int64 = -1
	className := ""
	for _, s := range p.Storage {
		if s.ClassID == vm.ClassID {
			capGB, className = s.CapacityGB, s.ClassName
		}
	}
	if capGB < 0 {
		return fail("este VDC não tem cota nessa classe de storage")
	}
	used, err := st.UsedDiskByClass(ctx, p.ID)
	if err != nil {
		return err
	}
	pendingDisk, pendingCPU, pendingMem, err := st.ProjectPendingCreates(ctx, p.ID)
	if err != nil {
		return err
	}
	inUse := used[vm.ClassID] + pendingDisk[vm.ClassID]
	if inUse+vm.DiskGB > capGB {
		return fail("disco %s insuficiente no VDC %s: %s em uso de %s, pedido de %s",
			className, p.Name, gb(inUse), gb(capGB), gb(vm.DiskGB))
	}
	if vm.CPUCores > p.CPUCores || vm.MemoryMB > p.MemoryMB {
		return fail("a VM é maior que a cota inteira do VDC %s (%d vCPU, %s)", p.Name, p.CPUCores, gb(p.MemoryMB/1024))
	}
	if vm.StartAfter {
		usedCPU, usedMem, err := st.ProjectCommitted(ctx, p.ID, uuid.Nil)
		if err != nil {
			return err
		}
		usedCPU += pendingCPU
		usedMem += pendingMem
		if usedCPU+vm.CPUCores > p.CPUCores {
			return fail("ligar a VM nova passaria a cota de vCPU do VDC %s (%d em uso + %d > %d); crie desligada",
				p.Name, usedCPU, vm.CPUCores, p.CPUCores)
		}
		if usedMem+vm.MemoryMB > p.MemoryMB {
			return fail("ligar a VM nova passaria a cota de memória do VDC %s (%s em uso + %s > %s); crie desligada",
				p.Name, gb(usedMem/1024), gb(vm.MemoryMB/1024), gb(p.MemoryMB/1024))
		}
	}
	return nil
}

// CheckAssign confere a atribuição de uma VM existente a um VDC: mesma
// localização, limite de VMs do MSP e, se ela estiver ligada, CPU e memória.
func CheckAssign(ctx context.Context, st *store.Store, p store.Project, owner store.Tenant, res store.ResourceInternal) error {
	if p.ClusterID == nil || *p.ClusterID != res.ClusterID {
		return fail("a VM está em outra localização; o VDC %s fica em %s", p.Name, p.Location)
	}
	if res.ProjectID != nil && *res.ProjectID == p.ID {
		return fail("a VM já está neste VDC")
	}
	if err := st.LockProject(ctx, p.ID); err != nil {
		return err
	}
	msp, err := MSPOf(ctx, st, owner)
	if err != nil {
		return err
	}
	limits, err := st.GetMSPLimits(ctx, msp.ID, true)
	if err != nil {
		return err
	}
	sameMSP := false
	if res.TenantID != nil {
		if cur, err := st.GetTenant(ctx, *res.TenantID); err == nil && strings.HasPrefix(cur.Path, msp.Path) {
			sameMSP = true // só troca de VDC dentro do mesmo MSP
		}
	}
	if limits.MaxVMs != nil && !sameMSP {
		n, err := st.MSPVMCount(ctx, msp.Path)
		if err != nil {
			return err
		}
		if n+1 > *limits.MaxVMs {
			return fail("o MSP %s já atingiu o limite de %d VMs", msp.Name, *limits.MaxVMs)
		}
	}
	if res.Status == "running" {
		usedCPU, usedMem, err := st.ProjectCommitted(ctx, p.ID, res.ResourceID)
		if err != nil {
			return err
		}
		memMB := res.MaxMemBytes / (1 << 20)
		if usedCPU+res.CPUs > p.CPUCores || usedMem+memMB > p.MemoryMB {
			return fail("a VM está ligada e não cabe na cota do VDC %s (vCPU %d+%d de %d, memória %s+%s de %s); desligue antes",
				p.Name, usedCPU, res.CPUs, p.CPUCores, gb(usedMem/1024), gb(memMB/1024), gb(p.MemoryMB/1024))
		}
	}
	return nil
}
