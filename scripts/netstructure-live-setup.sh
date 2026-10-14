#!/bin/sh
# 在 privileged 容器里现建结构分类涉及的各类网卡，再用 agent 的采集代码读真实 sysfs 核对。期望在 internal/agent/collect 的 liveWant。实测条件：内核 7.0.14-orbstack 的 privileged 容器，容器架构与本机一致。
# need 的网卡只依赖 dummy、网桥、veth、tun/tap、VLAN、bond、macvlan、vxlan，合起来覆盖四类排除原因与计入类：
# 建不出说明环境缺件，立即失败并留下 ip 的报错，门禁不能算过。try 的网卡依赖不一定编进内核的模块（ipvlan、
# geneve、WireGuard、ipip、sit、gre、ip6tnl、ip6gre），建不出记下退出码与报错首行后跳过，只核对建成的。
# 期望表在 netstructure_live_test.go。
set -eu
apk add -q iproute2 >/dev/null
created="eth0"
log=$(mktemp)
trap 'rm -f "$log"' EXIT
need() {
  name=$1; shift
  if "$@" 2>"$log"; then created="$created $name"; else rc=$?; echo "FAIL $name (rc=$rc): $*"; cat "$log"; exit 1; fi
}
try() {
  name=$1; shift
  if "$@" 2>"$log"; then created="$created $name"; else rc=$?; echo "skip $name (rc=$rc): $(head -n 1 "$log")"; fi
}
need d1 ip link add d1 type dummy
need d2 ip link add d2 type dummy
need d3 ip link add d3 type dummy
need br0 ip link add br0 type bridge
need d0 sh -c 'ip link add d0 type dummy && ip link set d0 master br0'
need tapb sh -c 'ip tuntap add tapb mode tap && ip link set tapb master br0'
need va sh -c 'ip link add va type veth peer name vb && ip link set va master br0'
created="$created vb"
need d1.100 ip link add link d1 name d1.100 type vlan id 100
need bond0 sh -c 'ip link add bond0 type bond && ip link set d2 master bond0'
need mv0 ip link add mv0 link d1 type macvlan mode bridge
need vxd ip link add vxd type vxlan id 42 dstport 4789 dev d1
need vxn ip link add vxn type vxlan id 43 dstport 4790 local 10.9.9.9
need tun0 ip tuntap add tun0 mode tun
try ipv0 ip link add ipv0 link d3 type ipvlan mode l2
try gnv0 ip link add gnv0 type geneve id 7 remote 10.1.1.1
try wg0 ip link add wg0 type wireguard
try ipip0 ip link add ipip0 type ipip remote 10.1.1.1 local 10.2.2.2
try sit1 ip link add sit1 type sit remote 10.1.1.1 local 10.2.2.2
try gre1 ip link add gre1 type gre remote 10.1.1.1 local 10.2.2.2
try ip6t0 ip link add ip6t0 type ip6tnl remote fd00::1 local fd00::2
try ip6g0 ip link add ip6g0 type ip6gre remote fd00::1 local fd00::2
echo "kernel $(uname -r) $(uname -m); created:$created"
HERON_LIVE_IFACES="$created" /w/collect.test -test.run '^TestLiveNetStructure$' -test.count=1 -test.v
# 移进另一个网络命名空间的 macvlan 与 VLAN：在那里没有 lower_*，照常计入。ip netns exec 为该命名空间重新挂载 sysfs。
need ns1 ip netns add ns1
need mvx sh -c 'ip link add mvx link d1 type macvlan mode bridge && ip link set mvx netns ns1'
need d1.200 sh -c 'ip link add link d1 name d1.200 type vlan id 200 && ip link set d1.200 netns ns1'
ip netns exec ns1 env HERON_LIVE_IFACES="mvx d1.200" /w/collect.test -test.run '^TestLiveNetStructure$' -test.count=1 -test.v
