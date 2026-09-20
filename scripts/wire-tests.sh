#!/usr/bin/env bash
# The three tests that put a real advert on a real multicast group, run somewhere it is nobody's
# business but ours.
#
#   internal/discovery  TestAdvertReachesTheWireAndIsWithdrawn
#   internal/discovery  TestNeighboursSeesAnotherCoordinatorAndNotItself
#   cmd/briard-tether   TestTheAdvertLeadsAClientToTheRadio
#
# ⚠️ **Why they are not in `go test ./...`.** The last one publishes
# `_zigbee-coordinator._tcp.local.` — the service type out of ZHA's own manifest — on every
# multicast-capable interface the machine has. Run it on a home network and Home Assistant offers
# a discovery card for a coordinator that exists for four seconds. Asking only whether the group
# can be bound is not a guard: it skips in sandboxes, which have nothing to pollute, and runs at
# full volume where it can do harm. `requireMulticast` is opt-in, and this script is what opts in.
#
# **The namespace is a veth pair, and that is the whole trick.** A network namespace with only
# loopback in it is not enough: tether refuses to advertise on a loopback interface on purpose
# (`advertisable`), so there is nothing left to announce on — which is also why
# `tests/run_gate.into_a_private_network`'s loopback-only namespace can carry one advert and not
# two. A veth pair is a real segment with the MULTICAST flag, a real address, and nothing else
# on it.
#
# **No root.** A user namespace carries CAP_NET_ADMIN over the network namespace it owns, and
# that is enough to create a veth pair with *both ends inside* it. (What needs root is putting one
# end on a bridge the host owns, which is a change to the host — see `OnTheGuestsSegment` in
# `tests/run_gate.py`, which does need it and says so.)
set -euo pipefail

cd "$(dirname "$0")/.."

# ⚠️ **The one way out of the namespace, and it is not for a developer's machine.**
# `unshare --user` needs unprivileged user namespaces, and a host can refuse: GitHub's runners
# answer `write failed /proc/self/uid_map: Operation not permitted`. A hosted runner is a
# throwaway VM with nobody's Home Assistant on it, so announcing on its own network costs
# nothing — but that is a fact about *that* machine, so it has to be asserted by whoever runs it
# rather than guessed at here. CI sets this; nothing else should.
if [ "${TETHER_WIRE_HOST_NETWORK:-}" = "1" ]; then
    echo "⚠️  TETHER_WIRE_HOST_NETWORK=1: announcing on this machine's own network, not a"
    echo "    namespace. Correct only where nothing else is listening for a coordinator."
    export TETHER_WIRE_TESTS=1
    exec go test -count=1 -run 'TestAdvertReachesTheWireAndIsWithdrawn|TestNeighboursSeesAnotherCoordinatorAndNotItself|TestTheAdvertLeadsAClientToTheRadio' \
        ./internal/discovery ./cmd/briard-tether "$@"
fi

if [ "${TETHER_WIRE_NS:-}" != "1" ]; then
    # Build outside the namespace: the Go toolchain comes from the nix store and the module cache
    # is in $HOME, both of which are fine in there, but a network-less namespace cannot fetch and
    # a first build that needed to would fail for a reason that has nothing to do with mDNS.
    go build ./... >/dev/null
    if ! unshare --user --map-root-user --net true 2>/dev/null; then
        echo "this machine will not give an unprivileged user namespace, so these tests have" >&2
        echo "nowhere private to announce on. Two ways on:" >&2
        echo "  · enable unprivileged user namespaces (on Ubuntu 24.04:" >&2
        echo "    sysctl kernel.apparmor_restrict_unprivileged_userns=0), or" >&2
        echo "  · TETHER_WIRE_HOST_NETWORK=1 $0 — only where nothing on the network is" >&2
        echo "    listening for a Zigbee coordinator, which is not a home LAN." >&2
        exit 1
    fi
    export TETHER_WIRE_NS=1
    exec unshare --user --map-root-user --net -- "$0" "$@"
fi

# Inside. Loopback for anything that talks to itself, then the segment the adverts live on.
ip link set lo up
ip link add wire0 type veth peer name wire1
ip addr add 10.86.0.1/24 dev wire0
ip addr add 10.86.0.2/24 dev wire1
ip link set wire0 up
ip link set wire1 up
# The far end carries frames but must not be advertisable: `advertisable` keeps anything Up and
# Multicast, so leaving the flag on announces two addresses for one tether — which is the coin
# flip a second advertised coordinator produces, reproduced inside our own rig.
ip link set wire1 multicast off
# Without a route for the group there is nowhere to send to, and the failure is an obscure
# "network is unreachable" from inside the responder rather than anything about mDNS.
ip route add 224.0.0.0/4 dev wire0

export TETHER_WIRE_TESTS=1
echo "── the wire tests, on a segment with nothing else on it ──"
ip -brief addr show | sed 's/^/  /'
echo
exec go test -count=1 -run 'TestAdvertReachesTheWireAndIsWithdrawn|TestNeighboursSeesAnotherCoordinatorAndNotItself|TestTheAdvertLeadsAClientToTheRadio' \
    ./internal/discovery ./cmd/briard-tether "$@"
