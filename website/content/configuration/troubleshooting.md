---
title: Troubleshooting
weight: 20
---

Once you setup a loadbalancer on Kubernetes and it doesn't _seem_ to work right away, here are a few tips to troubleshoot.

- Check that the LoadBalancer service has endpoints (`kubectl -n <namespace> get endpoints <service>`) that are not pending - if they are, MetalLB will not respond to ARP requests for that service's external IP
- SSH into one or more of your nodes and use `arping` and `tcpdump` to verify the ARP requests pass through your network

---

Below assumes you setup a loadbalancer with an IP of `192.168.1.240` and then we show usage of the commands and successful
requests going back and forth.

TL;DR - We found out that the IP `192.168.1.240` is located at the mac `FA:16:3E:5A:39:4C`.

If these requests fail, your network stack does not allow ARP to pass, therefore - we won't know about MetalLB's
assignment of the IP and can't connect.

## Tools

### arping

In this example, `arping` is used to trigger a request and it should receive a response.

The output should look similar to the following:

```bash
$ arping -I ens3 192.168.1.240
ARPING 192.168.1.240 from 192.168.1.35 ens3
Unicast reply from 192.168.1.240 [FA:16:3E:5A:39:4C]  1.077ms
Unicast reply from 192.168.1.240 [FA:16:3E:5A:39:4C]  1.321ms
Unicast reply from 192.168.1.240 [FA:16:3E:5A:39:4C]  0.883ms
Unicast reply from 192.168.1.240 [FA:16:3E:5A:39:4C]  0.968ms
^CSent 4 probes (1 broadcast(s))
Received 4 response(s)
```

`192.168.1.35` is the IP of one of the worker nodes and part of the same subnet.

### tcpdump

`tcpdump` can be used on the same worker node or another one in order to verify arp requests go back and forth.

Capture all replies from `192.168.1.240`:

```bash
$ tcpdump -n -i ens3 arp src host 192.168.1.240
tcpdump: verbose output suppressed, use -v or -vv for full protocol decode
listening on ens3, link-type EN10MB (Ethernet), capture size 262144 bytes
17:04:40.667263 ARP, Reply 192.168.1.240 is-at fa:16:3e:5a:39:4c, length 46
17:04:41.667485 ARP, Reply 192.168.1.240 is-at fa:16:3e:5a:39:4c, length 46
17:04:42.667572 ARP, Reply 192.168.1.240 is-at fa:16:3e:5a:39:4c, length 46
17:04:43.667545 ARP, Reply 192.168.1.240 is-at fa:16:3e:5a:39:4c, length 46
^C
4 packets captured
6 packets received by filter
0 packets dropped by kernel
```

### speaker logs

In addition to the above, make sure to watch the logs of MetalLB's speaker component for ARP requests and responses (using [kubetail](https://github.com/johanhaleby/kubetail)):

```bash
$ kubetail -l component=speaker -n metallb-system
...
```

## Status endpoint

MetalLB's speaker component exposes an HTTP endpoint which provides information
about the speaker's in-memory status. The status endpoint currently provides
information about BGP peering status only.

{{% notice note %}}
The status endpoint is intended for debugging purposes and should be used by
humans. There are no guarantees regarding the structure of the information
provided by the endpoint so automation shouldn't rely on it. In addition, it is
highly recommended not to expose the endpoint to the public internet as doing
so may introduce security risks.
{{% /notice %}}

To query the status endpoint of a speaker pod, use `kubectl` to forward a local
port to port `7473` on the pod:

```
$ kubectl -n metallb-system port-forward speaker-8l8gk 7473
```

Then, use `curl` (or a browser) to query the endpoint:

```
$ curl localhost:7473/status/bgp
{
  "Peers": [
    {
      "MyASN": 64512,
      "ASN": 64512,
      "Addr": "172.18.0.5",
      "Port": 179,
      "HoldTime": "1m30s",
      "RouterID": "",
      "NodeSelectors": [
        ""
      ],
      "Password": ""
    }
  ]
}
```
