// Package netaddr finds the addresses other LAN devices can reach us on.
package netaddr

import (
	"net"
	"slices"
)

// LocalIPs returns the addresses a browser on the local network can reach us on.
func LocalIPs() []string {
	ips := privateIPv4s()
	if len(ips) == 0 {
		return routedIP()
	}
	// The address the default route would use is the most likely to work, so
	// print it first — but only if it survived the filtering in privateIPv4s.
	if primary := routedIP(); len(primary) == 1 {
		if i := slices.Index(ips, primary[0]); i > 0 {
			ips[0], ips[i] = ips[i], ips[0]
		}
	}
	return ips
}

// privateIPv4s lists the private IPv4 addresses of the usable interfaces.
// Interfaces that are down, loopback or point-to-point are skipped: the last
// group is VPN tunnels, whose addresses mean nothing to a device on the LAN.
func privateIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	const unusable = net.FlagLoopback | net.FlagPointToPoint
	var ips []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&unusable != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			net4, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ip := net4.IP.To4(); ip != nil && ip.IsPrivate() {
				ips = append(ips, ip.String())
			}
		}
	}
	return ips
}

// routedIP asks the routing table which address would be used to reach the
// internet. With a full-tunnel VPN up that is the tunnel itself, which no other
// device can connect to, so it is only a fallback and a hint about ordering.
func routedIP() []string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return []string{"<laptop-ip>"}
	}
	defer conn.Close()
	return []string{conn.LocalAddr().(*net.UDPAddr).IP.String()}
}
