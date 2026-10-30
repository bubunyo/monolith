package network

import (
	"fmt"
	"net/netip"
	"os"
	"regexp"

	mnet "github.com/bubunyo/monolith/net"

	"github.com/spf13/cobra"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

var (
	flagMode          string
	flagSubnet        string
	flagCreateVerbose bool
)

// networkCfg holds the parameters for creating a named network.
type networkCfg struct {
	Name    string
	Mode    string
	Subnet  string // single CIDR (e.g. 192.168.77.0/24); empty means let vmnet pick
	Verbose bool
}

var createCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a named network",
	Long: `Create a named network with a background daemon. VMs can join this network with --network <name>.

The --subnet flag accepts a single IPv4 CIDR (e.g. 192.168.77.0/24).
The requested subnet must not overlap any existing named network.
If --subnet is omitted, vmnet picks a subnet automatically.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return createNetwork(networkCfg{
			Name:    args[0],
			Mode:    flagMode,
			Subnet:  flagSubnet,
			Verbose: flagCreateVerbose,
		})
	},
}

// validateSubnet parses and validates a single user-provided CIDR. Returns
// the canonicalised (masked) prefix on success.
//
// Rules:
//   - Must be valid CIDR.
//   - Must be IPv4.
//   - Prefix length 16 <= bits <= 28.
//   - Must overlap an allowed RFC 1918 range.
//
// Only 192.168.0.0/16 is allowed for now: it is the range vmnet uses for its
// own automatic subnets and the only one exercised by the tests. vz v4 no
// longer restricts the range, so 10/8 and 172.16/12 can be enabled once they
// are verified against vmnet.
func validateSubnet(s string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid subnet %q: %w (expected CIDR like 192.168.50.0/24)", s, err)
	}
	if !prefix.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("invalid subnet %q: only IPv4 is supported", s)
	}
	if prefix.Bits() < 16 || prefix.Bits() > 28 {
		return netip.Prefix{}, fmt.Errorf("invalid subnet %q: prefix length must be between /16 and /28 (got /%d)", s, prefix.Bits())
	}

	allowedRanges := []netip.Prefix{
		netip.MustParsePrefix("192.168.0.0/16"),
		// Not yet verified with vmnet:
		// netip.MustParsePrefix("10.0.0.0/8"),
		// netip.MustParsePrefix("172.16.0.0/12"),
	}
	for _, r := range allowedRanges {
		if r.Overlaps(prefix) {
			return prefix.Masked(), nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("invalid subnet %q: must be in an allowed RFC 1918 range (currently 192.168.0.0/16 only)", s)
}

// existingSubnets returns the set of subnets currently in use by other
// named networks (excluding the one being created), along with the network
// name each subnet belongs to. It looks at both the requested UserSubnet
// and the actual Subnet so that running and stopped networks both count.
func existingSubnets(excludeName string) (map[string]netip.Prefix, error) {
	dir, err := mnet.NetworksDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make(map[string]netip.Prefix)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == excludeName {
			continue
		}
		cfg, err := mnet.ReadNetworkConfig(e.Name())
		if err != nil {
			continue
		}
		// Prefer the actual subnet (running daemon has populated it);
		// fall back to the requested one for stopped networks.
		for _, s := range []string{cfg.Subnet, cfg.UserSubnet} {
			if s == "" {
				continue
			}
			if p, err := netip.ParsePrefix(s); err == nil {
				out[e.Name()] = p.Masked()
				break
			}
		}
	}
	return out, nil
}

// createNetwork validates, persists, and starts a new named network.
func createNetwork(cfg networkCfg) error {
	if len(cfg.Name) > 63 {
		return fmt.Errorf("network name %q is too long (max 63 characters)", cfg.Name)
	}
	if !namePattern.MatchString(cfg.Name) {
		return fmt.Errorf("network name %q is invalid (must be lowercase alphanumeric with hyphens)", cfg.Name)
	}

	if cfg.Mode != mnet.NetworkShared && cfg.Mode != mnet.NetworkIsolated {
		return fmt.Errorf("named networks only support shared or isolated modes, got %q", cfg.Mode)
	}

	var userSubnet string
	if cfg.Subnet != "" {
		want, err := validateSubnet(cfg.Subnet)
		if err != nil {
			return err
		}
		existing, err := existingSubnets(cfg.Name)
		if err != nil {
			return fmt.Errorf("checking existing networks: %w", err)
		}
		for name, ex := range existing {
			if want.Overlaps(ex) {
				return fmt.Errorf("requested subnet %s overlaps existing network %q (subnet %s)", want, name, ex)
			}
		}
		userSubnet = want.String()
	}

	if mnet.IsNetworkDaemonAlive(cfg.Name) {
		return fmt.Errorf("network %q already exists and is running", cfg.Name)
	}

	dir, err := mnet.NetworkDir(cfg.Name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("network %q already exists (use 'network start' to restart it)", cfg.Name)
	}

	netCfg := mnet.NetworkConfig{
		Mode:       cfg.Mode,
		UserSubnet: userSubnet,
	}
	if err := mnet.WriteNetworkConfig(cfg.Name, netCfg); err != nil {
		return fmt.Errorf("writing network config: %w", err)
	}

	if err := spawnDaemon(cfg.Name, cfg.Mode, cfg.Verbose); err != nil {
		// Daemon failed to start; clean up the half-written config dir
		// so the user can re-attempt with `network create`.
		os.RemoveAll(dir)
		return err
	}

	netCfg, err = mnet.ReadNetworkConfig(cfg.Name)
	if err != nil {
		fmt.Printf("Network %q created (mode: %s)\n", cfg.Name, cfg.Mode)
	} else {
		fmt.Printf("Network %q created (mode: %s, subnet: %s)\n", cfg.Name, netCfg.Mode, netCfg.Subnet)
	}

	return nil
}

func init() {
	createCmd.Flags().StringVar(&flagMode, "mode", mnet.NetworkShared, `network mode: shared or isolated (default: shared)`)
	createCmd.Flags().StringVar(&flagSubnet, "subnet", "", `IPv4 subnet in CIDR (e.g. 192.168.77.0/24); must not overlap an existing named network`)
	createCmd.Flags().BoolVarP(&flagCreateVerbose, "verbose", "v", false, "enable packet-level logging in daemon")
}
