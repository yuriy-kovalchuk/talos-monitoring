// Package snapshot holds the typed hardware inventory each collector gathers,
// so the dashboard reads Go structs instead of re-parsing the Prometheus
// exposition format it just produced.
//
// The exposition format is a wire format for Prometheus, not an internal data
// layer. Routing the UI through it meant every measurement had to be a label
// string, parse failures silently rendered as 0, a renamed label emptied the
// page with no compiler error, and rendering one node page cost O(all metrics
// x all nodes). Collectors already hold this data typed; they now write it
// here as well as registering metrics, and /metrics and the dashboard become
// two renderers over one source of truth.
//
// The pattern mirrors history.Store, which cpu.Collect has always filled
// alongside its metrics; this generalises it from time series to inventory.
package snapshot

import (
	"sync"
	"time"
)

// System is the machine's DMI identity (hardware.SystemInformation).
type System struct {
	Manufacturer string
	Product      string
	Version      string
	SerialNumber string
	UUID         string
	SKUNumber    string
	WakeUpType   string
}

// Socket is one CPU package (hardware.Processor). Speeds are hertz.
type Socket struct {
	ID           string
	Product      string
	PartNumber   string
	SerialNumber string
	AssetTag     string
	Cores        int
	CoresEnabled int
	Threads      int
	MaxHertz     float64
	BootHertz    float64
	Status       int
}

// Module is one DIMM (hardware.MemoryModule). Speed is transfers per second,
// not hertz: SMBIOS reports DDR5-6000 as 6000 MT/s on a 3000 MHz clock.
type Module struct {
	Slot          string
	Manufacturer  string
	Product       string
	SerialNumber  string
	AssetTag      string
	DeviceLocator string
	BankLocator   string
	SizeBytes     float64
	SpeedTransfer float64
}

// Disk is one block device (block.Disk).
type Disk struct {
	Device     string
	Model      string
	Serial     string
	WWID       string
	UUID       string
	Transport  string
	SubSystem  string
	BusPath    string
	Attachment string // "local" | "network" | "virtual"
	SizeBytes  float64
	SectorSize float64
	Rotational bool
	Readonly   bool
	CDROM      bool
}

// PCIDevice is one device on the PCI bus, with its sysfs enrichment. The
// Has* flags distinguish "not reported by this device" from a zero reading:
// bridges and root ports have no link state at all.
type PCIDevice struct {
	BDF                string
	Class              string
	Subclass           string
	Vendor             string
	Product            string
	ClassID            string
	SubclassID         string
	VendorID           string
	ProductID          string
	Driver             string
	Revision           string
	SubsystemVendorID  string
	SubsystemProductID string

	HasLink        bool
	LinkSpeedGTps  float64
	LinkWidth      float64
	MaxSpeedGTps   float64
	MaxWidth       float64
	PowerState     string
	HasNUMA        bool
	NUMANode       float64
	HasIRQ         bool
	IRQ            float64
	HasEnabled     bool
	Enabled        float64
	HasAER         bool
	AERCorrectable float64
	AERFatal       float64
	AERNonFatal    float64
}

// Address is one IP assigned to a link.
type Address struct {
	Address string
	Family  string
	Scope   string
}

// Link is one network interface, its counters and its addresses.
type Link struct {
	Name string
	Type string
	Kind string // "" for a physical NIC
	// Master names the bond or bridge this interface is a slave of; "" means the
	// interface is topmost. A slave's traffic is counted on its master too, so an
	// aggregate over every interface double-counts a bonded node.
	Master          string
	HWAddr          string
	Driver          string
	DriverVersion   string
	FirmwareVersion string
	BusPath         string
	PCIID           string
	Vendor          string
	Product         string
	Port            string
	Duplex          string
	Up              bool
	Carrier         bool
	SpeedMbit       float64
	MTUBytes        float64
	Addresses       []Address

	// Counters are cumulative since boot. HasRates is false on the first
	// scrape after a restart, when there is no previous sample to diff.
	HasCounters bool
	HasRates    bool
	RxPerSecond float64
	TxPerSecond float64
	RxBytes     float64
	TxBytes     float64
	RxDropped   float64
	TxDropped   float64
	RxErrors    float64
	TxErrors    float64
}

// Sensor is one hwmon or thermal-zone reading with its threshold context.
type Sensor struct {
	Chip     string // full sysfs path
	ChipName string // driver name (k10temp, coretemp, ...)
	Sensor   string // sysfs name (temp1, fan0, in0, ...)
	Kind     string // temperature | fan | voltage | power | frequency
	Label    string // human label when the driver gives one
	Value    float64

	// Limits is keyed by kind ("critical", "max", "min"); empty when the
	// driver publishes none, which is normal for k10temp and amdgpu.
	Limits   map[string]float64
	HasAlarm bool
	Alarm    float64
}

// Core is one hardware thread's live state.
type Core struct {
	Index     int
	HasUsage  bool
	UsagePct  float64
	CurrentHz float64
	MinimumHz float64
	MaximumHz float64
	Governor  string
}

// ModeShare is one /proc/stat mode's share of the node's CPU time over the
// last scrape interval, 0-1. The shares sum to 1.
type ModeShare struct {
	Mode  string
	Ratio float64
}

// CPU is the node's live processor state.
type CPU struct {
	HasUsage bool
	UsagePct float64 // node total
	Cores    []Core

	// Modes is empty on the first scrape after a restart, when there is no
	// interval to divide by.
	Modes []ModeShare
}

// Memory is /proc/meminfo, in bytes. Present is false on nodes that did not
// report it.
type Memory struct {
	Present     bool
	Total       float64
	Free        float64
	Available   float64
	Buffers     float64
	Cached      float64
	SwapTotal   float64
	SwapFree    float64
	Committed   float64
	CommitLimit float64
}

// Runtime is the node's Talos identity and liveness (nodeapi).
type Runtime struct {
	Version   string
	SHA       string
	Arch      string
	Built     string
	Uptime    float64 // seconds
	Running   uint64
	Blocked   uint64
	HasUptime bool

	// HasLoad is false on a node whose LoadAvg RPC failed; 0.0 load is a
	// legitimate reading on an idle machine and must not stand in for it.
	HasLoad bool
	Load1   float64
	Load5   float64
	Load15  float64
}

// Filesystem is one mounted filesystem on the node.
type Filesystem struct {
	Mountpoint string
	Device     string
	SizeBytes  float64
	AvailBytes float64
	UsedBytes  float64
	PV         string // set when the mount backs a PersistentVolume
	Kind       string // how the volume is reached: block | network | other
}

// Service is one init-system service (v1alpha1.Service).
//
// Healthy and Unknown are independent: a service with no health check reports
// Healthy=false with Unknown=true, which means "no opinion", not "unhealthy".
type Service struct {
	ID      string
	Running bool
	Healthy bool
	Unknown bool
}

// UnmetCondition is one reason a machine is not ready at its current stage.
type UnmetCondition struct {
	Name   string
	Reason string
}

// Diagnostic is one warning raised by Talos's own diagnostics. ID maps to
// talos.dev/diagnostic/<id>.
type Diagnostic struct {
	ID      string
	Message string
}

// Health is the node's own verdict on whether it is working: per-service
// state, the aggregated boot stage, and any diagnostic warnings.
//
// HasMachineStatus distinguishes "the machine reported stage running" from
// "the MachineStatus resource was absent", which an empty Stage alone cannot.
type Health struct {
	Stage            string
	Ready            bool
	HasMachineStatus bool
	Services         []Service
	UnmetConditions  []UnmetCondition
	Diagnostics      []Diagnostic
}

// Extension is one installed system extension (runtime.ExtensionStatus).
// The virtual `schematic` and `modules.dep` entries Talos reports alongside
// real extensions are lifted out into Identity's own fields instead.
type Extension struct {
	Name    string
	Version string
}

// Identity is what software the node is running: the image it was built from,
// the extensions in it, how it booted, and its security posture.
//
// HasSecurityState distinguishes "SecureBoot is off" from "the SecurityState
// resource was absent", which the booleans alone cannot.
type Identity struct {
	Schematic     string // Image Factory schematic id
	KernelVersion string // from the synthetic modules.dep entry
	Extensions    []Extension
	Cmdline       string
	BootedEntry   string

	HasSecurityState        bool
	SecureBoot              bool
	BootedWithUKI           bool
	ModuleSignatureEnforced bool
	SELinuxState            string
	FIPSState               string
}

// TimeSync is the node's clock discipline (time.Status + time.AdjtimeStatus).
//
// Synced and KernelSynced are different facts: the first is Talos's ntpd
// verdict on its servers, the second the kernel's STA_UNSYNC flag.
type TimeSync struct {
	HasStatus    bool
	Synced       bool
	SyncDisabled bool
	Epoch        int

	HasAdjtime      bool
	OffsetSeconds   float64
	MaxErrorSeconds float64
	EstErrorSeconds float64
	FrequencyRatio  float64
	KernelSynced    bool
	State           string // TIME_OK, TIME_ERROR, ...
}

// KernelParam is one kernel tunable, current versus default. Unsupported means
// the running kernel does not know it, so a configured value is ignored.
type KernelParam struct {
	Name        string
	Current     string
	Default     string
	Unsupported bool
}

// KernelModule is one loaded module.
type KernelModule struct {
	Name           string
	State          string
	SizeBytes      float64
	ReferenceCount int
}

// Kernel is the node's tunables and loaded modules.
type Kernel struct {
	Params  []KernelParam
	Modules []KernelModule
}

// Volume is one Talos volume. Type separates real partitions from the
// directory, overlay and symlink pseudo-volumes Talos also models here.
type Volume struct {
	ID         string
	Type       string
	Phase      string
	Ready      bool
	Filesystem string
	Location   string
	SizeBytes  float64
	PrettySize string

	EncryptionProvider    string // "" when not encrypted
	EncryptionFailedSyncs int
	ErrorMessage          string
}

// Mount is one mounted volume's metadata. Usage bytes are NOT here — they come
// from the Mounts RPC via Filesystem, which is the only source that has them.
type Mount struct {
	ID           string
	Source       string
	Target       string
	Filesystem   string
	ReadOnly     bool
	ProjectQuota bool
}

// Volumes is the node's volume layer: what Talos provisioned, what mounted,
// and which disk it installed itself on.
type Volumes struct {
	Volumes        []Volume
	Mounts         []Mount
	SystemDisk     string
	SystemDiskPath string
}

// GPU is one graphics or compute card. The Has* flags separate "this driver
// does not publish the quantity" from a genuine zero — an Intel iGPU reports
// no utilisation at all, and 0% busy would be a lie.
type GPU struct {
	Card   string
	Driver string
	PCIID  string
	Slot   string // PCI BDF, joins onto the PCI inventory

	HasBusy     bool
	BusyPercent float64
	Suspended   bool // card is runtime-suspended (asleep); busy is a derived 0

	HasVRAM        bool
	VRAMTotalBytes float64
	VRAMUsedBytes  float64

	HasGTT        bool
	GTTTotalBytes float64
	GTTUsedBytes  float64
}

// DiskIO is one block device's I/O counters, in base units. Kind is disk,
// partition or virtual — a partition's counters are also counted in its
// parent, so summing across every device double-counts a partitioned disk.
type DiskIO struct {
	Device string
	Kind   string

	ReadBytes       float64
	WrittenBytes    float64
	ReadsCompleted  float64
	WritesCompleted float64
	IOInProgress    float64
	IOTimeSeconds   float64
}

// Node is everything known about one machine. A nil section means that
// collector has not reported yet (or is disabled), which the UI renders as
// absent rather than as zero.
type Node struct {
	Updated time.Time

	System      *System
	Sockets     []Socket
	Modules     []Module
	Disks       []Disk
	PCI         []PCIDevice
	Links       []Link
	Sensors     []Sensor
	CPU         *CPU
	Memory      *Memory
	Runtime     *Runtime
	Filesystems []Filesystem
	Health      *Health
	Identity    *Identity
	TimeSync    *TimeSync
	Kernel      *Kernel
	Volumes     *Volumes
	GPUs        []GPU
	DiskIO      []DiskIO
}

// Store holds one Node per cluster member. Collectors write their own section
// concurrently and at different cadences, so updates are per-section: a
// collector that does not run this tick leaves the previous value in place,
// matching how the scraper keeps the last good metrics.
//
// Readers (the dashboard) and writers (the scheduler) run concurrently, so
// published snapshots are immutable — see Update.
type Store struct {
	mu    sync.RWMutex
	nodes map[string]*Node
	now   func() time.Time
}

// New returns an empty store.
func New() *Store {
	return &Store{nodes: make(map[string]*Node), now: time.Now}
}

// Update applies fn to a node's snapshot, creating it if needed. fn runs under
// the store lock, so it must not block.
//
// Copy-on-write: fn mutates a fresh copy, which then replaces the map entry.
// A published *Node is never written again, so a reader that already holds one
// keeps a consistent view without holding the lock. Mutating in place raced
// with every dashboard request — Node() hands the pointer out and the caller
// reads its fields outside the lock, so a slice header could be read while
// being written.
func (s *Store) Update(node string, fn func(*Node)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	next := &Node{}
	if cur, ok := s.nodes[node]; ok {
		*next = *cur // shallow: sections are replaced wholesale, never appended to
	}
	fn(next)
	next.Updated = s.now()
	s.nodes[node] = next
}

// Node returns a node's snapshot, or nil when nothing has been collected.
//
// The result is immutable and safe to read without the lock: Update publishes
// a new *Node rather than editing the one in place. Callers must not mutate
// it, and must not assume two calls return the same pointer.
func (s *Store) Node(name string) *Node {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodes[name]
}

// Prune drops nodes no longer in the cluster. An empty live set prunes
// nothing: it means discovery has not synced, not that the cluster is empty.
func (s *Store) Prune(live map[string]struct{}) {
	if s == nil || len(live) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.nodes {
		if _, ok := live[name]; !ok {
			delete(s.nodes, name)
		}
	}
}
