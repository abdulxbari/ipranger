package ipranger

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"sync/atomic"

	"github.com/projectdiscovery/hmap/store/hybrid"
	"github.com/projectdiscovery/iputil"
	"github.com/projectdiscovery/mapcidr"
	"github.com/projectdiscovery/networkpolicy"
	"github.com/yl2chen/cidranger"
)

type IPRanger struct {
	Np                *networkpolicy.NetworkPolicy
	iprangerop        cidranger.Ranger
	Hosts             *hybrid.HybridMap
	Stats             Stats
	CoalescedHostList []*net.IPNet
}

func New() (*IPRanger, error) {
	hm, err := hybrid.New(hybrid.DefaultDiskOptions)
	if err != nil {
		return nil, err
	}
	var np networkpolicy.NetworkPolicy

	return &IPRanger{Np: &np, iprangerop: cidranger.NewPCTrieRanger(), Hosts: hm}, nil
}

func (ir *IPRanger) Contains(host string) bool {
	// ip => check internal ip ranger
	if iputil.IsIP(host) {
		if ok, err := ir.iprangerop.Contains(net.ParseIP(host)); err == nil {
			return ok
		}
	}

	// fqdn, cidr => check hmap
	_, ok := ir.Hosts.Get(host)
	return ok
}

func (ir *IPRanger) Add(host string) error {
	// skip already contained
	if ir.Contains(host) {
		return errors.New("host already added")
	}

	// ips: valid + new => add
	if iputil.IsIP(host) {
		if ir.Np.Validate(host) {
			return ir.add(host)
		}
		return errors.New("invalid ip")
	}

	// cidrs => add
	if iputil.IsCIDR(host) {
		return ir.add(host)
	}

	return errors.New("only ip/cidr can be added")
}

func (ir *IPRanger) add(host string) error {
	if iputil.IsIPv4(host) {
		host = iputil.AsIPV6CIDR(host)
	}
	_, network, err := net.ParseCIDR(host)
	if err != nil {
		return err
	}

	atomic.AddUint64(&ir.Stats.IPS, mapcidr.AddressCountIpnet(network))

	return ir.iprangerop.Insert(cidranger.NewBasicRangerEntry(*network))
}

func (ir *IPRanger) IsValid(host string) bool {
	return ir.Np.Validate(host)
}

func (ir *IPRanger) Delete(host string) error {
	// if it's an ip convert it to cidr representation
	if iputil.IsIP(host) || iputil.IsCIDR(host) {
		return ir.delete(host)
	}

	return errors.New("only ip or cidr supported")
}

func (ir *IPRanger) delete(host string) error {
	if iputil.IsIPv4(host) {
		host = iputil.AsIPV6CIDR(host)
	}
	_, network, err := net.ParseCIDR(host)
	if err != nil {
		return err
	}

	atomic.AddUint64(&ir.Stats.IPS, -mapcidr.AddressCountIpnet(network))
	_, err = ir.iprangerop.Remove(*network)

	return err
}

func (ir *IPRanger) AddHostWithMetadata(host, metadata string) error {
	if !ir.IsValid(host) {
		return errors.New("invalid host with metadata")
	}
	// cache ip/cidr
	ir.Add(host)
	// dedupe all the hosts and also keep track of ip => host for the output - just append new hostname
	if data, ok := ir.Hosts.Get(host); ok {
		// check if fqdn not contained
		if !bytes.Contains(data, []byte(metadata)) {
			hosts := strings.Split(string(data), ",")
			hosts = append(hosts, metadata)
			atomic.AddUint64(&ir.Stats.Hosts, 1)
			return ir.Hosts.Set(host, []byte(strings.Join(hosts, ",")))
		}
		// host already contained
		return nil
	}

	atomic.AddUint64(&ir.Stats.Hosts, 1)
	return ir.Hosts.Set(host, []byte(metadata))
}

func (ir *IPRanger) HasIP(IP string) bool {
	_, ok := ir.Hosts.Get(IP)
	return ok
}

func (ir *IPRanger) GetHostsByIP(IP string) ([]string, error) {
	dt, ok := ir.Hosts.Get(IP)
	if ok {
		return strings.Split(string(dt), ","), nil
	}

	// if not found return the ip
	return []string{IP}, nil
}

func (ir *IPRanger) Close() error {
	return ir.Hosts.Close()
}

func (ir *IPRanger) Shrink() error {
	// shrink all the cidrs and ips (ipv4)
	var items []*net.IPNet
	ir.Hosts.Scan(func(item, _ []byte) error {
		items = append(items, iputil.AsIPV4IpNet(string(item)))
		return nil
	})
	ir.CoalescedHostList, _ = mapcidr.CoalesceCIDRs(items)
	// reset the internal ranger with the new data
	ir.iprangerop = cidranger.NewPCTrieRanger()
	atomic.StoreUint64(&ir.Stats.IPS, 0)
	for _, item := range ir.CoalescedHostList {
		err := ir.iprangerop.Insert(cidranger.NewBasicRangerEntry(*item))
		if err != nil {
			return err
		}
		atomic.AddUint64(&ir.Stats.IPS, mapcidr.AddressCountIpnet(item))
	}
	return nil
}
