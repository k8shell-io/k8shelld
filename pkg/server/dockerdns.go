package server

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/miekg/dns"
)

const (
	DNSListenIP    = "127.0.0.53"
	DNSListenPort  = "53"
	dockerSock     = "/var/run/dind/docker.sock"
	DNSCacheMaxAge = 500
)

// DockerDNSConf represents the configuration for the Docker DNS service (if enabled)
type DockerDNSConf struct {
	Enabled       bool     `yaml:"enabled"`
	Fqdn          bool     `yaml:"fqdn"`
	ContainerName bool     `yaml:"containerName"`
	ContainerId   bool     `yaml:"containerId"`
	DNSNames      bool     `yaml:"dnsNames"`
	UpstreamDNS   string   `yaml:"upstreamDNS"`
	Searches      []string `yaml:"searches"`
}

type DockerDNSCache struct {
	data      map[string][]string
	timestamp time.Time
}

type DockerDNS struct {
	logger            *Logger
	server            *dns.Server
	upstreamDNS       string
	baseDomain        string
	searches          []string
	enabled           bool
	fqdn              bool
	containerName     bool
	containerId       bool
	dnsNames          bool
	upstreamDNSClient *dns.Client
	cacheMutex        sync.Mutex
	cache             *DockerDNSCache
}

type DockerDNSRequest struct {
	Status string `json:"status"`
}

type DockerDNSResponse struct {
	Status string `json:"status"`
}

func NewDockerDNS(fqdn, containerName, containerId, dnsNames bool, upstreamDNS string, searches []string,
	defaultDNS string) (*DockerDNS, error) {
	var d *DockerDNS = &DockerDNS{
		logger:            NewLogger("docker-dns"),
		server:            nil,
		upstreamDNS:       upstreamDNS,
		searches:          searches,
		baseDomain:        "",
		enabled:           false,
		fqdn:              fqdn,
		containerName:     containerName,
		containerId:       containerId,
		dnsNames:          dnsNames,
		upstreamDNSClient: &dns.Client{},
		cacheMutex:        sync.Mutex{},
	}

	if d.upstreamDNS == "" || d.upstreamDNS == DNSListenIP {
		d.upstreamDNS = defaultDNS
		d.logger.Warn("Not a valid nameserver specified, using default DNS server %s", defaultDNS)
	}

	if len(d.searches) > 0 {
		d.baseDomain = d.searches[0]
	}

	d.logger.Info("Creating Docker DNS server, upstream DNS: %s, base domain: %s, searches: %v",
		d.upstreamDNS, d.baseDomain, d.searches)

	return d, nil
}

// Start the DNS server
func (d *DockerDNS) Run() {
	d.server = &dns.Server{
		Addr: DNSListenIP + ":" + DNSListenPort,
		Net:  "udp",
	}

	d.logger.Info("Starting DNS server on %s", DNSListenIP+":"+DNSListenPort)
	d.enabled = true

	// Use a goroutine to listen for shutdown signals
	go func() {
		dns.HandleFunc(".", d.handleDNSRequest)
		if err := d.server.ListenAndServe(); err != nil {
			d.logger.Error("Failed to start DNS server: %v", err)
		}
	}()
}

// Stop the DNS server
func (d *DockerDNS) Stop() {
	if d.server != nil {
		d.logger.Info("Stopping DNS server")
		d.server.Shutdown()
	}
}

// Disable the DNS server
func (d *DockerDNS) Disable() {
	if d.enabled {
		d.logger.Info("Disabling container names resolution")
		d.enabled = false
	}
}

// Disable the DNS server
func (d *DockerDNS) Enable() {
	if !d.enabled {
		d.logger.Info("Enabling container names resolution")
		d.enabled = true
	}
}

func (d *DockerDNS) handleDNSRequest(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)

	if d.enabled {
		_name := strings.TrimSuffix(r.Question[0].Name, ".")

		// Check container mappings first
		mappings, err := d.GetContainerMappings(DNSCacheMaxAge)
		if err != nil {
			d.logger.Error("Failed to get container mappings: %v", err)
		} else {
			for ip, names := range mappings {
				for _, n := range names {
					if n == _name || (d.baseDomain != "" && n+"."+d.baseDomain == _name) {
						rr := &dns.A{
							Hdr: dns.RR_Header{
								Name:   r.Question[0].Name,
								Rrtype: dns.TypeA,
								Class:  dns.ClassINET,
							},
							A: net.ParseIP(ip),
						}
						m.Answer = append(m.Answer, rr)
						w.WriteMsg(m)
						return
					}
				}
			}
		}
	}

	// Forward request to upstream DNS server
	d.logger.Debug("Forwarding DNS request for %s to upstream DNS %s", r.Question[0].Name, d.upstreamDNS)

	resp, _, err := d.upstreamDNSClient.Exchange(r, d.upstreamDNS+":53")
	if err != nil {
		d.logger.Error("Upstream DNS lookup failed for %s: %v", r.Question[0].Name, err)
		m.SetRcode(r, dns.RcodeServerFailure)
		w.WriteMsg(m)
		return
	}

	resp.SetReply(r)
	w.WriteMsg(resp)
}

// GetContainerMappings retrieves a mapping of container IP addresses to unique names
// based on the provided Unix socket path for the Docker API.
func (d *DockerDNS) GetContainerMappings(maxage int) (map[string][]string, error) {
	d.cacheMutex.Lock()
	defer d.cacheMutex.Unlock()

	if d.cache != nil && time.Since(d.cache.timestamp) < time.Duration(maxage)*time.Millisecond {
		return d.cache.data, nil
	}

	cli, err := client.NewClientWithOpts(client.WithHost("unix://" + dockerSock))
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client with Unix socket %s: %w", dockerSock, err)
	}

	ctx := context.Background()

	containers, err := cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	ipToNames := make(map[string][]string)

	for _, container := range containers {
		inspect, err := cli.ContainerInspect(ctx, container.ID)
		if err != nil {
			d.logger.Error("Failed to inspect container %s: %v", container.ID, err)
			continue
		}

		nameSet := make(map[string]bool)
		nameSet[container.ID[:12]] = true
		containerName := strings.TrimPrefix(inspect.Name, "/")
		nameSet[containerName] = true

		for _, network := range inspect.NetworkSettings.Networks {
			for _, dnsName := range network.DNSNames {
				nameSet[dnsName] = true
			}
		}

		// Convert the set of names to a slice
		var names []string
		for name := range nameSet {
			names = append(names, name)
		}

		// Get the primary IP address of the container (first network IP)
		for _, network := range inspect.NetworkSettings.Networks {
			if network.IPAddress != "" {
				ipToNames[network.IPAddress] = names
				break
			}
		}
	}

	d.cache = &DockerDNSCache{
		data:      ipToNames,
		timestamp: time.Now(),
	}

	return ipToNames, nil
}
