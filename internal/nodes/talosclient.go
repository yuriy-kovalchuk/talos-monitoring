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
	// talosReconcileInterval is the config reload and verify-retry period; steady state is one file read.
	talosReconcileInterval = 10 * time.Second
	// talosVerifyTimeout bounds a single per-node Talos API check.
	talosVerifyTimeout = 10 * time.Second
	// talosVerifyConcurrency bounds parallel verification so a fresh pool is ready in one timeout.
	talosVerifyConcurrency = 8
	// talosConfigRetry is the wait between talosconfig load attempts.
	talosConfigRetry = 10 * time.Second
	// clientRetireGrace keeps a replaced client open for collections still using it; it is above the node timeout.
	clientRetireGrace = 30 * time.Second
)

// retiredClient is a replaced client, closed once in-flight collections have had time to finish.
type retiredClient struct {
	c       *talosclient.Client
	closeAt time.Time
}

// TalosClientPool keeps one machinery client per node IP and verifies each node's Talos API.
type TalosClientPool struct {
	log       *slog.Logger
	listNodes func() []Node
	now       func() time.Time

	mu         sync.Mutex
	retired    []retiredClient // replaced on cert rotation or node removal, closed after clientRetireGrace
	cfg        *TalosConfig
	clients    map[string]*talosclient.Client // keyed by node IP
	verified   map[string]bool                // keyed by node name
	versions   map[string]string              // node name → verified Talos version
	verifiedIP map[string]string              // node name → the IP that verification used
}

// NewTalosClientPool reads the current node set from listNodes (typically Manager.Nodes).
func NewTalosClientPool(log *slog.Logger, listNodes func() []Node) *TalosClientPool {
	return &TalosClientPool{
		log:        log,
		listNodes:  listNodes,
		now:        time.Now,
		clients:    make(map[string]*talosclient.Client),
		verified:   make(map[string]bool),
		versions:   make(map[string]string),
		verifiedIP: make(map[string]string),
	}
}

// Run loads the talosconfig and reconciles per-node clients until ctx is done, retrying failures.
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

// Ready reports whether a talosconfig is loaded and per-node clients can be created.
func (p *TalosClientPool) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg != nil
}

// Get returns the machinery client for a node IP, creating it if needed.
func (p *TalosClientPool) Get(ip string) (*talosclient.Client, error) {
	return p.getOrCreate(ip)
}

// Status reports the last verified Talos version and whether the API check currently passes.
func (p *TalosClientPool) Status(name string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.versions[name], p.verified[name]
}

func talosConfigAvailable() bool {
	_, err := LoadTalosConfig()
	return err == nil
}

// markVerified records a successful Version check, keeping the IP to spot a node that moved.
func (p *TalosClientPool) markVerified(name, ip, version string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verified[name] = true
	p.versions[name] = version
	p.verifiedIP[name] = ip
}

func (p *TalosClientPool) reconcile(ctx context.Context) {
	// Config read and node list happen outside the lock: holding it there blocked every Get.
	fresh, freshErr := LoadTalosConfig()
	current := p.listNodes()

	p.mu.Lock()
	p.closeRetiredLocked(p.now())

	// A changed file is how the SA controller's 6 h client-cert rotation is picked up.
	rebuilt := false
	if freshErr == nil && p.cfg != nil && fresh.Fingerprint != p.cfg.Fingerprint {
		p.cfg = &fresh
		// Retired, not closed: a collection holding the old client would fail with "connection is closing".
		p.retireAllLocked()
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
			p.retireLocked(c)
			delete(p.clients, ip)
		}
	}
	// Forget departed nodes and nodes whose IP moved, or the old IP's Talos version is reported forever.
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
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			p.verifyOne(ctx, n)
		})
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

// apidEndpoint joins host and port, which brackets the IPv6 literals Sprintf would break.
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

// closeAll is the shutdown path: nothing is in flight any more, so retired clients close too.
func (p *TalosClientPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeAllLocked()
	for _, r := range p.retired {
		_ = r.c.Close()
	}
	p.retired = nil
}

func (p *TalosClientPool) retireLocked(c *talosclient.Client) {
	p.retired = append(p.retired, retiredClient{c: c, closeAt: p.now().Add(clientRetireGrace)})
}

func (p *TalosClientPool) retireAllLocked() {
	for ip, c := range p.clients {
		p.retireLocked(c)
		delete(p.clients, ip)
	}
}

// closeRetiredLocked closes the retired clients whose grace period ended by now.
func (p *TalosClientPool) closeRetiredLocked(now time.Time) {
	kept := p.retired[:0]
	for _, r := range p.retired {
		if now.Before(r.closeAt) {
			kept = append(kept, r)
			continue
		}
		_ = r.c.Close()
	}
	p.retired = kept
}

func (p *TalosClientPool) closeAllLocked() {
	for ip, c := range p.clients {
		_ = c.Close()
		delete(p.clients, ip)
	}
}
