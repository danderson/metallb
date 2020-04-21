---
title: Configuration
weight: 4
---

MetalLB remains idle until configured. This is accomplished by
creating and deploying a config map into the same namespace
(metallb-system) as the deployment.

There is an example config map in
[`manifests/example-config.yaml`](https://raw.githubusercontent.com/metallb/metallb/main/manifests/example-config.yaml),
annotated with explanatory comments.

If you've named the config map `config.yaml`, you can deploy the manifest with `kubectl apply -f config.yaml`.

{{% notice note %}}
If you installed MetalLB with Helm, you will need to change the
namespace of the config map to match the namespace in which MetalLB was
deployed, and change the name of the config map from `config` to
`metallb-config`.
{{% /notice %}}

The specific configuration depends on the protocol(s) you want to use
to announce service IPs. Jump to:

- [Layer 2 configuration](#layer-2-configuration)
- [BGP configuration](#bgp-configuration)
- [Advanced configuration](#advanced-address-pool-configuration)

## Layer 2 configuration

Layer 2 mode is the simplest to configure: in many cases, you don't
need any protocol-specific configuration, only IP addresses.

Layer 2 mode does not require the IPs to be bound to the network interfaces
of your worker nodes. It works by responding to ARP requests on your local
network directly, to give the machine's MAC address to clients.

For example, the following configuration gives MetalLB control over
IPs from `192.168.1.240` to `192.168.1.250`, and configures Layer 2
mode:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: metallb-system
  name: config
data:
  config: |
    address-pools:
    - name: default
      protocol: layer2
      addresses:
      - 192.168.1.240-192.168.1.250
```

## BGP configuration

For a basic configuration featuring one BGP router and one IP address
range, you need 4 pieces of information:

- The router IP address that MetalLB should connect to,
- The router's AS number,
- The AS number MetalLB should use,
- An IP address range expressed as a CIDR prefix.

As an example, if you want to give MetalLB the range 192.168.10.0/24
and AS number 64500, and connect it to a router at 10.0.0.1 with AS
number 64501, your configuration will look like:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: metallb-system
  name: config
data:
  config: |
    peers:
    - peer-address: 10.0.0.1
      peer-asn: 64501
      my-asn: 64500
    address-pools:
    - name: default
      protocol: bgp
      addresses:
      - 192.168.10.0/24
```

### Advertisement configuration

By default, BGP mode advertises each allocated IP to the configured
peers with no additional BGP attributes. The peer router(s) will
receive one `/32` route for each service IP, with the BGP localpref
set to zero and no BGP communities.

You can configure more elaborate advertisements by adding a
`bgp-advertisements` section that lists one or more custom
advertisements.

In addition to specifying localpref and communities, you can use this
to advertise aggregate routes. The `aggregation-length` advertisement
option lets you "roll up" the /32s into a larger prefix. Combined with
multiple advertisement configurations, this lets you create elaborate
advertisements that interoperate with the rest of your BGP network.

For example, let's say you have a leased `/24` of public IP space, and
you've allocated it to MetalLB. By default, MetalLB will advertise
each IP as a /32, but your transit provider rejects routes more
specific than `/24`. So, you need to somehow advertise a `/24` to your
transit provider, but still have the ability to do per-IP routing
internally.

Here's a configuration that implements this:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: metallb-system
  name: config
data:
  config: |
    peers:
    - peer-address: 10.0.0.1
      peer-asn: 64501
      my-asn: 64500
    address-pools:
    - name: default
      protocol: bgp
      addresses:
      - 198.51.100.0/24
      bgp-advertisements:
      - aggregation-length: 32
        localpref: 100
        communities:
        - no-advertise
      - aggregation-length: 24
    bgp-communities:
      no-advertise: 65535:65282
```

With this configuration, if we create a service with IP 198.51.100.10,
the BGP peer(s) will receive two routes:

- `198.51.100.10/32`, with localpref=100 and the `no-advertise`
  community, which tells the peer router(s) that they can use this
  route, but they shouldn't tell anyone else about it.
- `198.51.100.0/24`, with no custom attributes.

With this configuration, the peer(s) will propagate the
`198.51.100.0/24` route to your transit provider, but once traffic
shows up locally, the `198.51.100.10/32` route will be used to forward
into your cluster.

As you define more services, the router will receive one "local" `/32`
for each of them, as well as the covering `/24`. Each service you
define "generates" the `/24` route, but MetalLB deduplicates them all
down to one BGP advertisement before talking to its peers.

The above configuration also showcases the `bgp-communities`
configuration section, which lets you define readable names for BGP
communities that you can reuse in your advertisement
configurations. This is completely optional, you could just specify
`65535:65281` directly in the configuration of the `/24` if you
prefer.

### Limiting peers to certain nodes

By default, every node in the cluster connects to all the peers listed
in the configuration. In more advanced cluster topologies, you may
want each node to connect to different routers. For example, if you
have a "rack and spine" network topology, you likely want each machine
to peer with its top-of-rack router, but not the routers in other
racks.

{{<mermaid align="center">}}
graph BT
    subgraph ""
      metallbA("MetalLB<br>Speaker")
    end
    subgraph ""
      metallbB("MetalLB<br>Speaker")
    end

    subgraph ""
      metallbC("MetalLB<br>Speaker")
    end
    subgraph ""
      metallbD("MetalLB<br>Speaker")
    end

    metallbA-->torA(ToR Router)
    metallbB-->torA(ToR Router)
    metallbC-->torB(ToR Router)
    metallbD-->torB(ToR Router)

    torA-->spine(Spine Router)
    torB-->spine(Spine Router)
{{< /mermaid >}}

You can limit peers to certain nodes by using the `node-selectors`
attribute of peers in the configuration. The semantics of these
selectors are the same as those used elsewhere in Kubernetes, so refer
to
the
[labels documentation](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/) on
the Kubernetes website.

For example, this is a (somewhat contrived) definition for a peer that
will only be used by machines:

- With hostname `hostA` or `hostB`, or
- That have the `rack=frontend` label, but _not_ the label `network-speed=slow`:

```yaml
peers:
- peer-address: 10.0.0.1
  peer-asn: 64501
  my-asn: 64500
  node-selectors:
  - match-labels:
      rack: frontend
    match-expressions:
    - key: network-speed
      operator: NotIn
      values: [slow]
  - match-expressions:
    - key: kubernetes.io/hostname
      operator: In
      values: [hostA, hostB]
```

### Configuring the BGP source address

When a host has multiple network interfaces or multiple IP addresses
configured on one interface, the host's TCP/IP stack usually selects
the IP address that is used as the source IP address for outbound
connections automatically. This is true also for BGP connections.

Sometimes, the automatically-selected address may not be the desired
one for some reason. In such cases, MetalLB supports explicitly
specifying the source address to be used when establishing a BGP
session:

```yaml
peers:
- peer-address: 10.0.0.1
  peer-asn: 64501
  my-asn: 64500
  source-address: 10.0.0.2
  node-selectors:
  - match-labels:
      kubernetes.io/hostname: node-1
```

The configuration above tells the MetalLB speaker to check if the
address `10.0.0.2` exists locally on one of the host's network
interfaces, and if so - to use it as the source address when
establishing BGP sessions. If the address isn't found, the default
behavior takes place (that is, the kernel selects the source address
automatically).

{{% notice warning %}}
In most cases the `source-address` field should only be used with
**per-node peers**, i.e. peers with node selectors which select only
one node.

By default, a BGP peer configured under the `peers` configuration
section runs on **all** speaker nodes. It is likely meaningless to use
the `source-address` field in a peer configuration that applies to
more than one node because two nodes in a given network usually
shouldn't have the same IP address.
{{% /notice %}}

### Peer autodiscovery

In addition to configuring BGP peers statically using the `peers` configuration
section, MetalLB supports peer autodiscovery using node annotations/labels.
Peers configured in this way are called **node peers** because unlike
statically-configured peers, node peers are always bound to a specific
Kubernetes node.

Peer autodiscovery is useful in cases where it is undesirable or impossible to
maintain a static list of peers in the ConfigMap manually. It allows load
balancing to continue functioning when adding and removing nodes and even when
scaling clusters automatically in API-driven bare metal environments.

Peer autodiscovery may be used in conjunction with static peer configuration.

MetalLB discovers node peers by looking for
[annotations](https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations/)
and/or
[labels](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/)
on the `Node` object associated with the Kubernetes node which runs a MetalLB
speaker pod and using their values to figure out the BGP configuration for node
peers. The specific annotations or labels to look for are **configurable**.

Following is a sample configuration which tells MetalLB to discover node peers
using annotations:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: metallb-system
  name: config
data:
  config: |
    address-pools:
    - name: default
      protocol: bgp
      addresses:
      - 198.51.100.0/24
    peer-autodiscovery:
      from-annotations:
      - my-asn: example.com/my-asn
        peer-asn: example.com/peer-asn
        peer-address: example.com/peer-address
```

The example above instructs MetalLB to try and discover a node peer for every
node which runs a speaker pod. MetalLB will expect to find the value for the
local ASN in an annotation called `example.com/my-asn`, the value for the
remote ASN in an annotations called `example.com/peer-asn` and the value for
the peer address in an annotation called `example.com/peer-address`. Therefore,
the `Node` Kubernetes object must have these annotations with the correct
values as in the following example:

```yaml
apiVersion: v1
kind: Node
metadata:
  annotations:
    example.com/my-asn: 64500
    example.com/peer-asn: 64501
    example.com/peer-address: 10.0.0.3
```

Similarly, the same behavior can be achieved using labels. The only difference
is that we use `from-lables` in place of `from-annotations`:

```yaml
peer-autodiscovery:
  from-labels:
  - my-asn: example.com/my-asn
    peer-asn: example.com/peer-asn
    peer-address: example.com/peer-address
```

MetalLB can be configured to discover multiple node peers by specifying
multiple sets of annotations/labels in the autodiscovery configuration:

```yaml
peer-autodiscovery:
  from-annotations:
  - my-asn: example.com/p1-my-asn
    peer-asn: example.com/p1-peer-asn
    peer-address: example.com/p1-peer-address
  - my-asn: example.com/p2-my-asn
    peer-asn: example.com/p2-peer-asn
    peer-address: example.com/p2-peer-address
```

{{% notice note %}}
BGP authentication currently isn't supported for node peers. Specifying
clear-text passwords in `Node` objects is dangerous, and until a more secure
solution for handling passwords is introduced, peer autodiscovery can only work
in environments where BGP authentication isn't configured.
{{% /notice %}}

Setting the right annotations/labels is **out of scope** for MetalLB. `Node`
objects can be annotated or labeled in a variety of ways, and it is assumed
that there is some external mechanism that puts the BGP configuration in
annotations/labels for MetalLB to consume. Following are some common examples
of such mechanisms:

- A
  [Cloud Controller Manager](https://kubernetes.io/docs/tasks/administer-cluster/running-cloud-controller/)
  can be used to automatically populate annotations/labels with BGP-related
  information retrieved from a cloud provider's API.
- The `--node-labels` kubelet flag can be used to register nodes with labels.
- The tooling used to bootstrap a Kubernetes cluster may be able to set
  annotations/labels.

It is possible to specify **default values** for peer autodiscovery:

```yaml
peer-autodiscovery:
  defaults:
    my-asn: 100
    peer-asn: 200
```

Default values are useful in cases where some BGP parameters are common for all
node peers and therefore should be configured statically. For example, it is
likely that ASNs be the same for all the peers on a given cluster.

## Advanced address pool configuration

### Controlling automatic address allocation

In some environments, you'll have some large address pools of "cheap"
IPs (e.g. RFC1918), and some smaller pools of "expensive" IPs
(e.g. leased public IPv4 addresses).

By default, MetalLB will allocate IPs from any configured address pool
with free addresses. This might end up using "expensive" addresses for
services that don't require it.

To prevent this behaviour you can disable automatic allocation for a pool
by setting the `auto-assign` flag to `false`:

```yaml
# Rest of config omitted for brevity
address-pools:
- name: cheap
  protocol: bgp
  addresses:
  - 192.168.144.0/20
- name: expensive
  protocol: bgp
  addresses:
  - 42.176.25.64/30
  auto-assign: false
```

Addresses can still be specifically allocated from the "expensive"
pool with the methods described in
the [usage](/usage/#requesting-specific-ips) section.

{{% notice note %}}
To specify a single IP address in a pool, use `/32` in the CIDR notation
(e.g. `42.176.25.64/32`).
{{% /notice %}}

### Handling buggy networks

Some old consumer network equipment mistakenly blocks IP addresses
ending in `.0` and `.255`, because of
misguided
[smurf protection](https://en.wikipedia.org/wiki/Smurf_attack).

If you encounter this issue with your users or networks, you can set
`avoid-buggy-ips: true` on an address pool to mark `.0` and `.255`
addresses as unusable.
