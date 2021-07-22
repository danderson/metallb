# ARP Load Sharing

## Summary

MetalLB in layer 2 mode does not support load balancing. See [here](https://metallb.universe.tf/concepts/layer2/).
However, in some cases, L2 is the only option, yet we still require a load balancing solution which is internal to our infrastructure.

A method that can achieve that is known as "ARP Load Sharing". See for example [here](https://docs.paloaltonetworks.com/pan-os/10-0/pan-os-admin/high-availability/ha-concepts/arp-load-sharing). This technique involves fiddling with how ARP replies are sent, which raises the concern that it might get blocked by the network switch or security policy since it seems like an [ARP Spoofing](https://en.wikipedia.org/wiki/ARP_spoofing) attack. See more information about "does arp spoofing work on all lans" [here](https://security.stackexchange.com/questions/133784/does-arp-spoofing-work-on-all-lans).

This document proposes adding an option to MetalLB to support "ARP Load Sharing" mode.

## Motivation

When BGP is not available, in order to support load balancing and HA for a Kubernetes Service, we need to create as many Services as there are nodes, so that each node will be an owner of one VIP and that node will be able to get traffic directly from clients. The VIP is needed to support HA so that on node failure the VIP can move to another node. However, even with a service per node, we still need to add a higher level load balancer, which will balance those VIPs for the clients (for example using switches that can perform L3-routing, DNS round-robin, etc.).

We want to simplify this setup, and have an option to "serve" a VIP from all nodes.

### Goals

- Add config option to enable/disable arp-load-sharing.
- Assuming the network allows it, the clients should be balanced between the nodes using a single Service VIP.

### Non-Goals

- It is expected that the more sophisticated the network gateway is, that it might block this behavior, unless it is manually configured to allow it, but this will remain outside the scope of this proposal, as a manual integration to the network environment.

## Proposal

DRAFT - still some open questions:

- Add config option to enable/disable arp-load-sharing
  - where? can we tag a feature as alpha/experimental?
- In this mode instead of single VIP owner, any node will be able to reply to ARP requests on the IP. We can think of two ways to answer:
  - The simplest way is that all nodes will answer all the requests (with random jitter), and will effectively "race" on the ARP mapping for the clients, which will keep those mappings for some time until their ARP cache expires.
  - More organized way could be that every node will enumerate the list of nodes where the service is available (for example based on externalTrafficPolicy or other restrictions) and then use the client IP hash to select just one node for each client.


## Alternatives

- Use BGP if available, although it is still quite rare in the enterprise.
- Introduce LB router in L3+, which requires a VIP per node.
