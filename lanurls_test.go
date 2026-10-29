package main

import (
	"net"
	"slices"
	"strings"
	"testing"
)

func fakeIfaces(t *testing.T, def string, list ...netIface) {
	t.Helper()
	oldList, oldDef := listIfaces, defaultRouteIface
	listIfaces = func() []netIface { return list }
	defaultRouteIface = func() string { return def }
	t.Cleanup(func() { listIfaces, defaultRouteIface = oldList, oldDef })
}

func iface(name string, ips ...string) netIface {
	f := netIface{Name: name}
	for _, s := range ips {
		f.Addrs = append(f.Addrs, net.ParseIP(s))
	}
	return f
}

func TestLanURLsSkipVirtualPutDefaultFirst(t *testing.T) {
	fakeIfaces(t, "wlo1",
		iface("lo", "127.0.0.1", "::1"),
		iface("docker0", "172.17.0.1"),
		iface("virbr0", "192.168.122.1"),
		iface("br-183d32953d24", "172.18.0.1"),
		iface("veth1a2b3c", "172.17.0.5"),
		iface("vnet0", "192.168.122.10"),
		iface("cni0", "10.88.0.1"),
		iface("flannel.1", "10.244.0.0"),
		iface("podman0", "10.89.0.1"),
		iface("enp3s0", "10.0.0.5", "fe80::1"),
		iface("tailscale0", "100.101.102.103"), // VPN: a real way in
		iface("wg0", "10.8.0.2"),
		iface("wlo1", "192.168.1.20"),
		iface("eth9", "203.0.113.7"), // public: never offered
	)
	got := lanURLs("8899")
	want := []string{"http://192.168.1.20:8899", "http://10.0.0.5:8899", "http://100.101.102.103:8899", "http://10.8.0.2:8899"}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

// Only bridges on the machine (a container host): better something than nothing.
func TestLanURLsFallBackToBridges(t *testing.T) {
	fakeIfaces(t, "", iface("docker0", "172.17.0.1"), iface("virbr0", "192.168.122.1"), iface("eth9", "203.0.113.7"))
	if got := lanURLs("8899"); !slices.Equal(got, []string{"http://172.17.0.1:8899", "http://192.168.122.1:8899"}) {
		t.Fatalf("%v", got)
	}
	fakeIfaces(t, "")
	if got := lanURLs("8899"); got == nil || len(got) != 0 {
		t.Fatalf("no addresses: %#v (want an empty list for JSON)", got)
	}
}

func TestLanURLsWithoutDefaultRouteKeepOrder(t *testing.T) {
	fakeIfaces(t, "", iface("wg0", "10.8.0.2"), iface("wlo1", "192.168.1.20"))
	if got := lanURLs("8899"); !slices.Equal(got, []string{"http://10.8.0.2:8899", "http://192.168.1.20:8899"}) {
		t.Fatalf("%v", got)
	}
}

func TestParseDefaultRoute(t *testing.T) {
	const table = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"wlo1\t00000000\t0101A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"docker0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
		"enp3s0\t00000000\t0100000A\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" + // lower metric wins
		"wg0\t00000000\t00000000\t0000\t0\t0\t50\t00000000\t0\t0\t0\n" // not up: ignored
	if got := parseDefaultRoute(strings.NewReader(table)); got != "enp3s0" {
		t.Fatalf("got %q", got)
	}
	if got := parseDefaultRoute(strings.NewReader("Iface\tDestination\n")); got != "" {
		t.Fatalf("no default route: %q", got)
	}
}
