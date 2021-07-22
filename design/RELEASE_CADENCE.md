# MetalLB release cadence

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
- [Proposal](#proposal)
  - [Notes/Constraints/Caveats (Optional)](#notesconstraintscaveats-optional)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed (Optional)](#infrastructure-needed-optional)
<!-- /toc -->

## Summary

The purpouse of this enhancement is to define a versioning and release schedule tied to
kubernetes versions.

## Motivation

The reason for this proposal is to establish a dependency between a given MetalLB version and kubernetes versions.

This will make MetalLB more reliable with regards with the set of kubernetes apis it can use or can't use. A good example for it is the introduction of the use of EndpointSlices, which are versioned as beta in k8s 1.17+ but moving to stable in 1.21.

### Goals

Define MetalLB release cadence and versioning as bounded to kubernetes versions.

## Proposal

Each MetalLB release branch correspond to a released k8s version.

Every time a k8s version is released, the api used by metalLB are bumped to that release and the version used
by kind in CI is bumped. This would prevent using deprecated apis.

### Notes/Constraints/Caveats (Optional)

Each version of MetalLB needs to declare the compatibility with the cluster. For example, if it uses the stable endpoint slices, it will need to declare the k8s version range it's compatible with.

The most recent release of MetalLB must be able to support all the supported k8s versions (n-3 as of today). We can ensure that by having multiple CI lanes running kind
with different kubernetes versions.

This will guarantee that urgent bugs can be backported only to the most recent release (which will be able to run on all the supported k8s versions). Backports to older releases
will be considered under exceptional circumstances.

### Risks and Mitigations

Having more than one CI lanes with a min and a max k8s version each branch declares it to be compatible with.

## Design Details

No design details.

### Test Plan

As wrote in the risks, this is about tying one or more version kind uses to a specific release.

## Drawbacks

The need of more diligence when bumping up the api version used, and also the need for documenting the k8s version compatible with a given metalLB version.

## Alternatives

Continue doing periodic or "per feature" releases, independent from k8s versions.