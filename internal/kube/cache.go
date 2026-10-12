package kube

import (
	"context"
	"log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
)

type vmCache struct {
	vms, vmis cache.Store
}

func (c *Client) mirror(ctx context.Context, gvr schema.GroupVersionResource) (cache.Store, func() bool) {
	r := c.dyn.Resource(gvr)
	lw := &cache.ListWatch{
		ListWithContextFunc:  func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) { return r.List(ctx, o) },
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) { return r.Watch(ctx, o) },
	}
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	ref := cache.NewReflector(lw, &unstructured.Unstructured{}, store, 0)
	go ref.RunWithContext(ctx)
	return store, func() bool { return ref.LastSyncResourceVersion() != "" }
}

func (c *Client) WatchVMs(ctx context.Context) {
	vms, vmsSynced := c.mirror(ctx, vmGVR)
	vmis, vmisSynced := c.mirror(ctx, vmiGVR)
	if cache.WaitForCacheSync(ctx.Done(), vmsSynced, vmisSynced) {
		c.cache.Store(&vmCache{vms, vmis})
		log.Printf("VM cache synced")
	}
}

func (vc *vmCache) list() ([]*unstructured.Unstructured, map[string]*unstructured.Unstructured, error) {
	out := []*unstructured.Unstructured{}
	for _, o := range vc.vms.List() {
		out = append(out, o.(*unstructured.Unstructured))
	}
	inst := map[string]*unstructured.Unstructured{}
	for _, o := range vc.vmis.List() {
		u := o.(*unstructured.Unstructured)
		inst[u.GetNamespace()+"/"+u.GetName()] = u
	}
	return out, inst, nil
}
