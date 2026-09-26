package nodes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

const (
	// talosReconcileInterval is how often the pool reloads the config and
	// retries nodes without a verified client. Steady-state cost is one
	// local file read per tick (no RPCs once all nodes are verified), so
	// a short interval keeps startup snappy and rotation pickup fast.
	talosReconcileInterval = 10 * time.Second
	// talosVerifyTimeout bounds a single per-node Talos API check.
	talosVerifyTimeout = 10 * time.Second
	// talosVerifyConcurrency bounds the parallel node verification. Sequentially,
	// a fresh pool of N nodes cost up to N * talosVerifyTimeout before the first
	// scrape; in parallel the pool is ready in one timeout regardless of size.
	talosVerifyConcurrency = 8
	// talosConfigRetry is the wait between talosconfig load attempts.
	talosConfigRetry = 10 * time.Second
)

// TalosClientPool keeps one machinery client per node IP and verifies each
// node's Talos API (machine.MachineService/Version).
//
// The client certificate comes from the resolved talosconfig: in-cluster
// that is the serviceaccounts.talos.dev secret (Built-in Apps & In-Cluster
// Authentication — mandatory), locally ~/.talos/config. In-cluster the SA
// controller rotates the client cert every 6 h; the reconcile loop detects
// the file change (fingerprint) and rebuilds all clients.
type TalosClientPool struct {
	log       *slog.Logger
	listNodes func() []Node

	mu         sync.Mutex
	cfg        *TalosConfig
	clients    map[string]*talosclient.Client // keyed by node IP
	verified   map[string]bool                // keyed by node name
	versions   map[string]string              // node name → verified Talos version
	verifiedIP map[string]string              // node name → the IP that verification used
}

// NewTalosClientPool creates a pool; listNodes provides the current node
// snapshot (typically Manager.Nodes).
func NewTalosClientPool(log *slog.Logger, listNodes func() []Node) *TalosClientPool {
	return &TalosClientPool{
		log:        log,
		listNodes:  listNodes,
		clients:    make(map[string]*talosclient.Client),
		verified:   make(map[string]bool),
		versions:   make(map[string]string),
		verifiedIP: make(map[string]string),
	}
}

// Run loads the talosconfig and reconciles the per-node clients until ctx
// is done. Failures are logged and retried; Run never fails hard.
func (p *TalosClientPool) Run(ctx context.Context) {
	defer p.closeAll()

	if inCluster() && !talosConfigAvailable() {
		p.log.Warn("running in-cluster without a resolvable talosconfig; expecting the "+
			"serviceaccounts.talos.dev secret (Built-in Apps & In-Cluster Authentication) "+
			"to be mounted",
			"env", constants.TalosConfigEnvVar,
			"mount", constants.ServiceAccountMountPath+"/"+constants.TalosconfigFilename)
	}

	for {
		cfg, err := LoadTalosConfig()
		if err == nil {
			p.setConfig(&cfg)
			p.log.Info("talos config loaded", "path", cfg.Path, "context", cfg.ContextName, "cert_expires", cfg.CertNotAfter.UTC().Format(time.RFC3339))
			break
		}
		p.log.Error("talosconfig unavailable, retrying", "err", err, "retry_in", talosConfigRetry.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(talosConfigRetry):
		}
	}

	p.reconcile(ctx)
	ticker := time.NewTicker(talosReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.reconcile(ctx)
		}
	}
}

// Ready reports whether a talosconfig has been loaded and per-node clients
// can be created.
func (p *TalosClientPool) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg != nil
}

// Get returns the machinery client for a node IP, creating it if needed.
// Used by the collector framework (phase 2c).
func (p *TalosClientPool) Get(ip string) (*talosclient.Client, error) {
	return p.getOrCreate(ip)
}

// Status reports the last verified Talos version for a node and whether its
// API check currently passes.
func (p *TalosClientPool) Status(name string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.versions[name], p.verified[name]
}

func talosConfigAvailable() bool {
	_, err := LoadTalosConfig()
	return err == nil
}

// markVerified records a successful Version check. The IP is kept alongside so
// reconcile can tell a stable node from one that moved.
func (p *TalosClientPool) markVerified(name, ip, version string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verified[name] = true
	p.versions[name] = version
	p.verifiedIP[name] = ip
}

func (p *TalosClientPool) reconcile(ctx context.Context) {
	// Read the config file and list nodes before taking the lock: both touch
	// the filesystem or another subsystem, and holding the mutex across them
	// blocked every Get for the duration. The old code also unlocked and
	// relocked mid-function to log, which briefly exposed a half-rebuilt pool.
	fresh, freshErr := LoadTalosConfig()
	current := p.listNodes()

	p.mu.Lock()

	// Rebuild when the file changed. In-cluster this is how the 6 h client-cert
	// rotation by talos-sa-controller is picked up.
	rebuilt := false
	if freshErr == nil && p.cfg != nil && fresh.Fingerprint != p.cfg.Fingerprint {
		p.cfg = &fresh
		p.closeAllLocked()
		p.verified = make(map[string]bool)
		p.versions = make(map[string]string)
		rebuilt = true
	}

	// Drop clients and verification state for nodes that are gone.
	liveIPs := make(map[string]bool, len(current))
	for _, n := range current {
		if n.IP != "" {
			liveIPs[n.IP] = true
		}
	}
	for ip, c := range p.clients {
		if !liveIPs[ip] {
			_ = c.Close()
			delete(p.clients, ip)
		}
	}
	// Forget verification for nodes that are gone, and for nodes whose IP
	// moved: the client for the old IP was just closed above, so the version
	// on file was read from a machine we no longer talk to. Keying clients by
	// IP and verification by name meant a same-name-new-IP node kept reporting
	// its predecessor's Talos version forever.
	byName := make(map[string]string, len(current))
	for _, n := range current {
		byName[n.Name] = n.IP
	}
	for name := range p.verified {
		ip, stillThere := byName[name]
		if !stillThere || ip != p.verifiedIP[name] {
			delete(p.verified, name)
			delete(p.versions, name)
			delete(p.verifiedIP, name)
		}
	}

	// Collect nodes still needing verification, outside the lock.
	var pending []Node
	for _, n := range current {
		if n.IP != "" && !p.verified[n.Name] {
			pending = append(pending, n)
		}
	}
	p.mu.Unlock()

	if rebuilt {
		p.log.Info("talos config changed, clients rebuilt", "path", fresh.Path)
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, talosVerifyConcurrency)
	for i := range pending {
		n := pending[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p.verifyOne(ctx, n)
		}()
	}
	wg.Wait()
}

func (p *TalosClientPool) verifyOne(ctx context.Context, n Node) {
	c, err := p.getOrCreate(n.IP)
	if err != nil {
		p.log.Warn("talos client create failed", "node", n.Name, "ip", n.IP, "err", err)
		return
	}

	vctx, cancel := context.WithTimeout(ctx, talosVerifyTimeout)
	defer cancel()
	ver, err := c.Version(vctx)
	if err != nil {
		p.log.Warn("talos api check failed", "node", n.Name, "ip", n.IP, "err", err)
		return
	}
	if len(ver.Messages) == 0 || ver.Messages[0].Version == nil {
		p.log.Warn("talos api returned no version info", "node", n.Name, "ip", n.IP)
		return
	}

	tag := ver.Messages[0].Version.Tag
	p.markVerified(n.Name, n.IP, tag)
	p.log.Info("talos client ok", "node", n.Name, "ip", n.IP, "talos_version", tag)
}

// apidEndpoint builds the apid URL for a node address. JoinHostPort brackets
// IPv6 literals; a plain Sprintf produced an unparseable endpoint for an
// address like fd00::1, so IPv6-only clusters could never connect.
func apidEndpoint(ip string) string {
	return "https://" + net.JoinHostPort(ip, strconv.Itoa(constants.ApidPort))
}

func (p *TalosClientPool) getOrCreate(ip string) (*talosclient.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if c, ok := p.clients[ip]; ok {
		return c, nil
	}
	if p.cfg == nil {
		return nil, errors.New("talosconfig not loaded yet")
	}

	c, err := talosclient.New(context.Background(),
		talosclient.WithConfigContext(p.cfg.context),
		talosclient.WithEndpoints(apidEndpoint(ip)),
	)
	if err != nil {
		return nil, fmt.Errorf("talos client for %s: %w", ip, err)
	}
	p.clients[ip] = c
	return c, nil
}

func (p *TalosClientPool) setConfig(cfg *TalosConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg = cfg
	p.closeAllLocked()
}

func (p *TalosClientPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeAllLocked()
}

func (p *TalosClientPool) closeAllLocked() {
	for ip, c := range p.clients {
		_ = c.Close()
		delete(p.clients, ip)
	}
}
