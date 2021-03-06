package route

import (
	"context"

	// kube
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog"

	operatorv1 "github.com/openshift/api/operator/v1"
	routev1 "github.com/openshift/api/route/v1"
	routeclient "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"

	"github.com/openshift/console-operator/pkg/api"
	"github.com/openshift/console-operator/pkg/console/subresource/util"
)

const (
	// ingress instance named "default" is the OOTB ingresscontroller
	// this is an implicit stable API
	defaultIngressController = "default"
)

// holds information about custom TLS certificate and its key
type CustomTLSCert struct {
	Certificate string
	Key         string
}

func ApplyRoute(client routeclient.RoutesGetter, recorder events.Recorder, required *routev1.Route) (*routev1.Route, bool, error) {
	existing, err := client.Routes(required.Namespace).Get(context.TODO(), required.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		requiredCopy := required.DeepCopy()
		actual, err := client.Routes(requiredCopy.Namespace).Create(context.TODO(), resourcemerge.WithCleanLabelsAndAnnotations(requiredCopy).(*routev1.Route), metav1.CreateOptions{})
		return actual, true, err
	}
	if err != nil {
		return nil, false, err
	}

	existingCopy := existing.DeepCopy()
	modified := resourcemerge.BoolPtr(false)
	resourcemerge.EnsureObjectMeta(modified, &existingCopy.ObjectMeta, required.ObjectMeta)
	specSame := equality.Semantic.DeepEqual(existingCopy.Spec, required.Spec)

	if specSame && !*modified {
		klog.V(4).Infof("%s route exists and is in the correct state", existingCopy.ObjectMeta.Name)
		return existingCopy, false, nil
	}

	existingCopy.Spec = required.Spec
	actual, err := client.Routes(required.Namespace).Update(context.TODO(), existingCopy, metav1.UpdateOptions{})
	return actual, true, err
}

// ensures route exists.
// handles 404 with a create
// returns any other error
func GetOrCreate(ctx context.Context, client routeclient.RoutesGetter, required *routev1.Route) (*routev1.Route, bool, error) {
	isNew := false
	route, err := client.Routes(required.Namespace).Get(ctx, required.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		isNew = true
		route, err = client.Routes(required.Namespace).Create(ctx, required, metav1.CreateOptions{})
	}

	if err != nil {
		return nil, isNew, err
	}
	return route, isNew, nil
}

// Default `console` route points by default to the `console` service.
// If custom hostname for the console is set, then the default route
// should point to the redirect `console-redirect` service and the
// created custom route should be pointing to the `console` service.
func DefaultRoute(cr *operatorv1.Console) *routev1.Route {
	route := DefaultStub()
	usePort := api.ConsoleContainerPortName
	tlsTermination := routev1.TLSTerminationReencrypt
	serviceName := api.OpenShiftConsoleServiceName
	if IsCustomRouteSet(cr) {
		usePort = api.RedirectContainerPortName
		tlsTermination = routev1.TLSTerminationEdge
		serviceName = api.OpenshiftConsoleRedirectServiceName
	}
	route.Spec = routev1.RouteSpec{
		To:             toService(serviceName),
		Port:           port(usePort),
		TLS:            tls(nil, tlsTermination),
		WildcardPolicy: wildcard(),
	}
	util.AddOwnerRef(route, util.OwnerRefFrom(cr))
	return route
}

func DefaultStub() *routev1.Route {
	meta := util.SharedMeta()
	return &routev1.Route{
		ObjectMeta: meta,
	}
}

func CustomRoute(cr *operatorv1.Console, tlsConfig *CustomTLSCert) *routev1.Route {
	route := DefaultStub()
	route.ObjectMeta.Name = api.OpenshiftConsoleCustomRouteName
	route.Spec = routev1.RouteSpec{
		Host:           cr.Spec.Route.Hostname,
		To:             toService(api.OpenShiftConsoleServiceName),
		Port:           port(api.ConsoleContainerPortName),
		TLS:            tls(tlsConfig, routev1.TLSTerminationReencrypt),
		WildcardPolicy: wildcard(),
	}
	util.AddOwnerRef(route, util.OwnerRefFrom(cr))
	return route
}

func toService(serviceName string) routev1.RouteTargetReference {
	weight := int32(100)
	return routev1.RouteTargetReference{
		Kind:   "Service",
		Name:   serviceName,
		Weight: &weight,
	}
}

func port(port string) *routev1.RoutePort {
	return &routev1.RoutePort{
		TargetPort: intstr.FromString(port),
	}
}

func tls(tlsConfig *CustomTLSCert, terminationType routev1.TLSTerminationType) *routev1.TLSConfig {
	tls := &routev1.TLSConfig{
		Termination:                   terminationType,
		InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
	}
	if tlsConfig != nil {
		tls.Certificate = tlsConfig.Certificate
		tls.Key = tlsConfig.Key
	}
	return tls
}

func wildcard() routev1.WildcardPolicyType {
	return routev1.WildcardPolicyNone
}

func GetCanonicalHost(route *routev1.Route) string {
	for _, ingress := range route.Status.Ingress {
		if ingress.RouterName != defaultIngressController {
			klog.V(4).Infof("ignoring route ingress '%v'", ingress.RouterName)
			continue
		}
		// ingress must be admitted before it is useful to us
		if !isIngressAdmitted(ingress) {
			klog.V(4).Infof("route ingress '%v' not admitted", ingress.RouterName)
			continue
		}
		klog.V(4).Infof("route ingress '%v' found and admitted, host: %v", defaultIngressController, ingress.Host)
		return ingress.Host
	}
	klog.V(4).Infoln("route ingress not yet ready for console")
	return ""
}

// for the purpose of availability, we simply need to know when the
// route has been admitted.  we may have multiple ingress on the route, each
// with an admitted attribute.
func IsAdmitted(route *routev1.Route) bool {
	for _, ingress := range route.Status.Ingress {
		if isIngressAdmitted(ingress) {
			return true
		}
	}
	return false
}

func isIngressAdmitted(ingress routev1.RouteIngress) bool {
	admitted := false
	for _, condition := range ingress.Conditions {
		if condition.Type == routev1.RouteAdmitted && condition.Status == corev1.ConditionTrue {
			admitted = true
		}
	}
	return admitted
}

func IsCustomRouteSet(operatorConfig *operatorv1.Console) bool {
	if operatorConfig == nil {
		return false
	}
	return len(operatorConfig.Spec.Route.Hostname) != 0
}

// Check if reference for secret holding custom TLS certificate and key is set
func IsCustomRouteSecretSet(operatorConfig *operatorv1.Console) bool {
	if operatorConfig == nil {
		return false
	}
	return len(operatorConfig.Spec.Route.Secret.Name) != 0
}
