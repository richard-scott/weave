package net

import (
	"fmt"
	"net"

	"github.com/j-keck/arping"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/weaveworks/weave/common/odp"
)

// create and attach local name to the Weave bridge
func CreateAndAttachVeth(localName, peerName, bridgeName string, mtu int, init func(local, guest netlink.Link) error) (*netlink.Veth, error) {
	maybeBridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return nil, fmt.Errorf(`bridge "%s" not present; did you launch weave?`, bridgeName)
	}

	if mtu == 0 {
		mtu = maybeBridge.Attrs().MTU
	}
	local := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name: localName,
			MTU:  mtu},
		PeerName: peerName,
	}
	if err := netlink.LinkAdd(local); err != nil {
		return nil, fmt.Errorf(`could not create veth pair %s-%s: %s`, local.Name, local.PeerName, err)
	}

	cleanup := func(format string, a ...interface{}) (*netlink.Veth, error) {
		netlink.LinkDel(local)
		return nil, fmt.Errorf(format, a...)
	}

	switch maybeBridge.(type) {
	case *netlink.Bridge:
		if err := netlink.LinkSetMasterByIndex(local, maybeBridge.Attrs().Index); err != nil {
			return cleanup(`unable to set master of %s: %s`, local.Name, err)
		}
	case *netlink.GenericLink:
		if maybeBridge.Type() != "openvswitch" {
			return cleanup(`device "%s" is of type "%s"`, bridgeName, maybeBridge.Type())
		}
		if err := odp.AddDatapathInterface(bridgeName, local.Name); err != nil {
			return cleanup(`failed to attach %s to device "%s": %s`, local.Name, bridgeName, err)
		}
	case *netlink.Device:
		// Assume it's our openvswitch device, and the kernel has not been updated to report the kind.
		if err := odp.AddDatapathInterface(bridgeName, local.Name); err != nil {
			return cleanup(`failed to attach %s to device "%s": %s`, local.Name, bridgeName, err)
		}
	default:
		return cleanup(`device "%s" is not a bridge`, bridgeName)
	}

	if init != nil {
		guest, err := netlink.LinkByName(peerName)
		if err != nil {
			return cleanup("unable to find guest veth %s: %s", peerName, err)
		}
		if err := init(local, guest); err != nil {
			return cleanup("initializing veth: %s", err)
		}
	}

	if err := netlink.LinkSetUp(local); err != nil {
		return cleanup("unable to bring veth up: %s", err)
	}

	return local, nil
}

func SetupGuest(guest netlink.Link, name string) error {
	var err error
	if err = netlink.LinkSetName(guest, name); err != nil {
		return err
	}
	if err = ConfigureARPCache(name); err != nil {
		return err
	}
	return nil
}

func AddAddresses(guest netlink.Link, cidrs []*net.IPNet) (newAddrs []*net.IPNet, err error) {
	existingAddrs, err := netlink.AddrList(guest, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("failed to get IP address for %q: %v", guest.Attrs().Name, err)
	}
	for _, ipnet := range cidrs {
		if contains(existingAddrs, ipnet) {
			continue
		}
		if err := netlink.AddrAdd(guest, &netlink.Addr{IPNet: ipnet}); err != nil {
			return nil, fmt.Errorf("failed to add IP address to %q: %v", guest.Attrs().Name, err)
		}
		newAddrs = append(newAddrs, ipnet)
	}
	return newAddrs, nil
}

func contains(addrs []netlink.Addr, addr *net.IPNet) bool {
	for _, x := range addrs {
		if addr.IP.Equal(x.IPNet.IP) {
			return true
		}
	}
	return false
}

func interfaceExistsInNamespace(ns netns.NsHandle, ifName string) bool {
	err := WithNetNS(ns, func() error {
		_, err := netlink.LinkByName(ifName)
		return err
	})
	return err == nil
}

func AttachContainer(ns netns.NsHandle, id, ifName, bridgeName string, mtu int, withMulticastRoute bool, cidrs []*net.IPNet) error {
	if !interfaceExistsInNamespace(ns, ifName) {
		if len(id) > 5 {
			id = id[:5]
		}
		name, peerName := "vethwepl"+id, "vethwg"+id
		_, err := CreateAndAttachVeth(name, peerName, bridgeName, mtu, func(local, guest netlink.Link) error {
			EthtoolTXOff(peerName) // TODO: do we want to do this under fastdp?
			if err := netlink.LinkSetNsFd(guest, int(ns)); err != nil {
				return fmt.Errorf("failed to move veth to container netns: %s", err)
			}
			if err := WithNetNS(ns, func() error {
				return SetupGuest(guest, ifName)
			}); err != nil {
				return fmt.Errorf("error setting up interface: %s", err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	if err := WithNetNSLink(ns, ifName, func(guest netlink.Link) error {
		newAddresses, err := AddAddresses(guest, cidrs)
		if err != nil {
			return err
		}
		if err := netlink.LinkSetUp(guest); err != nil {
			return err
		}
		for _, ipnet := range newAddresses {
			arping.GratuitousArpOverIfaceByName(ipnet.IP, ifName)
		}
		if withMulticastRoute {
			/* Route multicast packets across the weave network.
			This must come last in 'attach'. If you change this, change weavewait to match.

			TODO: Add the MTU lock to prevent PMTU discovery for multicast
			destinations. Without that, the kernel sets the DF flag on
			multicast packets. Since RFC1122 prohibits sending of ICMP
			errors for packets with multicast destinations, that causes
			packets larger than the PMTU to be dropped silently.  */

			_, multicast, _ := net.ParseCIDR("224.0.0.0/4")
			if err := AddRoute(guest, netlink.SCOPE_LINK, multicast, nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	return nil
}

func DetachContainer(ns netns.NsHandle, id, ifName string, cidrs []*net.IPNet) error {
	return WithNetNSLink(ns, ifName, func(guest netlink.Link) error {
		existingAddrs, err := netlink.AddrList(guest, netlink.FAMILY_V4)
		if err != nil {
			return fmt.Errorf("failed to get IP address for %q: %v", guest.Attrs().Name, err)
		}
		for _, ipnet := range cidrs {
			if !contains(existingAddrs, ipnet) {
				continue
			}
			if err := netlink.AddrDel(guest, &netlink.Addr{IPNet: ipnet}); err != nil {
				return fmt.Errorf("failed to remove IP address from %q: %v", guest.Attrs().Name, err)
			}
		}
		addrs, err := netlink.AddrList(guest, netlink.FAMILY_V4)
		if err != nil {
			return fmt.Errorf("failed to get IP address for %q: %v", guest.Attrs().Name, err)
		}
		if len(addrs) == 0 { // all addresses gone: remove the interface
			if err := netlink.LinkDel(guest); err != nil {
				return err
			}
		}
		return nil
	})
}