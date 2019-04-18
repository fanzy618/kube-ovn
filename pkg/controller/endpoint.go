package controller

import (
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

func (c *Controller) enqueueAddEndpoint(obj interface{}) {
	if !c.isLeader() {
		return
	}
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.updateEndpointQueue.AddRateLimited(key)
}

func (c *Controller) enqueueUpdateEndpoint(old, new interface{}) {
	if !c.isLeader() {
		return
	}
	oldEp := old.(*v1.Endpoints)
	newEp := new.(*v1.Endpoints)
	if oldEp.ResourceVersion == newEp.ResourceVersion {
		return
	}

	if len(oldEp.Subsets) == 0 && len(newEp.Subsets) == 0 {
		return
	}

	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(new); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.updateEndpointQueue.AddRateLimited(key)
}

func (c *Controller) runUpdateEndpointWorker() {
	for c.processNextUpdateEndpointWorkItem() {
	}
}

func (c *Controller) processNextUpdateEndpointWorkItem() bool {
	obj, shutdown := c.updateEndpointQueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.updateEndpointQueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.updateEndpointQueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		if err := c.handleUpdateEndpoint(key); err != nil {
			c.updateEndpointQueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.updateEndpointQueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}
	return true
}

func (c *Controller) handleUpdateEndpoint(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}
	klog.Infof("update endpoint %s/%s", namespace, name)

	ep, err := c.endpointsLister.Endpoints(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	svc, err := c.servicesLister.Services(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	clusterIP := svc.Spec.ClusterIP
	if clusterIP == "" || clusterIP == v1.ClusterIPNone {
		return nil
	}

	backends := []string{}
	nameToPort := map[string]int32{}
	for _, subset := range ep.Subsets {
		for _, addr := range subset.Addresses {
			if addr.IP != "" {
				backends = append(backends, addr.IP)
			}
		}
		for _, port := range subset.Ports {
			nameToPort[port.Name] = port.Port
		}
	}

	for _, port := range svc.Spec.Ports {
		vip := fmt.Sprintf("%s:%d", clusterIP, port.Port)
		var targetPort int32
		if port.TargetPort.IntVal != 0 {
			targetPort = port.TargetPort.IntVal
		} else {
			port, ok := nameToPort[port.TargetPort.StrVal]
			if !ok {
				continue
			}
			targetPort = port
		}
		if port.Protocol == v1.ProtocolTCP {
			// for performance reason delete lb with no backends
			if len(backends) > 0 {
				err = c.ovnClient.CreateLoadBalancerRule(c.config.ClusterTcpLoadBalancer, vip, convertIpToAddress(backends, targetPort))
				if err != nil {
					klog.Errorf("failed to update vip %s to tcp lb, %v", vip, err)
					return err
				}
			} else {
				err = c.ovnClient.DeleteLoadBalancerVip(vip, c.config.ClusterTcpLoadBalancer)
				if err != nil {
					klog.Errorf("failed to delete vip %s at tcp lb, %v", vip, err)
					return err
				}
			}
		} else {
			if len(backends) > 0 {
				err = c.ovnClient.CreateLoadBalancerRule(c.config.ClusterUdpLoadBalancer, vip, convertIpToAddress(backends, targetPort))
				if err != nil {
					klog.Errorf("failed to update vip %s to udp lb, %v", vip, err)
					return err
				}
			} else {
				err = c.ovnClient.DeleteLoadBalancerVip(vip, c.config.ClusterUdpLoadBalancer)
				if err != nil {
					klog.Errorf("failed to delete vip %s at udp lb, %v", vip, err)
					return err
				}
			}
		}
	}
	return nil
}

func convertIpToAddress(backends []string, targetPort int32) string {
	addresses := make([]string, 0, len(backends))
	for _, backend := range backends {
		address := fmt.Sprintf("%s:%d", backend, targetPort)
		addresses = append(addresses, address)
	}
	return strings.Join(addresses, ",")
}
