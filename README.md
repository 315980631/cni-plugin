# HC-Calico

This project is based on  [Calico Networking for CNI](https://github.com/projectcalico/cni-plugin), and is designed for managing IP address by service-level in large-scale scene. When enabling service-level IP address management in Calico BGP mode, the route entry will change from node level to pod level, and EBGP mode can be used to process all routes by switch devices. At present, the scale of routing table, the number of BGP neighbors and the speed of routing convergence are close to the bottleneck when the L3 network of enterprise data center switches for cloud computing contains 1000-2000 nodes.

In `HC-Calico`, we introduced two new features: `FixedIP` and `ServiceIPPool`. 

## FixedIP

Calico provides `Specific IP` to meet the requirement of stable IP addresses for pods. To use `Specific IP`, we need to annotate pod with `cni.projectcalico.org/ipAddrs: <ipaddr>`, but it costs a lot of time to annotate each pod. Besides, manually annotating IP address for pod is inappropriate for `statefulset`.

`FixedIP` was proposed to solve this problem. To use `Fixed IP`, we need to annotate `fixed.ipam.harmonycloud.cn=true` to pod template of `statefulset`. Then each pod in this `statefulset` will get its unique IP address automatically, and the IP address will be stored in `configmap` to ensure it won't change after pod restart.

## ServiceIPPool

Calico provides [`IPPool`](https://docs.projectcalico.org/reference/resources/ippool), which represents a collection of IP addresses from which Calico expects endpoint IPs to be assigned. When the IP address from `IPPool` is not enough and there is no continuous large collection of IP address, we need to combine multiple small collection of IP addresses into an large `IPPool`. 

However, the `cidr` spec in `IPPool` doesn't support array of cidrs, Calico provides `cni.projectcalico.org/ipv4pools ` annotation in pod template to [assign multiple separate `IPPools`](https://docs.projectcalico.org/networking/legacy-firewalls). The change of pod template results in service rolling update, which affects the stability of the ·service.

Thus, we modified some parts of Calico-CNI and provide a new CRD called `ServiceIPPool`.  

```yaml
apiVersion: harmonycloud.cn/v1alpha1
kind: ServiceIPPool
metadata:
  name: svc1
  namespace: kube-system
spec:
  ipv4poolList:
    - ippool1
    - ippool2
  disabledIPv4PoolList:
    - ippool3
    - ippool4
  ipv6poolList:
    - ippool5
    - ippool6
  disabledIPv6PoolList:
    - ippool7
    - ippool8
```

Take the example above,  a `ServiceIPPool` contains multiple Calico `IPPools`. We can use `serviceIPPoolcrd` annotation in pod template to assign a `ServiceIPPool` to a service. Then the change of `IPPools` in `ServiceIPPool` won't result in service rolling update.

