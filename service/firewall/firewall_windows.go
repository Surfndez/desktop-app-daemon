package firewall

import (
	"errors"
	"fmt"
	"ivpn/daemon/netinfo"
	"ivpn/daemon/service/firewall/winlib"
	"ivpn/daemon/service/platform"
	"net"
	"os"
	"syscall"
)

var (
	providerKey = syscall.GUID{Data1: 0xfed0afd4, Data2: 0x98d4, Data3: 0x4233, Data4: [8]byte{0xa4, 0xf3, 0x8b, 0x7c, 0x02, 0x44, 0x50, 0x01}}
	sublayerKey = syscall.GUID{Data1: 0xfed0afd4, Data2: 0x98d4, Data3: 0x4233, Data4: [8]byte{0xa4, 0xf3, 0x8b, 0x7c, 0x02, 0x44, 0x50, 0x02}}

	v4Layers = []syscall.GUID{winlib.FwpmLayerAleAuthConnectV4, winlib.FwpmLayerAleAuthRecvAcceptV4}
	v6Layers = []syscall.GUID{winlib.FwpmLayerAleAuthConnectV6, winlib.FwpmLayerAleAuthRecvAcceptV6}

	manager                winlib.Manager
	clientLocalIPFilterIDs []uint64
	customDNS              net.IP

	isPersistant        bool
	isAllowLAN          bool
	isAllowLANMulticast bool
)

const (
	providerDName = "IVPN Kill Switch"
	sublayerDName = "IVPN Kill Switch Sub-Layer"
	filterDName   = "IVPN Kill Switch filter"
)

func init() {
	pInfo, err := manager.GetProviderInfo(providerKey)
	if err == nil {
		// save initial persistant state into package-variable
		isPersistant = pInfo.IsPersistent
	}
}

func implGetEnabled() (bool, error) {
	pInfo, err := manager.GetProviderInfo(providerKey)
	if err != nil {
		return false, fmt.Errorf("failed to get provider info: %w", err)
	}
	return pInfo.IsInstalled, nil
}

func implSetEnabled(isEnabled bool) (retErr error) {
	// start transaction
	if err := manager.TransactionStart(); err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	// do not forget to stop transaction
	defer func() {
		if r := recover(); r == nil {
			manager.TransactionCommit() // commit WFP transaction
		} else {
			manager.TransactionAbort() // abort WFPtransaction

			log.Error("PANIC (recovered): ", r)
			if e, ok := r.(error); ok {
				retErr = e
			} else {
				retErr = errors.New(fmt.Sprint(r))
			}
		}
	}()

	if isEnabled {
		return doEnable()
	}
	return doDisable()
}

func implSetPersistant(persistant bool) (retErr error) {
	// save persistent state
	isPersistant = persistant

	pinfo, err := manager.GetProviderInfo(providerKey)
	if err != nil {
		return fmt.Errorf("failed to get provider info: %w", err)
	}

	if pinfo.IsInstalled == true {
		if pinfo.IsPersistent == isPersistant {
			log.Info(fmt.Sprintf("Already enabled (persistent=%t).", isPersistant))
			return nil
		}

		log.Info(fmt.Sprintf("Re-enabling with persistent flag = %t", isPersistant))
		return reEnable()
	}

	return nil
}

// ClientConnected - allow communication for local vpn/client IP address
func implClientConnected(clientLocalIPAddress net.IP) (retErr error) {
	// start / commit transaction
	if err := manager.TransactionStart(); err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer func() {
		if retErr == nil {
			manager.TransactionCommit()
		} else {
			// abort transaction if there was an error
			manager.TransactionAbort()
		}
	}()

	err := doRemoveClientIPFilters()
	if err != nil {
		log.Error("Failed to remove previously defined client IP filters", err)
	}
	return doAddClientIPFilters(clientLocalIPAddress)
}

// ClientDisconnected - Disable communication for local vpn/client IP address
func implClientDisconnected() (retErr error) {
	// start / commit transaction
	if err := manager.TransactionStart(); err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer func() {
		if retErr == nil {
			manager.TransactionCommit()
		} else {
			// abort transaction if there was an error
			manager.TransactionAbort()
		}
	}()

	return doRemoveClientIPFilters()
}

func implAddHostsToExceptions(IPs []net.IP) error {
	// nothing to do for windows implementation
	return nil
}

// AllowLAN - allow/forbid LAN communication
func implAllowLAN(allowLan bool, allowLanMulticast bool) error {

	if isAllowLAN == allowLan && isAllowLANMulticast == allowLanMulticast {
		return nil
	}

	isAllowLAN = allowLan
	isAllowLANMulticast = allowLanMulticast

	if enabled, err := implGetEnabled(); enabled == false || err != nil {
		return fmt.Errorf("failed to get info if firewall is on: %w", err)
	}
	return reEnable()
}

// SetManualDNS - configure firewall to allow DNS which is out of VPN tunnel
// Applicable to Windows implementation (to allow custom DNS from local network)
func implSetManualDNS(addr net.IP) error {
	if addr.Equal(customDNS) {
		return nil
	}

	customDNS = addr

	if enabled, err := implGetEnabled(); enabled == false || err != nil {
		return fmt.Errorf("failed to get info if firewall is on: %w", err)
	}
	return reEnable()
}

func reEnable() (retErr error) {
	// start / commit transaction
	if err := manager.TransactionStart(); err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer func() {
		if retErr == nil {
			manager.TransactionCommit()
		} else {
			// abort transaction if there was an error
			manager.TransactionAbort()
		}
	}()

	err := doDisable()
	if err != nil {
		return fmt.Errorf("failed to disable firewall: %w", err)
	}

	err = doEnable()
	if err != nil {
		return fmt.Errorf("failed to enable firewall: %w", err)
	}

	return doAddClientIPFilters(connectedClientInterfaceIP)
}

func doEnable() (retErr error) {
	if enabled, err := implGetEnabled(); enabled == true || err != nil {
		return fmt.Errorf("failed to get info if firewall is on: %w", err)
	}

	addressesV6, err := netinfo.GetAllLocalV6Addresses()
	if err != nil {
		return fmt.Errorf("failed to get all local IPv6 addresses: %w", err)
	}
	addressesV4, err := netinfo.GetAllLocalV4Addresses()
	if err != nil {
		return fmt.Errorf("failed to get all local IPv4 addresses: %w", err)
	}

	provider := winlib.CreateProvider(providerKey, providerDName, "", isPersistant)
	sublayer := winlib.CreateSubLayer(sublayerKey, providerKey, sublayerDName, "", 2300, isPersistant)

	// add provider
	pinfo, err := manager.GetProviderInfo(providerKey)
	if err != nil {
		return fmt.Errorf("failed to get privuder info: %w", err)
	}
	if !pinfo.IsInstalled {
		if err = manager.AddProvider(provider); err != nil {
			return fmt.Errorf("failed to add provider : %w", err)
		}
	}

	// add sublayer
	installed, err := manager.IsSubLayerInstalled(sublayerKey)
	if err != nil {
		return fmt.Errorf("failed to check sublayer is installed: %w", err)
	}
	if !installed {
		if err = manager.AddSubLayer(sublayer); err != nil {
			return fmt.Errorf("failed to add sublayer: %w", err)
		}
	}

	// IPv6 filters
	for _, layer := range v6Layers {
		_, err := manager.AddFilter(winlib.NewFilterBlockAll(providerKey, layer, sublayerKey, filterDName, "", true, isPersistant))
		if err != nil {
			return fmt.Errorf("failed to add filter 'block all IPv6': %w", err)
		}

		if isAllowLAN {
			for _, ip := range addressesV6 {
				prefixLen, _ := ip.Mask.Size()
				_, err = manager.AddFilter(winlib.NewFilterAllowRemoteIPV6(providerKey, layer, sublayerKey, filterDName, "", ip.IP, byte(prefixLen), isPersistant))
				if err != nil {
					return fmt.Errorf("failed to add filter 'allow lan IPv6': %w", err)
				}
			}
		}
	}

	// IPv4 filters
	for _, layer := range v4Layers {
		// block all
		_, err := manager.AddFilter(winlib.NewFilterBlockAll(providerKey, layer, sublayerKey, filterDName, "", false, isPersistant))
		if err != nil {
			return fmt.Errorf("failed to add filter 'block all': %w", err)
		}

		// block DNS
		_, err = manager.AddFilter(winlib.NewFilterBlockDNS(providerKey, layer, sublayerKey, sublayerDName, "", customDNS, isPersistant))
		if err != nil {
			return fmt.Errorf("failed to add filter 'block dns': %w", err)
		}

		// allow DHCP port
		_, err = manager.AddFilter(winlib.NewFilterAllowLocalPort(providerKey, layer, sublayerKey, sublayerDName, "", 68, isPersistant))
		if err != nil {
			return fmt.Errorf("failed to add filter 'allow dhcp': %w", err)
		}

		// allow current executabe
		binaryPath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("failed to obtain executable info: %w", err)
		}
		_, err = manager.AddFilter(winlib.NewFilterAllowApplication(providerKey, layer, sublayerKey, sublayerDName, "", binaryPath, isPersistant))
		if err != nil {
			return fmt.Errorf("failed to add filter 'allow application': %w", err)
		}

		// allow OpenVPN executabe
		_, err = manager.AddFilter(winlib.NewFilterAllowApplication(providerKey, layer, sublayerKey, sublayerDName, "", platform.OpenVpnBinaryPath(), isPersistant))
		if err != nil {
			return fmt.Errorf("failed to add filter 'allow application - openvpn': %w", err)
		}
		// allow WireGuard executabe
		_, err = manager.AddFilter(winlib.NewFilterAllowApplication(providerKey, layer, sublayerKey, sublayerDName, "", platform.WgBinaryPath(), isPersistant))
		if err != nil {
			return fmt.Errorf("failed to add filter 'allow application - wireguard': %w", err)
		}
		// allow obfsproxy
		_, err = manager.AddFilter(winlib.NewFilterAllowApplication(providerKey, layer, sublayerKey, sublayerDName, "", platform.ObfsproxyStartScript(), isPersistant))
		if err != nil {
			return fmt.Errorf("failed to add filter 'allow application - obfsproxy': %w", err)
		}

		for _, ip := range addressesV4 {
			_, err = manager.AddFilter(winlib.NewFilterAllowRemoteIP(providerKey, layer, sublayerKey, filterDName, "", ip.IP, net.IPv4(255, 255, 255, 255), isPersistant))
			if err != nil {
				return fmt.Errorf("failed to add filter 'allow remote IP': %w", err)
			}

			if isAllowLAN {
				_, err = manager.AddFilter(winlib.NewFilterAllowRemoteIP(providerKey, layer, sublayerKey, filterDName, "", ip.IP, net.IP(ip.Mask), isPersistant))
				if err != nil {
					return fmt.Errorf("failed to add filter 'allow LAN': %w", err)
				}
			}
		}

		if isAllowLANMulticast {
			_, err = manager.AddFilter(winlib.NewFilterAllowRemoteIP(providerKey, layer, sublayerKey, filterDName, "",
				net.IPv4(224, 0, 0, 0), net.IPv4(240, 0, 0, 0), isPersistant))
			if err != nil {
				return fmt.Errorf("failed to add filter 'allow lan-multicast': %w", err)
			}
		}

		/*
			for ipStrKey := range allowedHosts {
				ip := net.ParseIP(ipStrKey)
				if ip == nil {
					continue
				}

				_, err = manager.AddFilter(winlib.NewFilterAllowRemoteIP(providerKey, layer, sublayerKey, filterDName, "",
					ip, net.IPv4(255, 255, 255, 255), isPersistant))
				if err != nil {
					return err
				}
			}*/
	}
	return nil
}

func doDisable() error {
	if enabled, err := implGetEnabled(); enabled == false || err != nil {
		return fmt.Errorf("failed to check if firewall enabled: %w", err)
	}

	// delete filters
	for _, l := range v6Layers {
		if err := manager.DeleteFilterByProviderKey(providerKey, l); err != nil {
			return fmt.Errorf("failed to delete filter : %w", err)
		}
	}

	for _, l := range v4Layers {
		if err := manager.DeleteFilterByProviderKey(providerKey, l); err != nil {
			return fmt.Errorf("failed to delete filter : %w", err)
		}
	}

	// delete sublayer
	installed, err := manager.IsSubLayerInstalled(sublayerKey)
	if err != nil {
		return fmt.Errorf("failed to check is sublayer installed : %w", err)
	}
	if installed {
		if err := manager.DeleteSubLayer(sublayerKey); err != nil {
			return fmt.Errorf("failed to delete sublayer : %w", err)
		}
	}

	// delete provider
	pinfo, err := manager.GetProviderInfo(providerKey)
	if err != nil {
		return fmt.Errorf("failed to get provider info : %w", err)
	}
	if pinfo.IsInstalled {
		if err := manager.DeleteProvider(providerKey); err != nil {
			return fmt.Errorf("failed to delete provider : %w", err)
		}
	}

	clientLocalIPFilterIDs = nil

	return nil
}

func doAddClientIPFilters(clientLocalIP net.IP) (retErr error) {
	if clientLocalIP == nil {
		return nil
	}

	if enabled, err := implGetEnabled(); enabled == false || err != nil {
		return fmt.Errorf("failed to get info is firewall enabled : %w", err)
	}

	filters := make([]uint64, 0, len(v4Layers))
	for _, layer := range v4Layers {
		f := winlib.NewFilterAllowLocalIP(providerKey, layer, sublayerKey, filterDName, "", clientLocalIP, net.IPv4(255, 255, 255, 255), false)
		id, err := manager.AddFilter(f)
		if err != nil {
			return fmt.Errorf("failed to add filter : %w", err)
		}
		filters = append(filters, id)
	}

	clientLocalIPFilterIDs = filters

	return nil
}

func doRemoveClientIPFilters() (retErr error) {
	defer func() {
		clientLocalIPFilterIDs = nil
	}()

	if enabled, err := implGetEnabled(); enabled == false || err != nil {
		return fmt.Errorf("failed to get info is firewall enabled : %w", err)
	}

	for _, filterID := range clientLocalIPFilterIDs {
		err := manager.DeleteFilterByID(filterID)
		if err != nil {
			return fmt.Errorf("failed to delete filter : %w", err)
		}
	}

	return nil
}
