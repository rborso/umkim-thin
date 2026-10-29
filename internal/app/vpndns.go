package app

import (
	"fmt"
	"sort"
	"strings"
)

// vpnDNSPairs returns the public host -> gateway mappings for every installed
// route that has opted into same-host VPN access. The DNS front on the
// WireGuard gateway reads this list; it deliberately contains app hosts only,
// never auth/console/home.
func (in *Installer) vpnDNSPairs() []string {
	if !in.cfg.TrustedAccessEnabled {
		return nil
	}
	c := in.loadContributions()
	seen := map[string]bool{}
	var hosts []string
	for _, name := range sortedKeys(c) {
		for _, rs := range c[name].Routes {
			host := rs.VPNHost
			if host == "" {
				host = rs.Host
			}
			if !rs.TrustedVPN || host == "" {
				continue
			}
			if seen[host] {
				continue
			}
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	sort.Strings(hosts)
	return hosts
}

// syncVPNDNS publishes the split-DNS host list to the WireGuard gateway. It is
// best-effort when WireGuard is not installed: there is nothing to configure
// until that release exists.
func (in *Installer) syncVPNDNS() error {
	if in.cfg.WGNamespace == "" || in.cfg.WGVPNDNSConfigMap == "" {
		return nil
	}
	if _, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"get", "namespace", in.cfg.WGNamespace, "-o", "name"); err != nil {
		return nil
	}
	if _, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", in.cfg.WGNamespace, "get", "configmap", in.cfg.WGVPNDNSConfigMap, "-o", "name"); err != nil {
		// The chart creates the empty ConfigMap. If it is absent, WireGuard has
		// not been installed through this release; do not create a stray object.
		return nil
	}
	if err := in.apply(vpnDNSConfigMap(in.cfg.WGVPNDNSConfigMap, in.cfg.WGNamespace,
		in.vpnDNSPairs(), in.cfg.VPNServerIP)); err != nil {
		return err
	}
	// Restart dnsmasq in each host-network gateway pod. An in-process SIGHUP
	// does not reliably reload newly added host-record entries on this image;
	// an explicit kill/start does. This also keeps the DNS view and the
	// gateway's resolved Traefik pod IP coherent.
	pods, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", in.cfg.WGNamespace,
		"get", "pod", "-l", "app=wireguard-gateway", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil || strings.TrimSpace(pods) == "" {
		return nil
	}
	for _, pod := range strings.Fields(pods) {
		_, _ = in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", in.cfg.WGNamespace,
			"exec", pod, "--", "sh", "-c",
			"if [ -s /run/dnsmasq.pid ]; then kill -9 \"$(cat /run/dnsmasq.pid)\" 2>/dev/null || true; sleep 2; fi; "+
				"dnsmasq --conf-file=/etc/dnsmasq.conf --pid-file=/run/dnsmasq.pid 2>/dev/null || true")
	}
	// The dedicated VPN DNS pod reads the ConfigMap at startup; restart it so
	// newly installed apps are visible immediately.
	_, _ = in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", in.cfg.WGNamespace,
		"rollout", "restart", "deployment/vpn-dns")
	return nil
}

func vpnDNSConfigMap(name, namespace string, hosts []string, serverIP string) string {
	if serverIP == "" {
		serverIP = "192.168.168.1"
	}
	var data string
	if len(hosts) > 0 {
		var b strings.Builder
		for _, host := range hosts {
			fmt.Fprintf(&b, "%s %s\n", host, serverIP)
		}
		data = fmt.Sprintf("  hosts: |\n%s", indent(strings.TrimSuffix(b.String(), "\n"), 4))
	} else {
		data = "  hosts: \"\"\n"
	}
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
%s`, name, namespace, data)
}

func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}
