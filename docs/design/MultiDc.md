# Multi Data Centers Clusters

## Context

This design doc will describe how to implement the support of multi dc cluster on the scylla operator.

See https://docs.scylladb.com/operating-scylla/procedures/cluster-management/create_cluster_multidc/ 
for more detail on this scylla feature

As a first step only hostNetwork cluster will support this feature.

It is probably possible to extend this to non hostNetwork cluster if a solution (outside of the scylla operator scope) is available
to ensure pods of each sites can reach pods from other sites (For ex.: https://docs.projectcalico.org/networking/determine-best-networking#pod-ip-routability-outside-of-the-cluster)

## Design

The design will be split in 2 parts:
- Exposing PodIP instead of member service IP
- Providing multi dc feature

### Exposing PodIP instead of member service IP

Current implementation of the hostNetwork cluster expose the member service IP, which is an IP only accessible from local kubernetes clusters, as the member address on pods.
Due to this each pods from a cluster will not be able to reach other sites pods. As this is a mandatory feature to be able to activate multi dc we will first ensure that the PodIP is used as the member address.

Exposing PodIP will still rely on the member service to provide the member address to the sidecar operator (listen-address, broadcast-address, seeds ip...) and other operator actions that require it (replace, upgrade, cluster status).
A specific label scylla/ip will be added to the member service with the PodIP in the MemberServiceForPod function for hostNetwork clusters.
All part of code that will require to get the member address will be updated to get IP from a common function GetIpFromService. This function will return member service clusterIP for non hostNetwork cluster or the scylla/ip label value if hostNetwork cluster.

The member service cluster IP will be set to None to avoid confusion on IP used as member address for hostNetwork clusters.

Member service are created when a reconcile request from the statefulset is issued.
As the pod can still be in pending state the member service could be created without the scylla/ip label set.
When the pod state change to Running the PodIP can be set but no reconcile request will be emitted by the statefulset as there will be no update on the members.
In order to update the member service with the this PodIP we will have to add a watcher on pod events with a predicate to filter events for pods with label app.kubernetes.io/managed-by=scylla-operator.
Those events will be managed by a specific events handler to be able to retrieve statefulset owner and then the scyllacluster owner.


### Providing multi dc feature

A specific field multiDcCluster will be added to the ScyllaCluster CRD in order to enable multi dc feature on a cluster.

2 properties will be added to that field:

- list of seeds (ip or dns entries) that will be used as remote seeds to use to bootstrap multi dc clusters.
- init cluster boolean that will indicate if the current cluster is the first cluster from the multi dc cluster. This first cluster will be allowed to bootstrap without the remote seeds as it will be the reference clusters for other dc members.

For each entry of the seeds list a multi dc service will be created.
This creation will be managed by a specific step in the sync function before cluster member services.
Each multi dc services will be created with the scylla/ip label storing the related ip/dns of the remote seed and a specific scylla/multi-dc-seed label to easily select those multi dc seeds service on the sidecar operator.

The operator will have to manage multi dc bootstrap in 2 different ways depending on the init cluster value.

- If the cluster is the init cluster the sidecar operator will not have to take multi dc service when selecting seeds in GetSeeds function. In fact this cluster will only rey on the local seed during bootstrap and run.
- If the cluster is not the init cluster the sidecar operator will have to enforce the bootstrap of the cluster from the remote seed. It will only select the multi dc service and won't take in account local one during the bootstrap in GetSeeds function.
Once the cluster is boostrap successfully it will rely back on local seeds only.
In order to know if the cluster has been well bootstrap a new Status parameter bootstrap will be added to the ScyllaCluster CRD. This field will contains a specific string ("ongoing" or "finished") so the sidecar can rely on this information to do appropriate seeds selection.
The status will be updated by the operator when updating status in the updateStatus function. The boostratp state will be set to finished once all pods will be in ready state for all required statefulsets. The state will never been rollbacked by the operator once set to finished.
