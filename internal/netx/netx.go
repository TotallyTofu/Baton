// Package netx lets baton answer on this PC's Tailscale address as well as
// loopback, for Tailscale devices only (your phone over the tailnet), never
// for the LAN.
package netx

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	_, cgnat, _ = net.ParseCIDR("100.64.0.0/10")       // Tailscale IPv4
	_, tsULA, _ = net.ParseCIDR("fd7a:115c:a1e0::/48") // Tailscale IPv6
	trustedKey  = struct{ name string }{"tailnet"}
)

// TailscaleIP returns this PC's Tailscale IPv4 address, if Tailscale is up.
func TailscaleIP() net.IP {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var fallback net.IP
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || !cgnat.Contains(ipn.IP) {
				continue
			}
			if strings.Contains(strings.ToLower(ifc.Name), "tailscale") {
				return ipn.IP.To4()
			}
			fallback = ipn.IP.To4()
		}
	}
	return fallback
}

func fromTailnet(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && (cgnat.Contains(ip) || tsULA.Contains(ip) || ip.IsLoopback())
}

// hostOK accepts the names a tailnet device uses to reach this PC: its
// Tailscale address, its MagicDNS name, or its machine name. Anything else
// (a DNS-rebinding page) is refused.
func hostOK(host string, ip net.IP) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	h = strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
	if ip != nil && h == ip.String() {
		return true
	}
	name, _ := os.Hostname()
	name = strings.ToLower(name)
	return name != "" && (h == name || strings.HasPrefix(h, name+".") && strings.HasSuffix(h, ".ts.net"))
}

// Trusted reports whether r arrived on the Tailscale listener from a
// Tailscale device with an acceptable Host.
func Trusted(r *http.Request) bool {
	v, _ := r.Context().Value(trustedKey).(bool)
	return v
}

func guard(h http.Handler, ip net.IP) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fromTailnet(r.RemoteAddr) {
			http.Error(w, "only Tailscale devices may connect", http.StatusForbidden)
			return
		}
		if !hostOK(r.Host, ip) {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), trustedKey, true)))
	})
}

// Remote keeps listeners open on the Tailscale address, following it as
// Tailscale comes and goes.
type Remote struct {
	mu      sync.Mutex
	ip      net.IP
	servers []*http.Server
	err     string
	targets []target
	onLog   func(format string, args ...any)
}

type target struct {
	port string
	h    http.Handler
}

func NewRemote(log func(string, ...any)) *Remote { return &Remote{onLog: log} }

// Add serves h on the Tailscale address at the port of loopbackAddr.
func (r *Remote) Add(loopbackAddr string, h http.Handler) {
	_, port, err := net.SplitHostPort(loopbackAddr)
	if err != nil {
		return
	}
	r.targets = append(r.targets, target{port, h})
}

// Run checks the Tailscale address every 20 s until ctx ends.
func (r *Remote) Run(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		r.sync()
		select {
		case <-ctx.Done():
			r.closeAll()
			return
		case <-t.C:
		}
	}
}

func (r *Remote) sync() {
	ip := TailscaleIP()
	r.mu.Lock()
	defer r.mu.Unlock()
	if ip.Equal(r.ip) && (ip == nil || len(r.servers) > 0) {
		return
	}
	r.closeAllLocked()
	r.ip = ip
	if ip == nil {
		r.err = "Tailscale is not running"
		return
	}
	r.err = ""
	for _, t := range r.targets {
		addr := net.JoinHostPort(ip.String(), t.port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			r.err = err.Error()
			r.onLog("cannot listen on %s: %v", addr, err)
			continue
		}
		srv := &http.Server{Handler: guard(t.h, ip), ReadHeaderTimeout: 10 * time.Second}
		r.servers = append(r.servers, srv)
		go func() { _ = srv.Serve(ln) }()
	}
	if len(r.servers) > 0 {
		r.onLog("listening on Tailscale address %s for Tailscale devices", ip)
	}
}

func (r *Remote) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeAllLocked()
}

func (r *Remote) closeAllLocked() {
	for _, s := range r.servers {
		_ = s.Close()
	}
	r.servers = nil
}

// Status is the address phones should use, or why there is none.
func (r *Remote) Status() (ip string, errText string) {
	if r == nil {
		return "", ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ip != nil {
		ip = r.ip.String()
	}
	return ip, r.err
}
