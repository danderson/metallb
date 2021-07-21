---
title: Release Notes
weight: 8
---

## Version 0.11.0 (Currently Under Development)

New Features:

- (helm chart) Add support for specifying additional labels for `PodMonitor`
  and `PrometheusRule` resources. This is needed when using the Prometheus
  operator and have it configured to use `PodMonitors` and `PrometheusRules`
  that are using a specific label.

- Leveled logging is now supported. You can set `--log-level` flag to one of 
  `all`, `debug`, `info`, `warn`, `error` or `none` to filter produced logs by level.
  The default value is set to `info` on both helm charts and k8s manifests.

Changes in behavior:

- With the newly introduced leveled logging support, the default value for the 
  `--log-level` is set to `info` on both helm charts and k8s manifests.
  This will produce fewer logs compared to the previous releases, 
  since many `debug` level logs will be filtered out. You can preserve the old verbosity by 
  editing the k8s manifests and setting the argument `--log-level=all` for both the controller and 
  speaker when installing using manifests, or by overriding helm values `controller.logLevel=all` 
  and `speaker.logLevel=all` when installing with Helm.

- MetalLB now uses a headless service to track the speakers. You must
  configure `METALLB_SERVICE`/`service` and create the service. Manifests and
  charts provided by MetalLB have been updated to create this new service.
  If you are using your own manifests, you must update them. ([#595](https://github.com/metallb/metallb/pull/595))

- The `METALLB_ML_LABELS`/`ml-labels` config option has been removed.
  The `pod-lister` role/rolebinding are not used anymore and have been removed.
  If you are using MetalLB-provided manifests to deploy, you can clean the old resources up by running
  `kubectl delete -n metallb-system role/pod-lister rolebinding/pod-lister`. ([#595](https://github.com/metallb/metallb/pull/595))

Bug Fixes:

- MetalLB now only considers ready speakers when memberlist is disabled.
  This allows `externalTrafficPolicy: cluster` services to work when MetalLB
  speakers run only on a subset of the nodes. ([#595](https://github.com/metallb/metallb/pull/595))

## Version 0.10.3

Bug Fixes:

- Add `fsGroup` to the MetalLB controller deployment to address compatibility with Kubernetes 1.21
  and later. See [Kubernetes issue #70679](https://github.com/kubernetes/kubernetes/issues/70679).
  This ensures the MetalLB controller can read the service account token volume.
  ([Issue #890](https://github.com/metallb/metallb/issues/890))

## Version 0.10.2

Bug Fixes:

- Fix a missing RBAC update in the manifests used by the helm chart.
  ([Issue #878](https://github.com/metallb/metallb/issues/878))

## Version 0.10.1

Bug Fixes:

- Fix the images in `manifests/metallb.yaml` to refer to the images for the
  release tag instead of the `main` branch.
  ([Issue #874](https://github.com/metallb/metallb/issues/874))

## Version 0.10.0

New Features:

- Helm Charts are now provided. You should be able to migrate from Bitnami
  Charts to MetalLB Charts by just changing the repo and upgrading. For more
  details, see the installation documentation.

- Version 0.9.x required the creation of a Secret called `memberlist`. This
  Secret is now automatically created by the MetalLB controller if it does not
  already exist. To use this feature you must set the new `ml-secret-name` and `deployment`
  options or `METALLB_ML_SECRET_NAME` and `METALLB_DEPLOYMENT` environment variables.
  This is already done in the manifests provided with this release.

- Endpoint Slices support. Endpoint slices are the proposed and more scalable
  way introduced in k8s to find services endpoints. From this version, MetalLB checks for
  EndpointSlices availability and uses them, otherwise it backs up to endpoints.

Changes in behavior:

- The `port` option to the `speaker`, which is the prometheus metrics port, now
  defaults to port `7472`. This was already the default in the manifests
  included with MetalLB, but the binary itself previously defaulted to port
  `80`.

- The `config-ns` option of both the `controller` and the `speaker` and the `ml-namespace`
  option and `METALLB_ML_NAMESPACE` environment variable of the `speaker` are
  replaced by the `namespace` option or the `METALLB_NAMESPACE` environment
  variable. If not set the namespace is read from `/var/run/secrets/kubernetes.io/serviceaccount/namespace`.

This release includes contributions from Adit Sachde, Adrian Goins, Andrew
Grosser, Brian Topping, Chance Carey, Chris Tarazi, Damien TOURDE, David
Anderson, Dax McDonald, dougbtv, Etienne Champetier, Federico Paolinelli,
Graeme Lawes, Henry-Kim-Youngwoo, Igal Serban, Jan Krcmar, JinLin Fu, Johannes
Liebermann, Jumpy Squirrel, Lars Ekman, Leroy Shirto, Mark Gray, NorthFuture,
Oleg Mayko, Reinier Schoof, Rodrigo Campos, Russell Bryant, Sebastien Dionne,
Stefan Lasiewski, Steven Follis, sumarsono, Thorsten Schifferdecker, toby
cabot, Tomofumi Hayashi, Tony Perez, and Yuan Liu. Thank you!

## Version 0.9.6
[Documentation for this release](https://metallb.universe.tf)

Bugfixes:

- Fix nodeAssigned event on k8s >= 1.20 ([#812](https://github.com/metallb/metallb/pull/812)).

This release includes contributions from Lars Ekman, Rodrigo Campos, Russell
Bryant and Stefan Lasiewski. Thanks for making MetalLB better!

## Version 0.9.5
[Documentation for this release](https://v0-9-5--metallb.netlify.com)

New features:

- Update manifests/metallb.yaml for kubernetes v1.19 ([#744](https://github.com/metallb/metallb/pull/744)).

Bugfixes:

- Update repository URLs ([#688](https://github.com/metallb/metallb/pull/688)).

This release includes contributions from Adit Sachde and Jan Krcmar. Thanks for
making MetalLB better!

## Version 0.9.4
[Documentation for this release](https://v0-9-4--metallb.netlify.com)

New features:

- Make Memberlist bind port configurable ([#582](https://github.com/metallb/metallb/pull/582)).

Bugfixes:

- Improve speaker log output ([#587](https://github.com/metallb/metallb/pull/587)).
- Add "other" exec permission to binaries in Docker images ([#644](https://github.com/metallb/metallb/pull/644)).
- Fix wrong behavior of the addresses_in_use_total metric under certain conditions ([#627](https://github.com/metallb/metallb/pull/627)).
- Layer 2: Fix Memberlist convergence following a network partition ([#662](https://github.com/metallb/metallb/pull/662)).
- Layer 2: Send gratuitous ARP / unsolicited NDP neighbor advertisements following a network partition ([#736](https://github.com/metallb/metallb/pull/736)).

This release includes contributions from Andrew Grosser, Chance Carey, Damien
TOURDE, Etienne Champetier, Johannes Liebermann, Jumpy Squirrel, Lars Ekman,
Rodrigo Campos, Russell Bryant, Sebastien Dionne, Steven Follis, sumarsono
Thorsten Schifferdecker, toby cabot and Yuan Liu. Thanks to all of them for
making MetalLB better!

## Version 0.9.3
[Documentation for this release](https://v0-9-3--metallb.netlify.com)

Bugfixes:

- Fix manifests to use container image version `v0.9.3` instead of `main`. Users
  of `v0.9.2` are encouraged to upgrade, as [manifests included in that
  release](https://raw.githubusercontent.com/metallb/metallb/v0.9.2/manifests/metallb.yaml)
  use an incorrect container image version. Those two images happen to match
  now but, as development continues on `main` branch, they will differ.

- Update installation procedure to create the namespace first ([#557](https://github.com/metallb/metallb/pull/557)).

This release includes contributions from Henry-Kim-Youngwoo, Oleg Mayko and
Rodrigo Campos. Thanks to all of them for making MetalLB better!

## Version 0.9.2
[Documentation for this release](https://v0-9-2--metallb.netlify.com)

New features:

-  Dramatically reduce dead node detection time when using Layer 2 mode ([#527](https://github.com/metallb/metallb/pull/527)).
   This is improvement closes the long standing issue
[#298](https://github.com/metallb/metallb/issues/298) that has been a common
pain point for users using Layer 2 mode. This feature is enabled by default. You
can disable it by simply changing the `speaker` `Daemonset` manifest and
remove the `METALLB_ML_BIND_ADDR` environment variable. Also, you can verify
the old method is being used by checking the `speaker` log on startup to
contain: `Not starting fast dead node detection (MemberList)`. If not shown,
the new fast node detection method is being used.

- Allow spaces in address pool IP ranges ([#499](https://github.com/metallb/metallb/issues/499)).

Action required:

- Layer 2 users by default will use a new algorithm to detect dead nodes (time
  is significantly reduced). If you want to continue with the old way, see the
  New features section to see how to opt-out. If you find any problems with the
  new algorithm, as usual, please open an issue.

Bug fixes:

- Allow kustomize to change namespace MetalLB runs ([#516](https://github.com/metallb/metallb/pull/516)).
- Fix layer2 not sending ARP messages when IP changes ([#520](https://github.com/metallb/metallb/pull/520)). Fixes [#471](https://github.com/metallb/metallb/issues/471).
- Fix to properly expose `address_total` Prometheus metric ([#518](https://github.com/metallb/metallb/pull/518)).
- Add note in installation process about `strictARP` when using `kube-proxy` in IPVS mode ([#507](https://github.com/metallb/metallb/pull/507)).
- Support older devices that might not support RFC4893 ([#491](https://github.com/metallb/metallb/pull/491)).

This release includes contributions from binoue, David Anderson, dulltz, Etienne
Champetier, Gary Richards, Jean-Philippe Evrard, Johan Fleury, k2mahajan, Knic
Knic, kvaps, Lars Ekman, masa213f, remche, Rickard von Essen, Rui Lopes, Serge
Bazanski, Spence. Thanks to all of them for making MetalLB better!

## Versions 0.9.0 and 0.9.1

0.9.0 and 0.9.1 were never released, due to a bug that prevented
building Docker images. 0.9.2 is the first "real" release of the 0.9.x
branch.

## Version 0.8.3

[Documentation for this release](https://v0-8-3--metallb.netlify.com)

New features:

- The manifests directory now has a kustomize file, which allows using
  kustomize to install and configure MetalLB.

This release includes contributions from Rémi Cailletaud.

## Version 0.8.2

[Documentation for this release](https://v0-8-2--metallb.netlify.com)

Action required:

- The MetalLB Helm chart in the official helm repository is no longer
  a supported installation method.

Bugfixes:

- Fix layer2 node selection when healthy and unhealthy replicas are colocated on a single node. ([#474](https://github.com/metallb/metallb/issues/474))

This release includes contributions from David Anderson and Gary Richards.

## Version 0.8.1

[Documentation for this release](https://v0-8-1--metallb.netlify.com)

Bugfixes:

- Fix the apiGroup for PodSecurityPolicy, for compatibility with Kubernetes 1.16. ([#458](https://github.com/metallb/metallb/issues/458)).
- Fix speaker posting events with an empty string as the announcing node name. ([#456](https://github.com/metallb/metallb/issues/456)).
- Fix RBAC permissions on speaker, to allow it to post events to all
  namespaces. ([#455](https://github.com/metallb/metallb/issues/455)).

This release includes contributions from David Anderson.

## Version 0.8.0

[Documentation for this release](https://v0-8-0--metallb.netlify.com)

Action required if updating from 0.7.x:

- The `speaker` DaemonSet now specifies a toleration to run on
  Kubernetes control plane nodes that have the standard, unfortunately
  named "master" taint. If you don't want MetalLB to run on control
  plane nodes, you need to remove that toleration from the manifest.
- The manifest and Helm chart both now specify a `PodSecurityPolicy`
  allowing the `speaker` DaemonSet to request the elevated privileges
  it needs. If your cluster enforces pod security policies, you should
  review the provided policy before deploying it.
- The speaker defaults to only offering its Prometheus metrics on the
  node IP as registered in Kubernetes (i.e. the IP you see in `kubectl
  get nodes -owide`). To revert to the previous behavior of offering
  metrics on all interfaces, remove the METALLB_HOST environment
  variable from the manifest.

New features:

- The manifest and Helm chart now define a `PodSecurityPolicy` for the
  MetalLB speaker, granting it the necessary privileges for it to
  function. This should make MetalLB work out of the box in clusters
  with pod security policies enforced.
- On Windows/Linux hybrid Kubernetes clusters, MetalLB constrains
  itself to run only on linux nodes (via a `nodeSelector`).
- The MetalLB speaker now tolerates running on Kubernetes control
  plane nodes. This means that services whose pods run only on control
  plane nodes (e.g. the Kubernetes dashboard, in some setups) are now
  reachable.
- MetalLB withdraws BGP announcements entirely for services with no
  healthy pods. This enables anycast geo-redundancy by advertising the
  same IP from multiple Kubernetes
  clusters. ([#312](https://github.com/metallb/metallb/issues/312))
- The speaker only exposes its Prometheus metrics port on the node IP
  registered with Kubernetes, rather than on all interfaces. This
  should reduce the risk of exposure for clusters where nodes have
  separate public and private interfaces.
- The website has updated compatibility grids for both [Kubernetes
  network
  addons](https://metallb.universe.tf/installation/network-addons/)
  and [cloud
  providers](https://metallb.universe.tf/installation/cloud/), listing
  known issues and configuration tips.
- MetalLB now publishes a Kubernetes event to a service, indicating
  which nodes are announcing that service. This makes it much easier
  to determine how traffic is
  flowing. ([#430](https://github.com/metallb/metallb/issues/430))
- The manifest and Helm chart now use the `apps/v1` version of
  `Deployment` and `DaemonSet`, rather than the obsolete
  `extensions/v1beta1`.

Bugfixes:

- Fix address allocation in cases where no addresses were available at
  service creation, but the deletion of another service subsequently
  makes one
  available. ([#413](https://github.com/metallb/metallb/issues/413))
- Fix allocation not updating when the address pool annotation
  changes. ([#448](https://github.com/metallb/metallb/issues/448)).
- Fix periodic crashes due to `glog` trying to write to disk despite
  explicit instructions to the
  contrary. ([#427](https://github.com/metallb/metallb/issues/427))
- Fix `spec.loadBalancerIP` validation on IPv6 clusters.
  ([#301](https://github.com/metallb/metallb/issues/301))
- Fix BGP Router ID selection on v6 BGP sessions.
- Fix handling of IPv6 addresses in the BGP connection establishment
  logic.
- Generate deterministically pseudorandom BGP router IDs in IPv6-only
  clusters.
- Fix incorrect ARP/NDP responses on bonded interfaces.
  ([#349](https://github.com/metallb/metallb/issues/349))
- Fix ARP/NDP responses sent on interfaces with the NOARP flag.
  ([#351](https://github.com/metallb/metallb/issues/351))
- Update MetalLB logs on the website to the new structured
  format. ([#275](https://github.com/metallb/metallb/issues/301))

This release includes contributions from Alex Lovell-Troy, Antonio
Ojea, aojeagarcia, Ashley Dumaine, Brian, Brian Topping, David
Anderson, Eduardo Minguez Perez, Elan Hasson, Irit Goihman, Ivan
Kurnosov, Jeff Kolb, johnl, Jordan Neufeld, kvaps, Lars Ekman, Matt
Sharpe, Maxime Guyot, Miek Gieben, Niklas Voss, Oilbeater, remche,
Rodrigo Campos, Sergey Anisimov, Stephan Fudeus, Steven Beverly,
stokbaek and till. Thanks to all of them for making MetalLB better!

## Version 0.7.3

[Documentation for this release](https://v0-7-3--metallb.netlify.com)

Bugfixes:

- Fix BGP announcement refcounting when using shared
  IPs. ([#295](https://github.com/metallb/metallb/issues/295))

## Version 0.7.2

[Documentation for this release](https://v0-7-2--metallb.netlify.com)

Bugfixes:

- Fix gratuitous ARP and NDP announcements on IP
  failover. ([#291](https://github.com/metallb/metallb/issues/291))
- Fix BGP dialing on Arm64, by using `x/sys/unix` instead of the
  `syscall` package. ([#289](https://github.com/metallb/metallb/issues/289))

## Version 0.7.1

[Documentation for this release](https://v0-7-1--metallb.netlify.com)

Bugfixes:

- Actually allow layer2 mode to use the Local traffic
  policy. Oops. ([#279](https://github.com/metallb/metallb/issues/279))

## Version 0.7.0

[Documentation for this release](https://v0-7-0--metallb.netlify.com)

Action required if updating from 0.6.x:

- MetalLB no longer does leader election. After upgrading to 0.7, you
  can delete a number of k8s resources associated with that. This is
  just a cleanup, nothing bad happens if you leave the resources
  orphaned in your cluster. Depending on your installation method,
  some of these may have already been cleaned up for you.
  - `kubectl delete -nmetallb-system endpoints metallb-speaker`
  - `kubectl delete -nmetallb-system rolebinding leader-election`
  - `kubectl delete -nmetallb-system role leader-election`

New features:

- Layer2 mode now supports `externalTrafficPolicy=Local`, meaning layer2
  services can see the true client source
  IP. ([#257](https://github.com/metallb/metallb/issues/257))
- Layer2 mode now selects leader nodes on a per-service level, instead of using
  a single leader node for all services in the cluster. If you have many
  services, this change spreads the load of handling incoming traffic across
  more than one machine. ([#195](https://github.com/metallb/metallb/issues/195))
- MetalLB's maturity has upgraded from _alpha_ to _beta_! Mostly this
  just reflects the increased confidence in the code from the larger
  userbase, and adds some guarantees around graceful upgrades from one
  version to the next.

Bugfixes:

- Speaker no longer sends localpref over eBGP sessions
  ([#266](https://github.com/metallb/metallb/issues/266))

This release includes contributions from Baul, David Anderson, Ryan
Roemmich, Sanjeev Rampal, and Steve Sloka. Thanks to all of them for
making MetalLB better!

## Version 0.6.2

[Documentation for this release](https://v0-6-2--metallb.netlify.com)

Bugfixes:

- Fix nil pointer deref crash on BGP peers that reject MetalLB's OPEN message too promptly ([#250](https://github.com/metallb/metallb/issues/250))

## Version 0.6.1

[Documentation for this release](https://v0-6-1--metallb.netlify.com)

Bugfixes:

- Speaker no longer goes into a tight CPU-burning loop when pods are
  deleted on the
  node. ([#246](https://github.com/metallb/metallb/issues/246))

## Version 0.6.0

[Documentation for this release](https://v0-6-0--metallb.netlify.com)

Action required if upgrading from 0.5.x:

- As documented in the 0.5.0 release notes, several deprecated fields
  have been removed from the configuration. If you didn't update your
  configurations for 0.5, you may need to make the following changes:
  - Rename the `cidr` field of address pools to `addresses`
  - Rename `protocol: arp` and `protocol: ndp` to `protocol: layer2`
  - Replace `arp-network` statements with a range-based IP allocation

New features:

- You can now colocate multiple services on a single IP address, using
  annotations on the Service objects. See
  the
  [IP sharing documentation]({{% relref "usage/_index.md" %}}#ip-address-sharing) for
  instructions and caveats. ([#121](https://github.com/metallb/metallb/issues/121))
- Layer 2 mode now listens on all interfaces for ARP and NDP requests,
  not just the interface used for communication by Kubernetes
  components. ([#165](https://github.com/metallb/metallb/issues/165))
- MetalLB now uses structured logging instead of Google's glog
  package. Logging events are written to standard output as a series
  of JSON objects suitable for collection by centralized logging
  systems. ([#189](https://github.com/metallb/metallb/issues/189))
- BGP connections can now specify a password for TCP MD5 secured BGP
  sessions. ([#215](https://github.com/metallb/metallb/issues/215))
- MetalLB is now available as a Helm package in the "stable" Helm
  repository. Note that, due to code review delay, it may take several
  days after a release before the Helm package is
  updated. ([#177](https://github.com/metallb/metallb/issues/177))

Bugfixes:

- Correctly use AS_SEQUENCE in eBGP session messages, rather than
  AS_SET ([#225](https://github.com/metallb/metallb/issues/225))

This release includes contributions from David Anderson, ghorofamike,
Serguei Bezverkhi, and Zsombor Welker. Thanks to all of them for making
MetalLB better!

## Version 0.5.0

[Documentation for this release](https://v0-5-0--metallb.netlify.com)

Action required if upgrading from 0.4.x:

- The `cidr` field of address pools in the configuration file has been
  renamed to `addresses`. MetalLB 0.5 understands both `cidr` and
  `addresses`, but in 0.6 it will only understand `addresses`, so
  please update now.
- The `arp` and `ndp` protocols have been replaced by a unified
  `layer2` protocol. MetalLB 0.5 understands both the old and new
  names, but 0.6 will only understand `layer2`, so please update now.
- Remove any `arp-network` entries from your configuration. If your
  address pool overlaps with the ethernet network or broadcast
  addresses for your LAN, use IP range notation (see new features) to
  exclude them from your address pool.
- The router IDs used on BGP sessions may change in this version, in
  clusters where nodes have multiple IP addresses. If your BGP
  infrastructure monitors or enforces specific router IDs for peers,
  you may need to update those systems to match new router IDs.
- The Prometheus metrics for ARP and NDP traffic have been
  merged. Instead of `arp_*` and `ndp_*` metrics, there is now single
  set of `layer2_*` metrics, in which the `ip` label can be IPv4 or
  IPv6.

New features:

- ARP and NDP modes have been replaced by a single "layer 2" mode,
  indicated by `protocol: layer2` in the configuration file. Layer 2
  mode uses ARP and NDP under the hood, but having a single protocol
  name makes it easier to build protocol-agnostic configuration
  templates.
- You can give addresses to MetalLB using a simple IP range notation,
  in addition to CIDR prefixes. For example,
  `192.168.0.0-192.168.0.255` is equivalent to `192.168.0.0/24`. This
  makes it much easier to allocate IP ranges that don't fall cleanly
  on CIDR prefix boundaries.
- BGP mode supports nodes with multiple interfaces and IP addresses
  ([#182](https://github.com/metallb/metallb/issues/182)). Previously,
  MetalLB could only establish working BGP sessions on the node's
  "primary" interface, i.e. the one that owned the IP that Kubernetes
  uses to identify the node. Now, peerings may be established via any
  interface on the nodes, and traffic will flow in the expected
  manner.

Bugfixes:

- The NDP
  handler
  [refcounts its sollicited multicast group memberships](https://github.com/metallb/metallb/issues/184),
  to avoid extremely rare cases where it might stop responding for a
  service IP.

## Version 0.4.6

[Documentation for this release](https://v0-4-6--metallb.netlify.com)

Bugfixes:

- [Remove the --config-ns flag](https://github.com/metallb/metallb/issues/193)

## Version 0.4.5

[Documentation for this release](https://v0-4-5--metallb.netlify.com)

Bugfixes:

- [Controller doesn't clean up balancers that change their type away from LoadBalancer](https://github.com/metallb/metallb/issues/190)

## Version 0.4.4

[Documentation for this release](https://v0-4-4--metallb.netlify.tf)

This was a broken attempt to fix the same bugs as 0.4.5. You should
not use this version.

## Version 0.4.3

[Documentation for this release](https://v0-4-3--metallb.netlify.com)

Changes:

- Make the configmap's namespace and name configurable via flags, for
  Helm upstreaming.

## Version 0.4.2

[Documentation for this release](https://v0-4-2--metallb.netlify.com)

Bugfixes:

- [Speaker doesn't readvertise existing services on sessions added by node label changes](https://github.com/metallb/metallb/issues/181).

## Version 0.4.1

[Documentation for this release](https://v0-4-1--metallb.netlify.com)

Bugfixes:

- [Make speaker not crash on machines with IPv6 disabled](https://github.com/metallb/metallb/issues/180).

## Version 0.4.0

[Documentation for this release](https://v0-4-0--metallb.netlify.com)

Action required if upgrading from 0.3.x:

- MetalLB's use of Kubernetes labels has changed slightly to conform
  to Kubernetes best practices. If you were using a label match on
  `app: controller` or `app: speaker` Kubernetes labels to find
  MetalLB objects, you should now match on a combination of `app:
  metallb`, `component: controller` or `component: speaker`, depending
  on what objects you want to select.
- RBAC rules have changed, and now allow the MetalLB speaker to list
  and watch Node objects. If you are not installing MetalLB via the
  provided manifest, you will need to make this change by hand.
- If you want to switch to using Helm to manage your MetalLB
  installation, you must first uninstall the manifest-based version,
  with `kubectl delete -f metallb.yaml`.

New features:

- Initial IPv6 support! The `ndp` protocol allows v6 Kubernetes
  clusters to advertise their services using
  the
  [Neighbor Discovery Protocol](https://en.wikipedia.org/wiki/Neighbor_Discovery_Protocol),
  IPv6's analog to ARP. If you have an IPv6 Kubernetes cluster, please
  try it out
  and [file bugs](https://github.com/metallb/metallb/issues/new)!
- BGP peers now have
  a
  [node selector]({{% relref "configuration/_index.md" %}}#limiting-peers-to-certain-nodes). You
  can use this to integrate MetalLB into more complex cluster network
  topologies.
- MetalLB now has
  a
  [Helm chart](https://github.com/metallb/metallb/tree/main/helm/metallb). If
  you use [Helm](https://helm.sh) on your cluster, this should make it
  easier to track and manage your MetalLB installation. The chart will
  be submitted for inclusion in the main Helm stable repository
  shortly after the release is finalized. Use of Helm is optional,
  installing the manifest directly is still fully supported.

Other improvements:

- MetalLB
  now
  [backs off on failing BGP connections](https://github.com/metallb/metallb/issues/84),
  to avoid flooding logs with failures
- ARP mode should be a little
  more
  [interoperable with clients](https://github.com/metallb/metallb/issues/172),
  and failover should be a little faster, thanks to tweaks to its
  advertisement logic.
- ARP and NDP modes export [Prometheus](https://prometheus.io) metrics
  for requests received, responses sent, and failover-related
  transmissions. This brings them up to "monitoring parity" with BGP
  mode.
- Binary internals were refactored to share more common code. This
  should reduce the amount of visual noise in the logs.

This release includes contributions from Oga Ajima, David Anderson,
Matt Layher, John Marcou, Paweł Prażak, and Hugo Slabbert. Thanks to
all of them for making MetalLB better!

## Version 0.3.1

[Documentation for this release](https://v0-3-1--metallb.netlify.com)

Fixes a couple
of [embarrassing bugs](https://github.com/metallb/metallb/issues/142)
that sneaked into 0.3.

Bugfixes:

- Revert to using `apps/v1beta2` instead of `apps/v1` for MetalLB's
  Deployment and Daemonset, to remain compatible with Kubernetes 1.8.
- Create the `metallb-system` namespace when installing
  `test-bgp-router`.
- Disable BIRD in `test-bgp-router`. Bird got updated to 2.0, and the
  integration with `test-bgp-router` needs some reworking.

## Version 0.3.0

[Documentation for this release](https://v0-3-0--metallb.netlify.com)

Action required if upgrading from 0.2.x:

- The `bgp-speaker` DaemonSet has been renamed to just
  `speaker`. Before applying the manifest for 0.3.0, delete the old
  daemonset with `kubectl delete -n metallb-system
  ds/bgp-speaker`. This will take down your load balancers until you
  deploy the new DaemonSet.
- The
  [configuration file format](https://raw.githubusercontent.com/metallb/metallb/main/manifests/example-config.yaml) has
  changed in a few backwards-incompatible ways. You need to update
  your ConfigMap by hand:
  - Each `address-pool` must now have a `protocol` field, to select
    between ARP and BGP mode. For your existing configurations, add
    `protocol: bgp` to each address pool definition.
  - The `advertisements` field of `address-pool` has been renamed to
    `bgp-advertisements`, and is now optional. If you don't need any
    special advertisement settings, you can remove the section
    entirely, and MetalLB will use a reasonable default.
  - The `communities` section has been renamed to `bgp-communities`.

New features:

- MetalLB now supports ARP advertisement, enabled by setting
  `protocol: arp` on an address pool. ARP mode does not require any
  special network equipment, and minimal configuration. You can follow
  the ARP mode tutorial to get started. There is also a page about ARP
  mode's [behavior and tradeoffs]({{% relref "concepts/layer2.md"
  %}}), and documentation on [configuring ARP mode]({{% relref
  "configuration/_index.md" %}}).
- The container images are
  now
  [multi-architecture images](https://blog.docker.com/2017/11/multi-arch-all-the-things/). MetalLB
  now supports running on all supported Kubernetes architectures:
  amd64, arm, arm64, ppc64le, and s390x.
- You can
  now
  [disable automatic address allocation]({{% relref "configuration/_index.md" %}}#controlling-automatic-address-allocation) on
  address pools, if you want to have manual control over the use of
  some addresses.
- MetalLB pods now come
  with
  [Prometheus scrape annotations](https://github.com/prometheus/prometheus/blob/main/documentation/examples/prometheus-kubernetes.yml). If
  you've configured your Prometheus-on-Kubernetes to automatically
  discover monitorable pods, MetalLB will be discovered and scraped
  automatically. For more advanced monitoring needs,
  the
  [Prometheus Operator](https://coreos.com/operators/prometheus/docs/latest/user-guides/getting-started.html) supports
  more flexible monitoring configurations in a Kubernetes-native way.
- We've documented how
  to
  [Integrate with the Romana networking system]({{% relref "configuration/romana.md" %}}),
  so that you can use MetalLB alongside Romana's BGP route publishing.
- The website got a makeover, to accommodate the growing amount of
  documentation in a discoverable way.

This release includes contributions from David Anderson, Charles
Eckman, Miek Gieben, Matt Layher, Xavier Naveira, Marcus Söderberg,
Kouhei Ueno. Thanks to all of them for making MetalLB better!

## Version 0.2.1

[Documentation for this release](https://v0-2-1--metallb.netlify.com)

Notable fixes:

- MetalLB unable to start because Kubernetes cannot verify that
  "nobody" is a non-root
  user ([#85](https://github.com/metallb/metallb/issues/85))

## Version 0.2.0

[Documentation for this release](https://v0-2-0--metallb.netlify.com)

Major themes for this version are: improved BGP interoperability,
vastly increased test coverage, and improved documentation structure
and accessibility.

Notable features:
 
- This website! It replaces a loose set of markdown files, and
  hopefully makes MetalLB more accessible.
- The BGP speaker now speaks Multiprotocol BGP
  ([RFC 4760](https://tools.ietf.org/html/rfc4760)). While we still
  only support IPv4 service addresses, speaking Multiprotocol BGP is a
  requirement to successfully interoperate with several popular BGP
  stacks. In particular, this makes MetalLB compatible
  with [Quagga](http://www.nongnu.org/quagga/) and Ubiquiti's
  EdgeRouter and Unifi product lines.
- The development workflow with Minikube now works with Docker for
  Mac, allowing mac users to hack on MetalLB. See
  the [hacking documentation]({{% relref "community/_index.md" %}})
  for the required additional setup.

Notable fixes:

- Handle multiple BGP peers properly. Previously, bgp-speaker
  mistakenly made all its connections to the last defined peer,
  ignoring the others.
- Fix a startup race condition where MetalLB might never allocate an
  IP for some services.
- Test coverage is above 90% for almost all packages, up from ~0%
  previously.
- Fix yaml indentation in the MetalLB manifests.

## Version 0.1.0

[Documentation for this release](https://github.com/metallb/metallb/tree/v0.1)

This was the first tagged version of MetalLB. Its changelog is
effectively "MetalLB now exists, where previously it did not."
