// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package helmreconciler

import (
	"context"
	"fmt"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"istio.io/api/operator/v1alpha1"
	v1alpha12 "istio.io/istio/operator/pkg/apis/istio/v1alpha1"
	"istio.io/istio/operator/pkg/name"
	"istio.io/istio/operator/pkg/object"
	"istio.io/istio/operator/pkg/translate"
	"istio.io/istio/operator/pkg/util"
	binversion "istio.io/istio/operator/version"
	"istio.io/pkg/log"
)

const (
	pollTimeout  = 100 * time.Second
	pollInterval = 2 * time.Second
)

// HelmReconciler reconciles resources rendered by a set of helm charts for a specific instances of a custom resource,
// or deletes all resources associated with a specific instance of a custom resource.
type HelmReconciler struct {
	client             client.Client
	customizer         RenderingCustomizer
	instance           runtime.Object
	needUpdateAndPrune bool
}

// NewHelmReconciler creates a HelmReconciler and returns a ptr to it
func NewHelmReconciler(instance runtime.Object, customizer RenderingCustomizer, client client.Client) *HelmReconciler {
	return &HelmReconciler{
		instance:   instance,
		client:     client,
		customizer: customizer,
	}
}

// Factory is a factory for creating HelmReconciler objects using the specified CustomizerFactory.
type Factory struct {
	// CustomizerFactory is a factory for creating the Customizer object for the HelmReconciler.
	CustomizerFactory RenderingCustomizerFactory
}

// New Returns a new HelmReconciler for the custom resource.
// instance is the custom resource to be reconciled/deleted.
// client is the kubernetes client
// logger is the logger
func (f *Factory) New(instance runtime.Object, client client.Client) (*HelmReconciler, error) {
	delegate, err := f.CustomizerFactory.NewCustomizer(instance)
	if err != nil {
		return nil, err
	}
	wrappedcustomizer, err := wrapCustomizer(instance, delegate)
	if err != nil {
		return nil, err
	}
	reconciler := &HelmReconciler{client: client, customizer: wrappedcustomizer, instance: instance, needUpdateAndPrune: true}
	wrappedcustomizer.RegisterReconciler(reconciler)
	return reconciler, nil
}

// wrapCustomizer creates a new internalCustomizer object wrapping the delegate, by inject a LoggingRenderingListener,
// an OwnerReferenceDecorator, and a PruningDetailsDecorator into a CompositeRenderingListener that includes the listener
// from the delegate.  This ensures the HelmReconciler can properly implement pruning, etc.
// instance is the custom resource to be processed by the HelmReconciler
// delegate is the delegate
func wrapCustomizer(instance runtime.Object, delegate RenderingCustomizer) (*SimpleRenderingCustomizer, error) {
	ownerReferenceDecorator, err := NewOwnerReferenceDecorator(instance)
	if err != nil {
		return nil, err
	}
	return &SimpleRenderingCustomizer{
		InputValue:          delegate.Input(),
		PruningDetailsValue: delegate.PruningDetails(),
		ListenerValue: &CompositeRenderingListener{
			Listeners: []RenderingListener{
				&LoggingRenderingListener{Level: 1},
				ownerReferenceDecorator,
				NewPruningMarkingsDecorator(delegate.PruningDetails()),
				delegate.Listener(),
			},
		},
	}, nil
}

// Reconcile the resources associated with the custom resource instance.
func (h *HelmReconciler) Reconcile() error {
	// any processing required before processing the charts
	err := h.customizer.Listener().BeginReconcile(h.instance)
	if err != nil {
		return err
	}

	// render charts
	manifestMap, err := h.RenderCharts(h.customizer.Input())
	if err != nil {
		// TODO: this needs to update status to RECONCILING.
		return err
	}

	status := h.processRecursive(manifestMap)

	// Delete any resources not in the manifest but managed by operator.
	var errs util.Errors
	if h.needUpdateAndPrune {
		errs = util.AppendErr(errs, h.Prune(allObjectHashes(manifestMap), false))
	}
	errs = util.AppendErr(errs, h.customizer.Listener().EndReconcile(h.instance, status))

	return errs.ToError()
}

// processRecursive processes the given manifests in an order of dependencies defined in h. Dependencies are a tree,
// where a child must wait for the parent to complete before starting.
func (h *HelmReconciler) processRecursive(manifests ChartManifestsMap) *v1alpha1.InstallStatus {
	deps, dch := h.customizer.Input().GetProcessingOrder(manifests)
	componentStatus := make(map[string]*v1alpha1.InstallStatus_VersionStatus)

	// mu protects the shared InstallStatus componentStatus across goroutines
	var mu sync.Mutex
	// wg waits for all manifest processing goroutines to finish
	var wg sync.WaitGroup

	for c, m := range manifests {
		c, m := c, m
		wg.Add(1)
		go func() {
			defer wg.Done()
			cn := name.ComponentName(c)
			if s := dch[cn]; s != nil {
				log.Infof("%s is waiting on dependency...", c)
				<-s
				log.Infof("Dependency for %s has completed, proceeding.", c)
			}

			// Set status when reconciling starts
			status := v1alpha1.InstallStatus_RECONCILING
			mu.Lock()
			if _, ok := componentStatus[c]; !ok {
				componentStatus[c] = &v1alpha1.InstallStatus_VersionStatus{}
				componentStatus[c].Status = status
			}
			mu.Unlock()

			// Process manifests and get the status result
			errString := ""
			if len(m) == 0 {
				status = v1alpha1.InstallStatus_NONE
			} else {
				status = v1alpha1.InstallStatus_HEALTHY
				if cnt, err := h.ProcessManifest(m[0]); err != nil {
					errString = err.Error()
					status = v1alpha1.InstallStatus_ERROR
				} else if cnt == 0 {
					status = v1alpha1.InstallStatus_NONE
				}
			}

			// Update status based on the result
			mu.Lock()
			if status == v1alpha1.InstallStatus_NONE {
				delete(componentStatus, c)
			} else {
				componentStatus[c].Status = status
				if errString != "" {
					componentStatus[c].Error = errString
				}
			}
			mu.Unlock()

			// Signal all the components that depend on us.
			for _, ch := range deps[cn] {
				log.Infof("Unblocking dependency %s.", ch)
				dch[ch] <- struct{}{}
			}
		}()
	}
	wg.Wait()

	// Update overall status
	// - If all components are HEALTHY, overall status is HEALTHY.
	// - If one or more components are RECONCILING and others are HEALTHY, overall status is RECONCILING.
	// - If one or more components are UPDATING and others are HEALTHY, overall status is UPDATING.
	// - If components are a mix of RECONCILING, UPDATING and HEALTHY, overall status is UPDATING.
	// - If any component is in ERROR state, overall status is ERROR.
	overallStatus := v1alpha1.InstallStatus_HEALTHY
	for _, cs := range componentStatus {
		if cs.Status == v1alpha1.InstallStatus_ERROR {
			overallStatus = v1alpha1.InstallStatus_ERROR
			break
		} else if cs.Status == v1alpha1.InstallStatus_UPDATING {
			overallStatus = v1alpha1.InstallStatus_UPDATING
			break
		} else if cs.Status == v1alpha1.InstallStatus_RECONCILING {
			overallStatus = v1alpha1.InstallStatus_RECONCILING
			break
		}
	}
	// update status further based on the in cluster resources status if manifests processed successfully,
	// otherwise just use status obtained from processing manifests.
	if overallStatus == v1alpha1.InstallStatus_HEALTHY {
		err := h.checkResourceStatus(&componentStatus, &overallStatus)
		if err != nil {
			log.Errorf("failed to check resource status %v", err)
		}
	}
	out := &v1alpha1.InstallStatus{
		Status:          overallStatus,
		ComponentStatus: componentStatus,
	}

	return out
}

// checkResourceStatus check and wait for resource to be ready,
// update overallStatus and componentStatus correspondingly
func (h *HelmReconciler) checkResourceStatus(componentStatus *map[string]*v1alpha1.InstallStatus_VersionStatus,
	overallStatus *v1alpha1.InstallStatus_Status) error {
	cs := h.client
	iop := h.GetInstance().(*v1alpha12.IstioOperator)
	if iop == nil {
		return fmt.Errorf("failed to get IstioOperator instance")
	}
	t, err := translate.NewTranslator(binversion.OperatorBinaryVersion.MinorVersion)
	if err != nil {
		return err
	}
	errPoll := wait.Poll(pollInterval, pollTimeout, func() (bool, error) {
		for cn := range *componentStatus {
			cnMap, ok := t.ComponentMaps[name.ComponentName(cn)]
			if ok && cnMap.ResourceName != "" {
				dp := &appsv1.Deployment{}
				key := client.ObjectKey{Namespace: iop.Namespace, Name: cnMap.ResourceName}
				err := cs.Get(context.TODO(), key, dp)
				if err != nil {
					log.Warnf("deployment: %v not found", cnMap.ResourceName)
					continue
				}

				if dp.Status.ReadyReplicas != dp.Status.UnavailableReplicas+dp.Status.AvailableReplicas {
					(*componentStatus)[cn].Status = v1alpha1.InstallStatus_UPDATING
					return false, nil
				}
			}
			(*componentStatus)[cn].Status = v1alpha1.InstallStatus_HEALTHY
		}

		podList := &v1.PodList{}
		err := cs.List(context.TODO(), podList, client.InNamespace(iop.Namespace))
		if err != nil {
			return false, err
		}
		for _, pod := range podList.Items {
			if len(pod.Status.Conditions) > 0 {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == v1.PodReady && condition.Status != v1.ConditionTrue {
						return false, nil
					}
				}
			}
		}
		return true, nil
	})
	if errPoll != nil {
		*overallStatus = v1alpha1.InstallStatus_UPDATING
	}
	return nil
}

// Delete resources associated with the custom resource instance
func (h *HelmReconciler) Delete() error {
	h.needUpdateAndPrune = true
	allErrors := []error{}

	// any processing required before processing the charts
	err := h.customizer.Listener().BeginDelete(h.instance)
	if err != nil {
		allErrors = append(allErrors, err)
	}

	err = h.customizer.Listener().BeginPrune(true)
	if err != nil {
		allErrors = append(allErrors, err)
	}
	err = h.Prune(nil, true)
	if err != nil {
		allErrors = append(allErrors, err)
	}
	err = h.customizer.Listener().EndPrune()
	if err != nil {
		allErrors = append(allErrors, err)
	}

	// any post processing required after deleting
	err = utilerrors.NewAggregate(allErrors)
	if listenerErr := h.customizer.Listener().EndDelete(h.instance, err); listenerErr != nil {
		log.Errorf("error calling listener: %s", listenerErr)
	}

	// return any errors
	return err
}

// allObjectHashes returns a map with object hashes of all the objects contained in cmm as the keys.
func allObjectHashes(cmm ChartManifestsMap) map[string]bool {
	ret := make(map[string]bool)
	for _, mm := range cmm {
		for _, m := range mm {
			objs, err := object.ParseK8sObjectsFromYAMLManifest(m.Content)
			if err != nil {
				log.Error(err.Error())
			}
			for _, o := range objs {
				ret[o.Hash()] = true
			}
		}
	}
	return ret
}

// GetClient returns the kubernetes client associated with this HelmReconciler
func (h *HelmReconciler) GetClient() client.Client {
	return h.client
}

// GetInstance returns the instance associated with this HelmReconciler
func (h *HelmReconciler) GetInstance() runtime.Object {
	return h.instance
}

// SetInstance set the instance associated with this HelmReconciler
func (h *HelmReconciler) SetInstance(instance runtime.Object) {
	h.instance = instance
}

// SetNeedUpdateAndPrune set the needUpdateAndPrune flag associated with this HelmReconciler
func (h *HelmReconciler) SetNeedUpdateAndPrune(u bool) {
	h.needUpdateAndPrune = u
}
