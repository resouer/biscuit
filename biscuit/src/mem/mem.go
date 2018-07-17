package mem

import "sync"
import "sync/atomic"
import "unsafe"
import "util"

const PGSHIFT uint = 12
const PGSIZE int = 1 << PGSHIFT
const PGOFFSET Pa_t = 0xfff
const PGMASK Pa_t = ^(PGOFFSET)

const PTE_P Pa_t = 1 << 0
const PTE_W Pa_t = 1 << 1
const PTE_U Pa_t = 1 << 2
const PTE_G Pa_t = 1 << 8
const PTE_PCD Pa_t = 1 << 4
const PTE_PS Pa_t = 1 << 7

const PTE_ADDR Pa_t = PGMASK

type Pa_t uintptr
type Bytepg_t [PGSIZE]uint8
type Pg_t [512]int
type Pmap_t [512]Pa_t

type Unpin_i interface {
	Unpin(Pa_t)
}

type Mmapinfo_t struct {
	Pg   *Pg_t
	Phys Pa_t
}

func Pg2bytes(pg *Pg_t) *Bytepg_t {
	return (*Bytepg_t)(unsafe.Pointer(pg))
}

func Bytepg2pg(pg *Bytepg_t) *Pg_t {
	return (*Pg_t)(unsafe.Pointer(pg))
}

func pg2pmap(pg *Pg_t) *Pmap_t {
	return (*Pmap_t)(unsafe.Pointer(pg))
}

func _pg2pgn(p_pg Pa_t) uint32 {
	return uint32(p_pg >> PGSHIFT)
}

func _refaddr(p_pg Pa_t) (*int32, uint32) {
	idx := _pg2pgn(p_pg) - Physmem.startn
	return &Physmem.pgs[idx].refcnt, idx
}

// can account for up to 16TB of mem
type Physpg_t struct {
	refcnt int32
	// index into pgs of next page on free list
	nexti uint32
}

type Physmem_t struct {
	pgs    []Physpg_t
	startn uint32
	// index into pgs of first free pg
	freei uint32
	pmaps uint32
	sync.Mutex
	dmapinit bool
	_vdirect uintptr
}

var Physmem = &Physmem_t{}

func (phys *Physmem_t) _refpg_new() (*Pg_t, Pa_t, bool) {
	return phys._phys_new(&phys.freei)
}

func (phys *Physmem_t) Refcnt(p_pg Pa_t) int {
	ref, _ := _refaddr(p_pg)
	c := atomic.AddInt32(ref, 1)
	return int(c)
}

func (phys *Physmem_t) Refup(p_pg Pa_t) {
	ref, _ := _refaddr(p_pg)
	c := atomic.AddInt32(ref, 1)
	// XXXPANIC
	if c <= 0 {
		panic("wut")
	}
}

// returns true if p_pg should be added to the free list and the index of the
// page in the pgs array
func (phys *Physmem_t) _refdec(p_pg Pa_t) (bool, uint32) {
	ref, idx := _refaddr(p_pg)
	c := atomic.AddInt32(ref, -1)
	// XXXPANIC
	if c < 0 {
		panic("wut")
	}
	return c == 0, idx
}

func (phys *Physmem_t) _reffree(idx uint32) {
	phys.Lock()
	onext := phys.freei
	phys.pgs[idx].nexti = onext
	phys.freei = idx
	phys.Unlock()
}

func (phys *Physmem_t) Refdown(p_pg Pa_t) bool {
	// add to freelist?
	if add, idx := phys._refdec(p_pg); add {
		phys._reffree(idx)
		return true
	}
	return false
}

var Zeropg *Pg_t

// refcnt of returned page is not incremented (it is usually incremented via
// Proc_t.page_insert). requires direct mapping.
func (phys *Physmem_t) Refpg_new() (*Pg_t, Pa_t, bool) {
	if !phys.dmapinit {
		panic("refpg_new")
	}
	pg, p_pg, ok := phys._refpg_new()
	if !ok {
		return nil, 0, false
	}
	*pg = *Zeropg
	return pg, p_pg, true
}

func (phys *Physmem_t) Refpg_new_nozero() (*Pg_t, Pa_t, bool) {
	pg, p_pg, ok := phys._refpg_new()
	if !ok {
		return nil, 0, false
	}
	return pg, p_pg, true
}

func (phys *Physmem_t) Pmap_new() (*Pmap_t, Pa_t, bool) {
	a, b, ok := phys._phys_new(&phys.pmaps)
	if ok {
		return pg2pmap(a), b, true
	}
	a, b, ok = phys.Refpg_new()
	return pg2pmap(a), b, ok
}

func (phys *Physmem_t) _phys_new(fl *uint32) (*Pg_t, Pa_t, bool) {
	if !phys.dmapinit {
		panic("dmap not initted")
	}

	var p_pg Pa_t
	var ok bool
	phys.Lock()
	ff := *fl
	if ff != ^uint32(0) {
		p_pg = Pa_t(ff+phys.startn) << PGSHIFT
		*fl = phys.pgs[ff].nexti
		ok = true
		if phys.pgs[ff].refcnt < 0 {
			panic("negative ref count")
		}
	}
	phys.Unlock()
	if ok {
		return phys.Dmap(p_pg), p_pg, true
	}
	return nil, 0, false
}

func (phys *Physmem_t) _phys_put(fl *uint32, p_pg Pa_t) {
	if add, idx := phys._refdec(p_pg); add {
		phys.Lock()
		phys.pgs[idx].nexti = *fl
		*fl = idx
		phys.Unlock()
	}
}

// decrease ref count of pml4, freeing it if no CPUs have it loaded into cr3.
func (phys *Physmem_t) Dec_pmap(p_pmap Pa_t) {
	phys._phys_put(&phys.pmaps, p_pmap)
}

// returns a page-aligned virtual address for the given physical address using
// the direct mapping
func (phys *Physmem_t) Dmap(p Pa_t) *Pg_t {
	pa := uintptr(p)
	if pa >= 1<<39 {
		panic("direct map not large enough")
	}

	v := phys._vdirect
	v += uintptr(util.Rounddown(int(pa), PGSIZE))
	return (*Pg_t)(unsafe.Pointer(v))
}

func (phys *Physmem_t) Dmap_v2p(v *Pg_t) Pa_t {
	va := (uintptr)(unsafe.Pointer(v))
	if va <= 1<<39 {
		panic("address isn't in the direct map")
	}

	pa := va - phys._vdirect
	return Pa_t(pa)
}

// returns a byte aligned virtual address for the physical address as slice of
// uint8s
func (phys *Physmem_t) Dmap8(p Pa_t) []uint8 {
	pg := phys.Dmap(p)
	off := p & PGOFFSET
	bpg := Pg2bytes(pg)
	return bpg[off:]
}

func (phys *Physmem_t) Pgcount() (int, int) {
	phys.Lock()
	r1 := 0
	for i := phys.freei; i != ^uint32(0); i = phys.pgs[i].nexti {
		r1++
	}
	r2 := phys.pmapcount()
	phys.Unlock()
	return r1, r2
}

func (phys *Physmem_t) _pmcount(pml4 Pa_t, lev int) int {
	pg := pg2pmap(phys.Dmap(pml4))
	ret := 0
	for _, pte := range pg {
		if pte&PTE_U != 0 && pte&PTE_P != 0 {
			ret += 1 + phys._pmcount(Pa_t(pte&PTE_ADDR), lev-1)
		}
	}
	return ret
}

func (phys *Physmem_t) pmapcount() int {
	c := 0
	for ni := phys.pmaps; ni != ^uint32(0); ni = phys.pgs[ni].nexti {
		v := phys._pmcount(Pa_t(ni+phys.startn)<<PGSHIFT, 4)
		c += v
	}
	return c
}
