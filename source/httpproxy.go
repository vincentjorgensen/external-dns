/*
Copyright 2017 The Kubernetes Authors.

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

package source

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"text/template"
	"time"

	contourapi "github.com/projectcontour/contour/apis/projectcontour/v1"
	contour "github.com/projectcontour/contour/apis/generated/clientset/versioned"
	contourinformers "github.com/projectcontour/contour/apis/generated/informers/externalversions"
	extinformers "github.com/projectcontour/contour/apis/generated/informers/externalversions/projectcontour/v1"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"sigs.k8s.io/external-dns/endpoint"
)

// HTTPProxySource is an implementation of Source for ProjectContour HTTPProxy objects.
// The HTTPProxy implementation uses the spec.virtualHost.fqdn value for the hostname.
// Use targetAnnotationKey to explicitly set Endpoint.
type HTTPProxySource struct {
	kubeClient                 kubernetes.Interface
	contourClient              contour.Interface
	contourLoadBalancerService string
	namespace                  string
	annotationFilter           string
	fqdnTemplate               *template.Template
	combineFQDNAnnotation      bool
	ignoreHostnameAnnotation   bool
	HTTPProxyInformer       extinformers.HTTPProxyInformer
}

// NewContourHTTPProxySource creates a new contourHTTPProxySource with the given config.
func NewContourHTTPProxySource(
	kubeClient kubernetes.Interface,
	contourClient contour.Interface,
	contourLoadBalancerService string,
	namespace string,
	annotationFilter string,
	fqdnTemplate string,
	combineFqdnAnnotation bool,
	ignoreHostnameAnnotation bool,
) (Source, error) {
	var (
		tmpl *template.Template
		err  error
	)
	if fqdnTemplate != "" {
		tmpl, err = template.New("endpoint").Funcs(template.FuncMap{
			"trimPrefix": strings.TrimPrefix,
		}).Parse(fqdnTemplate)
		if err != nil {
			return nil, err
		}
	}

	if _, _, err = parseContourLoadBalancerService(contourLoadBalancerService); err != nil {
		return nil, err
	}

	// Use shared informer to listen for add/update/delete of ingressroutes in the specified namespace.
	// Set resync period to 0, to prevent processing when nothing has changed.
	informerFactory := contourinformers.NewSharedInformerFactoryWithOptions(
		contourClient,
		0,
		contourinformers.WithNamespace(namespace),
	)
	HTTPProxyInformer := informerFactory.Contour().V1beta1().HTTPProxys()

	// Add default resource event handlers to properly initialize informer.
	HTTPProxyInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
			},
		},
	)

	// TODO informer is not explicitly stopped since controller is not passing in its channel.
	informerFactory.Start(wait.NeverStop)

	// wait for the local cache to be populated.
	err = wait.Poll(time.Second, 60*time.Second, func() (bool, error) {
		return HTTPProxyInformer.Informer().HasSynced() == true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to sync cache: %v", err)
	}

	return &HTTPProxySource{
		kubeClient:                 kubeClient,
		contourClient:              contourClient,
		contourLoadBalancerService: contourLoadBalancerService,
		namespace:                  namespace,
		annotationFilter:           annotationFilter,
		fqdnTemplate:               tmpl,
		combineFQDNAnnotation:      combineFqdnAnnotation,
		ignoreHostnameAnnotation:   ignoreHostnameAnnotation,
		HTTPProxyInformer:       HTTPProxyInformer,
	}, nil
}

// Endpoints returns endpoint objects for each host-target combination that should be processed.
// Retrieves all ingressroute resources in the source's namespace(s).
func (sc *HTTPProxySource) Endpoints() ([]*endpoint.Endpoint, error) {
	irs, err := sc.HTTPProxyInformer.Lister().HTTPProxys(sc.namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	irs, err = sc.filterByAnnotations(irs)
	if err != nil {
		return nil, err
	}

	endpoints := []*endpoint.Endpoint{}

	for _, ir := range irs {
		// Check controller annotation to see if we are responsible.
		controller, ok := ir.Annotations[controllerAnnotationKey]
		if ok && controller != controllerAnnotationValue {
			log.Debugf("Skipping ingressroute %s/%s because controller value does not match, found: %s, required: %s",
				ir.Namespace, ir.Name, controller, controllerAnnotationValue)
			continue
		} else if ir.CurrentStatus != "valid" {
			log.Debugf("Skipping ingressroute %s/%s because it is not valid", ir.Namespace, ir.Name)
			continue
		}

		irEndpoints, err := sc.endpointsFromHTTPProxy(ir)
		if err != nil {
			return nil, err
		}

		// apply template if fqdn is missing on ingressroute
		if (sc.combineFQDNAnnotation || len(irEndpoints) == 0) && sc.fqdnTemplate != nil {
			tmplEndpoints, err := sc.endpointsFromTemplate(ir)
			if err != nil {
				return nil, err
			}

			if sc.combineFQDNAnnotation {
				irEndpoints = append(irEndpoints, tmplEndpoints...)
			} else {
				irEndpoints = tmplEndpoints
			}
		}

		if len(irEndpoints) == 0 {
			log.Debugf("No endpoints could be generated from ingressroute %s/%s", ir.Namespace, ir.Name)
			continue
		}

		log.Debugf("Endpoints generated from ingressroute: %s/%s: %v", ir.Namespace, ir.Name, irEndpoints)
		sc.setResourceLabel(ir, irEndpoints)
		endpoints = append(endpoints, irEndpoints...)
	}

	for _, ep := range endpoints {
		sort.Sort(ep.Targets)
	}

	return endpoints, nil
}

func (sc *HTTPProxySource) endpointsFromTemplate(HTTPProxy *contourapi.HTTPProxy) ([]*endpoint.Endpoint, error) {
	// Process the whole template string
	var buf bytes.Buffer
	err := sc.fqdnTemplate.Execute(&buf, HTTPProxy)
	if err != nil {
		return nil, fmt.Errorf("failed to apply template on ingressroute %s/%s: %v", HTTPProxy.Namespace, HTTPProxy.Name, err)
	}

	hostnames := buf.String()

	ttl, err := getTTLFromAnnotations(HTTPProxy.Annotations)
	if err != nil {
		log.Warn(err)
	}

	targets := getTargetsFromTargetAnnotation(HTTPProxy.Annotations)

	if len(targets) == 0 {
		targets, err = sc.targetsFromContourLoadBalancer()
		if err != nil {
			return nil, err
		}
	}

	providerSpecific, setIdentifier := getProviderSpecificAnnotations(HTTPProxy.Annotations)

	var endpoints []*endpoint.Endpoint
	// splits the FQDN template and removes the trailing periods
	hostnameList := strings.Split(strings.Replace(hostnames, " ", "", -1), ",")
	for _, hostname := range hostnameList {
		hostname = strings.TrimSuffix(hostname, ".")
		endpoints = append(endpoints, endpointsForHostname(hostname, targets, ttl, providerSpecific, setIdentifier)...)
	}
	return endpoints, nil
}

// filterByAnnotations filters a list of configs by a given annotation selector.
func (sc *HTTPProxySource) filterByAnnotations(HTTPProxys []*contourapi.HTTPProxy) ([]*contourapi.HTTPProxy, error) {
	labelSelector, err := metav1.ParseToLabelSelector(sc.annotationFilter)
	if err != nil {
		return nil, err
	}
	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		return nil, err
	}

	// empty filter returns original list
	if selector.Empty() {
		return HTTPProxys, nil
	}

	filteredList := []*contourapi.HTTPProxy{}

	for _, HTTPProxy := range HTTPProxys {
		// convert the ingressroute's annotations to an equivalent label selector
		annotations := labels.Set(HTTPProxy.Annotations)

		// include ingressroute if its annotations match the selector
		if selector.Matches(annotations) {
			filteredList = append(filteredList, HTTPProxy)
		}
	}

	return filteredList, nil
}

func (sc *HTTPProxySource) setResourceLabel(HTTPProxy *contourapi.HTTPProxy, endpoints []*endpoint.Endpoint) {
	for _, ep := range endpoints {
		ep.Labels[endpoint.ResourceLabelKey] = fmt.Sprintf("ingressroute/%s/%s", HTTPProxy.Namespace, HTTPProxy.Name)
	}
}

func (sc *HTTPProxySource) targetsFromContourLoadBalancer() (targets endpoint.Targets, err error) {
	lbNamespace, lbName, err := parseContourLoadBalancerService(sc.contourLoadBalancerService)
	if err != nil {
		return nil, err
	}
	if svc, err := sc.kubeClient.CoreV1().Services(lbNamespace).Get(lbName, metav1.GetOptions{}); err != nil {
		log.Warn(err)
	} else {
		for _, lb := range svc.Status.LoadBalancer.Ingress {
			if lb.IP != "" {
				targets = append(targets, lb.IP)
			}
			if lb.Hostname != "" {
				targets = append(targets, lb.Hostname)
			}
		}
	}

	return
}

// endpointsFromHTTPProxyConfig extracts the endpoints from a Contour HTTPProxy object
func (sc *HTTPProxySource) endpointsFromHTTPProxy(HTTPProxy *contourapi.HTTPProxy) ([]*endpoint.Endpoint, error) {
	if HTTPProxy.CurrentStatus != "valid" {
		log.Warn(errors.Errorf("cannot generate endpoints for ingressroute with status %s", HTTPProxy.CurrentStatus))
		return nil, nil
	}

	var endpoints []*endpoint.Endpoint

	ttl, err := getTTLFromAnnotations(HTTPProxy.Annotations)
	if err != nil {
		log.Warn(err)
	}

	targets := getTargetsFromTargetAnnotation(HTTPProxy.Annotations)

	if len(targets) == 0 {
		targets, err = sc.targetsFromContourLoadBalancer()
		if err != nil {
			return nil, err
		}
	}

	providerSpecific, setIdentifier := getProviderSpecificAnnotations(HTTPProxy.Annotations)

	if virtualHost := HTTPProxy.Spec.VirtualHost; virtualHost != nil {
		if fqdn := virtualHost.Fqdn; fqdn != "" {
			endpoints = append(endpoints, endpointsForHostname(fqdn, targets, ttl, providerSpecific, setIdentifier)...)
		}
	}

	// Skip endpoints if we do not want entries from annotations
	if !sc.ignoreHostnameAnnotation {
		hostnameList := getHostnamesFromAnnotations(HTTPProxy.Annotations)
		for _, hostname := range hostnameList {
			endpoints = append(endpoints, endpointsForHostname(hostname, targets, ttl, providerSpecific, setIdentifier)...)
		}
	}

	return endpoints, nil
}

func parseContourLoadBalancerService(service string) (namespace, name string, err error) {
	parts := strings.Split(service, "/")
	if len(parts) != 2 {
		err = fmt.Errorf("invalid contour load balancer service (namespace/name) found '%v'", service)
	} else {
		namespace, name = parts[0], parts[1]
	}

	return
}
