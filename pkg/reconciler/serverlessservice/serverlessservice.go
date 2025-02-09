/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package serverlessservice

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	sksreconciler "knative.dev/networking/pkg/client/injection/reconciler/networking/v1alpha1/serverlessservice"

	netv1alpha1 "knative.dev/networking/pkg/apis/networking/v1alpha1"
	"knative.dev/pkg/hash"
	"knative.dev/pkg/logging"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/pkg/system"
	"knative.dev/serving/pkg/networking"
	"knative.dev/serving/pkg/reconciler/serverlessservice/resources"
	"knative.dev/serving/pkg/reconciler/serverlessservice/resources/names"
	presources "knative.dev/serving/pkg/resources"
)

// reconciler implements controller.Reconciler for Service resources.
type reconciler struct {
	kubeclient kubernetes.Interface

	// listers index properties about resources
	serviceLister   corev1listers.ServiceLister
	endpointsLister corev1listers.EndpointsLister

	// Used to get PodScalables from object references.
	listerFactory func(schema.GroupVersionResource) (cache.GenericLister, error)
}

// Check that our Reconciler implements Interface
var _ sksreconciler.Interface = (*reconciler)(nil)

// Reconcile compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Revision resource
// with the current status of the resource.
func (r *reconciler) ReconcileKind(ctx context.Context, sks *netv1alpha1.ServerlessService) pkgreconciler.Event {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	logger := logging.FromContext(ctx)
	// Don't reconcile if we're being deleted.
	if sks.GetDeletionTimestamp() != nil {
		return nil
	}

	for i, fn := range []func(context.Context, *netv1alpha1.ServerlessService) error{
		r.reconcilePrivateService, // First make sure our data source is setup.
		r.reconcilePublicService,
		r.reconcilePublicEndpoints,
	} {
		if err := fn(ctx, sks); err != nil {
			logger.Debugw(strconv.Itoa(i)+": reconcile failed", zap.Error(err))
			return err
		}
	}
	return nil
}

func (r *reconciler) reconcilePublicService(ctx context.Context, sks *netv1alpha1.ServerlessService) error {
	logger := logging.FromContext(ctx)

	sn := sks.Name
	srv, err := r.serviceLister.Services(sks.Namespace).Get(sn)
	if apierrs.IsNotFound(err) {
		logger.Info("K8s public service does not exist; creating.")
		// We've just created the service, so it has no endpoints.
		sks.Status.MarkEndpointsNotReady("CreatingPublicService")
		srv = resources.MakePublicService(sks)
		_, err := r.kubeclient.CoreV1().Services(sks.Namespace).Create(ctx, srv, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create public K8s Service: %w", err)
		}
		logger.Info("Created public K8s service: ", sn)
	} else if err != nil {
		return fmt.Errorf("failed to get public K8s Service: %w", err)
	} else if !metav1.IsControlledBy(srv, sks) {
		sks.Status.MarkEndpointsNotOwned("Service", sn)
		return fmt.Errorf("SKS: %s does not own Service: %s", sks.Name, sn)
	} else {
		tmpl := resources.MakePublicService(sks)
		want := srv.DeepCopy()
		want.Spec.Ports = tmpl.Spec.Ports
		want.Spec.Selector = nil

		if !equality.Semantic.DeepEqual(want.Spec, srv.Spec) {
			logger.Info("Public K8s Service changed; reconciling: ", sn, cmp.Diff(want.Spec, srv.Spec))
			if _, err = r.kubeclient.CoreV1().Services(sks.Namespace).Update(ctx, want, metav1.UpdateOptions{}); err != nil {
				return fmt.Errorf("failed to update public K8s Service: %w", err)
			}
		}
	}
	sks.Status.ServiceName = sn
	logger.Debug("Done reconciling public K8s service: ", sn)
	return nil
}

// subsetEndpoints computes a subset of all endpoints of size `n` using a consistent
// selection algorithm. For non empty input, subsetEndpoints returns a copy of the
// input with the irrelevant endpoints and empty subsets filtered out, if the input
// size is larger than `n`,
// Otherwise the input is returned as is.
// `target` is the revision name for which we are computing a subset.
func subsetEndpoints(eps *corev1.Endpoints, target string, n int) *corev1.Endpoints {
	// n == 0 means all, and if there are no subsets there's no work to do either.
	if len(eps.Subsets) == 0 || n == 0 {
		return eps
	}

	addrs := make(sets.String, len(eps.Subsets[0].Addresses))
	for _, ss := range eps.Subsets {
		for _, addr := range ss.Addresses {
			addrs.Insert(addr.IP)
		}
	}

	// The input is not larger than desired.
	if len(addrs) <= n {
		return eps
	}

	selection := hash.ChooseSubset(addrs, n, target)

	// Copy the informer's copy, so we can filter it out.
	neps := eps.DeepCopy()
	// Standard in place filter using read and write indices.
	// This preserves the original object order.
	r, w := 0, 0
	for r < len(neps.Subsets) {
		ss := neps.Subsets[r]
		// And same algorithm internally.
		ra, wa := 0, 0
		for ra < len(ss.Addresses) {
			if selection.Has(ss.Addresses[ra].IP) {
				ss.Addresses[wa] = ss.Addresses[ra]
				wa++
			}
			ra++
		}
		// At least one address from the subset was preserved, so keep it.
		if wa > 0 {
			ss.Addresses = ss.Addresses[:wa]
			// At least one address from the subset was preserved, so keep it.
			neps.Subsets[w] = ss
			w++
		}
		r++
	}
	// We are guaranteed here to have w > 0, because
	// 0. There's at least one subset (checked above).
	// 1. A subset cannot be empty (k8s validation).
	// 2. len(addrs) is at least as big as n
	// Thus there's at least 1 non empty subset (and for all intents and purposes we'll have 1 always).
	neps.Subsets = neps.Subsets[:w]
	return neps
}

func (r *reconciler) reconcilePublicEndpoints(ctx context.Context, sks *netv1alpha1.ServerlessService) error {
	logger := logging.FromContext(ctx)
	dlogger := logger.Desugar()

	var (
		srcEps                *corev1.Endpoints
		foundServingEndpoints bool
	)
	activatorEps, err := r.endpointsLister.Endpoints(system.Namespace()).Get(networking.ActivatorServiceName)
	if err != nil {
		return fmt.Errorf("failed to get activator service endpoints: %w", err)
	}
	if dlogger.Core().Enabled(zap.DebugLevel) {
		// Spew is expensive and there might be a lof of activator endpoints.
		dlogger.Debug("Activator endpoints: " + spew.Sprint(activatorEps.Subsets))
	}

	psn := sks.Status.PrivateServiceName
	pvtEps, err := r.endpointsLister.Endpoints(sks.Namespace).Get(psn)
	if err != nil {
		return fmt.Errorf("failed to get private K8s Service endpoints: %w", err)
	}

	// We still might be "ready" even if in proxy mode,
	// if proxy mode is by means of burst capacity handling.
	pvtReady := presources.ReadyAddressCount(pvtEps)
	sharedReady := presources.ReadyAddressCount(activatorEps)

	logger.Infof("SKS is in %s mode; has %d endpoints in %s; %d activator endpoints",
		sks.Spec.Mode, pvtReady, psn, sharedReady)

	// Spew is expensive and there might be a lof of endpoints.
	if dlogger.Core().Enabled(zap.DebugLevel) {
		dlogger.Debug("Private endpoints: " + spew.Sprint(pvtEps.Subsets))
		dlogger.Debug(fmt.Sprintf("Subset of activator endpoints (needed %d): %s",
			sks.Spec.NumActivators, spew.Sprint(activatorEps)))
	}

	// The logic below is as follows:
	// mode = sks.Spec.Mode
	// if len(private_service_endpoints) == 0:
	//    // Override the mode to Proxy if the service has no endpoints
	//    mode = Proxy
	// if len(activator_endpoints) == 0:
	//    // Override the mode to Serve if the shared activator has no
	//    // endpoints.
	//    mode = Serve
	//
	// if mode == serve:
	//    srcEps = private_service_endpoints
	// else:
	//    srcEps = subset(activator_endpoints)
	// The reason for this is, we don't want to leave the public service endpoints empty,
	// since those endpoints are the ones programmed into the VirtualService.
	mode := sks.Spec.Mode
	if pvtReady == 0 {
		logger.Info("Forcing SKS into Proxy mode, insufficient ready endpoints.")
		mode = netv1alpha1.SKSOperationModeProxy
	} else {
		foundServingEndpoints = true
	}
	if sharedReady == 0 {
		logger.Info("Forcing SKS into Serve mode, no activator endpoints.")
		mode = netv1alpha1.SKSOperationModeServe
	}
	switch mode {
	case netv1alpha1.SKSOperationModeServe:
		srcEps = pvtEps
	case netv1alpha1.SKSOperationModeProxy:
		srcEps = subsetEndpoints(activatorEps, sks.Name, int(sks.Spec.NumActivators))
	}

	sn := sks.Name
	eps, err := r.endpointsLister.Endpoints(sks.Namespace).Get(sn)

	if apierrs.IsNotFound(err) {
		dlogger.Info("Public endpoints object does not exist; creating.")
		sks.Status.MarkEndpointsNotReady("CreatingPublicEndpoints")
		if _, err = r.kubeclient.CoreV1().Endpoints(sks.Namespace).Create(ctx, resources.MakePublicEndpoints(sks, srcEps), metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("failed to create public K8s Endpoints: %w", err)
		}
		dlogger.Info("Created K8s Endpoints: " + sn)
	} else if err != nil {
		return fmt.Errorf("failed to get public K8s Endpoints: %w", err)
	} else if !metav1.IsControlledBy(eps, sks) {
		sks.Status.MarkEndpointsNotOwned("Endpoints", sn)
		return fmt.Errorf("SKS: %s does not own Endpoints: %s", sks.Name, sn)
	} else {
		wantSubsets := resources.FilterSubsetPorts(sks, srcEps.Subsets)
		if !equality.Semantic.DeepEqual(wantSubsets, eps.Subsets) {
			want := eps.DeepCopy()
			want.Subsets = wantSubsets
			logger.Info("Public K8s Endpoints changed; reconciling: ", sn)
			if _, err = r.kubeclient.CoreV1().Endpoints(sks.Namespace).Update(ctx, want, metav1.UpdateOptions{}); err != nil {
				return fmt.Errorf("failed to update public K8s Endpoints: %w", err)
			}
		}
	}
	if foundServingEndpoints {
		sks.Status.MarkEndpointsReady()
	} else {
		dlogger.Info("No ready endpoints backing revision")
		sks.Status.MarkEndpointsNotReady("NoHealthyBackends")
	}
	// If we have no backends or if we're in the proxy mode, then
	// activator backs this revision.
	if !foundServingEndpoints || sks.Spec.Mode == netv1alpha1.SKSOperationModeProxy {
		sks.Status.MarkActivatorEndpointsPopulated()
	} else {
		sks.Status.MarkActivatorEndpointsRemoved()
	}

	dlogger.Debug("Done reconciling public K8s endpoints")
	return nil
}

func (r *reconciler) reconcilePrivateService(ctx context.Context, sks *netv1alpha1.ServerlessService) error {
	logger := logging.FromContext(ctx)

	selector, err := r.getSelector(sks)
	if err != nil {
		return fmt.Errorf("error retrieving deployment selector spec: %w", err)
	}

	sn := names.PrivateService(sks.Name)
	svc, err := r.serviceLister.Services(sks.Namespace).Get(sn)
	if apierrs.IsNotFound(err) {
		logger.Info("SKS has no private service; creating.")
		sks.Status.MarkEndpointsNotReady("CreatingPrivateService")
		svc = resources.MakePrivateService(sks, selector)
		svc, err = r.kubeclient.CoreV1().Services(sks.Namespace).Create(ctx, svc, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create private K8s Service: %w", err)
		}
		logger.Info("Created private K8s service: ", svc.Name)
	} else if err != nil {
		return fmt.Errorf("failed to get private K8s Service: %w", err)
	} else if !metav1.IsControlledBy(svc, sks) {
		sks.Status.MarkEndpointsNotOwned("Service", svc.Name)
		return fmt.Errorf("SKS: %s does not own Service: %s", sks.Name, svc.Name)
	} else {
		tmpl := resources.MakePrivateService(sks, selector)
		want := svc.DeepCopy()
		// Our controller manages only part of spec, so set the fields we own.
		want.Spec.Ports = tmpl.Spec.Ports
		want.Spec.Selector = tmpl.Spec.Selector

		if !equality.Semantic.DeepEqual(svc.Spec, want.Spec) {
			// Spec has only public fields and cmp can't panic here.
			logger.Debug("Private service diff(-want,+got):", cmp.Diff(want.Spec, svc.Spec))
			sks.Status.MarkEndpointsNotReady("UpdatingPrivateService")
			logger.Info("Reconciling a changed private K8s Service  ", svc.Name)
			if _, err = r.kubeclient.CoreV1().Services(sks.Namespace).Update(ctx, want, metav1.UpdateOptions{}); err != nil {
				return fmt.Errorf("failed to update private K8s Service: %w", err)
			}
		}
	}

	sks.Status.PrivateServiceName = svc.Name
	logger.Debug("Done reconciling private K8s service: ", svc.Name)
	return nil
}

func (r *reconciler) getSelector(sks *netv1alpha1.ServerlessService) (map[string]string, error) {
	scale, err := presources.GetScaleResource(sks.Namespace, sks.Spec.ObjectRef, r.listerFactory)
	if err != nil {
		return nil, err
	}
	return scale.Spec.Selector.MatchLabels, nil
}
