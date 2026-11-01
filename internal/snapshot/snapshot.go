// Package snapshot holds the typed per-node inventory the dashboard renders, alongside the metrics.
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

// Module is one DIMM; Speed is MT/s as SMBIOS reports it, not hertz.
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

// PCIDevice is one PCI device with sysfs enrichment; Has* flags separate absent from zero.
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
	// Master is the bond or bridge this interface slaves to; its traffic is counted there too.
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

	// Counters are cumulative since boot; HasRates is false until a previous sample exists.
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

	// Limits is keyed by kind (critical/max/min); empty when the driver publishes none.
	Limits   map[string]float64
	HasAlarm bool
	Alarm    float64
}

// Core is one hardware thread's live state; frequencies are hertz like every other speed here.
type Core struct {
	Index     int
	HasUsage  bool
	UsagePct  float64
	CurrentHz float64
	MinimumHz float64
	MaximumHz float64
	Governor  string
}

// ModeShare is one /proc/stat mode's share of CPU time over the last interval, 0-1.
type ModeShare struct {
	Mode  string
	Ratio float64
}

// CPU is the node's live processor state.
type CPU struct {
	HasUsage bool
	UsagePct float64 // node total
	Cores    []Core

	// Modes is empty on the first scrape, when there is no interval to divide by.
	Modes []ModeShare
}

// Memory is /proc/meminfo in bytes; Present is false on nodes that did not report it.
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

	// HasLoad is false when the LoadAvg RPC failed; 0.0 is a real reading on an idle node.
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

// Service is one init-system service; Unknown means no health check, not unhealthy.
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

// Diagnostic is one Talos warning; ID maps to talos.dev/diagnostic/<id>.
type Diagnostic struct {
	ID      string
	Message string
}

// Health is the node's own verdict: service state, boot stage and diagnostic warnings.
type Health struct {
	Stage            string
	Ready            bool
	HasMachineStatus bool
	Services         []Service
	UnmetConditions  []UnmetCondition
	Diagnostics      []Diagnostic
}

// Extension is one installed system extension; Talos's pseudo-entries go to Identity instead.
type Extension struct {
	Name    string
	Version string
}

// Identity is the node's software: image, extensions, boot mode and security posture.
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

// TimeSync is clock discipline; Talos's ntpd verdict and the kernel's flag are separate facts.
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

// KernelParam is one tunable, current vs default; Unsupported means the kernel ignores it.
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

// Volume is one Talos volume; Type separates partitions from pseudo-volumes.
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

// Mount is a mounted volume's metadata; usage bytes come from the Mounts RPC instead.
type Mount struct {
	ID           string
	Source       string
	Target       string
	Filesystem   string
	ReadOnly     bool
	ProjectQuota bool
}

// Volumes is the node's volume layer: provisioned, mounted, and the install disk.
type Volumes struct {
	Volumes        []Volume
	Mounts         []Mount
	SystemDisk     string
	SystemDiskPath string
}

// GPU is one card; Has* flags separate an unpublished quantity from a genuine zero.
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

// DiskIO is one device's counters; a partition's are also in its parent, so sums double-count.
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

// Node is everything known about a machine; a nil section means that collector has not reported.
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

// Store holds one Node per member; collectors write their own section, so updates are per-section.
type Store struct {
	mu    sync.RWMutex
	nodes map[string]*Node
	now   func() time.Time
}

// New returns an empty store.
func New() *Store {
	return &Store{nodes: make(map[string]*Node), now: time.Now}
}

// Update copy-on-writes the snapshot under the lock, so a published *Node is never mutated again.
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

// Node returns an immutable snapshot, safe to read without the lock; callers must not mutate it.
func (s *Store) Node(name string) *Node {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodes[name]
}

// Prune drops departed nodes; an empty live set means discovery has not synced and prunes nothing.
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
