// Package config parses config.yaml (same keys as the C vde_plug) and the
// VNL parameter string UML appends after "slirp://".
//
// Defaults match upstream libslirp so existing setups keep working:
// 10.0.2.0/24, gateway .2, DNS .3, DHCP from .15.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Forward is one port forwarding rule.
type Forward struct {
	UDP       bool
	HostIP    string // listen address on the host, "" = all interfaces
	HostPort  uint16
	GuestIP   string // explicit guest address, "" = fallback rule
	GuestPort uint16
	// LastOctet is set for ".N" shorthand rules; completed against Network.
	LastOctet int
}

// Config mirrors the C vde_plug's config.yaml keys.
type Config struct {
	IPv4       bool `yaml:"ipv4"`
	IPv6       bool `yaml:"ipv6"`
	Switch     *bool  `yaml:"switch"` // nil = default true
	Socket     string `yaml:"socket"`
	SocketMode uint32 `yaml:"socket_mode"`
	Uplink     string `yaml:"uplink"` // slirp | tap:NAME | none

	Network    string `yaml:"network"`
	Gateway    string `yaml:"gateway"`
	Nameserver string `yaml:"nameserver"`
	DHCPStart  string `yaml:"dhcp_start"`
	Hostname   string `yaml:"hostname"`
	MTU        int    `yaml:"mtu"`

	PortFwd bool     `yaml:"portfwd"`
	Ports   []string `yaml:"ports"`

	Batch int `yaml:"batch"`
}

// Defaults returns the C binary's default configuration.
func Defaults() *Config {
	return &Config{
		IPv4:       true,
		Network:    "10.0.2.0/24",
		Gateway:    "10.0.2.2",
		Nameserver: "10.0.2.3",
		DHCPStart:  "10.0.2.15",
		Uplink:     "slirp",
		Batch:      16,
	}
}

// Load reads config.yaml; missing file means defaults. Unknown keys are
// ignored (including `engine:`, consumed by ./boot).
func Load(path string) (*Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// SwitchEnabled reports the effective switch mode.
func (c *Config) SwitchEnabled() bool {
	return c.Switch == nil || *c.Switch
}

// ParsePorts converts the raw "ports:" entries. Shapes:
//
//	HOST:GUEST         fallback rule (follows the first client)
//	ADDR:HOST:GUEST    pinned rule; ADDR is an IP or ".N" shorthand
func (c *Config) ParsePorts() ([]Forward, error) {
	var out []Forward
	for _, raw := range c.Ports {
		spec := strings.TrimSpace(raw)
		f := Forward{}
		switch {
		case strings.HasPrefix(spec, "udp "):
			f.UDP, spec = true, strings.TrimSpace(spec[4:])
		case strings.HasPrefix(spec, "tcp "):
			spec = strings.TrimSpace(spec[4:])
		}
		parts := strings.Split(spec, ":")
		var err error
		switch len(parts) {
		case 2: // HOST:GUEST
			f.HostPort, f.GuestPort, err = twoPorts(parts[0], parts[1])
		case 3: // ADDR:HOST:GUEST
			addr := strings.TrimSpace(parts[0])
			if strings.HasPrefix(addr, ".") {
				f.LastOctet, err = strconv.Atoi(addr[1:])
				if err == nil && (f.LastOctet < 1 || f.LastOctet > 254) {
					err = fmt.Errorf("last octet %d out of range", f.LastOctet)
				}
			} else if net.ParseIP(addr) == nil {
				err = fmt.Errorf("bad address %q", addr)
			} else {
				f.GuestIP = addr
			}
			if err == nil {
				f.HostPort, f.GuestPort, err = twoPorts(parts[1], parts[2])
			}
		default:
			err = fmt.Errorf("malformed port entry %q", raw)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

func twoPorts(host, guest string) (uint16, uint16, error) {
	hp, err1 := strconv.ParseUint(strings.TrimSpace(host), 10, 16)
	gp, err2 := strconv.ParseUint(strings.TrimSpace(guest), 10, 16)
	if err1 != nil || err2 != nil || hp == 0 || hp > 65535 || gp == 0 || gp > 65535 {
		return 0, 0, fmt.Errorf("bad ports %q:%q", host, guest)
	}
	return uint16(hp), uint16(gp), nil
}

// ApplyVNL applies "key=val/key=val" parameters from the vnl string;
// they override the config file, matching the C binary.
func (c *Config) ApplyVNL(params string) {
	if params == "" {
		return
	}
	for _, tok := range strings.Split(params, "/") {
		if tok == "" {
			continue
		}
		eq := strings.IndexByte(tok, '=')
		if eq < 0 {
			switch tok {
			case "switch":
				on := true
				c.Switch = &on
			case "noswitch":
				off := false
				c.Switch = &off
			}
			continue
		}
		k, v := tok[:eq], tok[eq+1:]
		switch k {
		case "v6":
			c.IPv6 = v == "1" || v == "true"
		case "mtu":
			if n, err := strconv.Atoi(v); err == nil {
				c.MTU = n
			}
		case "switch":
			on := v == "1" || v == "true" || v == "yes"
			c.Switch = &on
		case "socket":
			c.Socket = v
		case "uplink":
			c.Uplink = v
		case "dhcp_start":
			c.DHCPStart = v
		case "network":
			c.Network = v
		case "gateway", "host", "addr":
			c.Gateway = strings.SplitN(v, "/", 2)[0]
		case "batch":
			if n, err := strconv.Atoi(v); err == nil {
				c.Batch = n
			}
		}
	}
}
