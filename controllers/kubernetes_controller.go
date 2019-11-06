/*

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

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"operators.maisem.dev/kube/pkg/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	etcdapi "github.com/coreos/etcd-operator/pkg/apis/etcd/v1beta2"
	cmapi "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha2"
	cmmeta "github.com/jetstack/cert-manager/pkg/apis/meta/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	opapi "operators.maisem.dev/kube/api/v1alpha1"
)

// KubernetesReconciler reconciles a Kubernetes object
type KubernetesReconciler struct {
	client.Client
	Log logr.Logger
}

// +kubebuilder:rbac:groups=operators.maisem.dev,resources=kubernetes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=operators.maisem.dev,resources=kubernetes/status,verbs=get;update;patch

func (r *KubernetesReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	_ = r.Log.WithValues("kubernetes", req.NamespacedName)

	// your logic here
	k := &opapi.Kubernetes{}
	if err := r.Get(ctx, req.NamespacedName, k); err != nil {
		if kubeerrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !k.GetDeletionTimestamp().IsZero() {
		return r.handleDelete(ctx, k)
	}
	kr := &kubeReconciliation{
		kube:      k,
		kubeCerts: newKubeCerts(k),
	}

	p := newPipeline(
		r.handleServices,
		r.handleCerts,
		inParallel(
			r.createAdminKubeConfig,
			r.handleEtcd,
			r.handleKubeAPIServer,
			r.handleKubeControllerManager,
			r.handleKubeScheduler,
		),
	)
	if err := p(ctx, kr); err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: time.Second}, err
	}

	return ctrl.Result{}, nil
}

func (r *KubernetesReconciler) kubeconfigFromSecret(ctx context.Context, kr *kubeReconciliation, secretName, kubeconfigSecret string, endpoint *opapi.Endpoint) error {
	nn := types.NamespacedName{
		Name:      secretName,
		Namespace: kr.kube.Namespace,
	}
	s := &corev1.Secret{}
	if err := r.Get(ctx, nn, s); err != nil {
		return err
	}

	kc := kubernetes.KubeConfig{
		Address:    fmt.Sprintf("https://%v:%d", endpoint.Address, endpoint.Port),
		CA:         s.Data["ca.crt"],
		ClientKey:  s.Data["tls.key"],
		ClientCert: s.Data["tls.crt"],
	}

	cfgData, err := kc.AsYaml()
	if err != nil {
		return err
	}

	cmKubecfgSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: kr.kube.Namespace,
			Name:      kubeconfigSecret,
		},
	}
	_, err = r.CreateOrUpdate(ctx, cmKubecfgSecret, func() error {
		addOwnerReferenceIfRequired(kr.kube, cmKubecfgSecret)
		copyLabels(kr.kube, &cmKubecfgSecret.ObjectMeta)
		if cmKubecfgSecret.Data == nil {
			cmKubecfgSecret.Data = make(map[string][]byte)
		}
		cmKubecfgSecret.Data["kubeconfig"] = cfgData
		return nil
	})
	return err
}

func (r *KubernetesReconciler) handleKubeScheduler(ctx context.Context, kr *kubeReconciliation) error {
	ksKubeconfigSecret := kr.kube.Name + "-ks-kubeconfig"
	if err := r.kubeconfigFromSecret(ctx, kr, kr.kubeCerts.schedulerCert.name, ksKubeconfigSecret, &kr.kube.Status.InternalEndpoint); err != nil {
		return err
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kr.kube.Name + "-kube-scheduler",
			Namespace: kr.kube.Namespace,
		},
	}
	ks := (&kubernetes.KubeScheduler{
		Image:            "k8s.gcr.io/kube-scheduler:" + kr.kube.Spec.Version,
		KubeConfigSecret: ksKubeconfigSecret,
	}).Spec()
	if _, err := r.CreateOrUpdate(ctx, dep, func() error {
		addOwnerReferenceIfRequired(kr.kube, dep)
		copyLabels(kr.kube, &dep.ObjectMeta)
		dep.Labels["app"] = "kube-scheduler"
		dep.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: dep.Labels,
		}
		dep.Spec.Template.Labels = dep.Labels
		dep.Spec.Template.Spec = *ks
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (r *KubernetesReconciler) handleKubeControllerManager(ctx context.Context, kr *kubeReconciliation) error {
	kcmKubeconfigSecret := kr.kube.Name + "-kcm-kubeconfig"
	if err := r.kubeconfigFromSecret(ctx, kr, kr.kubeCerts.controllerManagerCert.name, kcmKubeconfigSecret, &kr.kube.Status.InternalEndpoint); err != nil {
		return err
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kr.kube.Name + "-kube-controller-manager",
			Namespace: kr.kube.Namespace,
		},
	}
	kcm := (&kubernetes.KubeControllerManager{
		Image:                 "k8s.gcr.io/kube-controller-manager:" + kr.kube.Spec.Version,
		KubeConfigSecret:      kcmKubeconfigSecret,
		CASecret:              kr.kubeCerts.kubeCACert.name,
		FrontProxySecret:      kr.kubeCerts.frontProxyClient.name,
		ServiceAccountSecret:  kr.kubeCerts.saCert.name,
		EnableBootstrapTokens: true,
	}).Spec()
	if _, err := r.CreateOrUpdate(ctx, dep, func() error {
		addOwnerReferenceIfRequired(kr.kube, dep)
		copyLabels(kr.kube, &dep.ObjectMeta)
		dep.Labels["app"] = "kube-controller-manager"
		dep.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: dep.Labels,
		}
		dep.Spec.Template.Labels = dep.Labels
		dep.Spec.Template.Spec = *kcm
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (r *KubernetesReconciler) createAdminKubeConfig(ctx context.Context, kr *kubeReconciliation) error {
	if kr.kube.Status.ExternalEndpoint != nil {
		return r.kubeconfigFromSecret(ctx, kr, kr.kubeCerts.adminCert.name, kr.kube.Name+"-admin-kubeconfig", kr.kube.Status.ExternalEndpoint)
	}
	return r.kubeconfigFromSecret(ctx, kr, kr.kubeCerts.adminCert.name, kr.kube.Name+"-admin-kubeconfig", &kr.kube.Status.InternalEndpoint)
}

func newPipeline(steps ...reconciliationStep) reconciliationStep {
	rs := pipeline(steps)
	return rs.run
}

type pipeline []reconciliationStep

func (p *pipeline) run(ctx context.Context, kr *kubeReconciliation) error {
	kr = kr.Copy()
	for _, fn := range *p {
		if err := fn(ctx, kr); err != nil {
			return err
		}
	}
	return nil
}

type reconciliationStep func(ctx context.Context, kr *kubeReconciliation) error

type kubeReconciliation struct {
	kube      *opapi.Kubernetes
	kubeCerts *kubeCerts
}

func (kr *kubeReconciliation) Copy() *kubeReconciliation {
	x := *kr
	x.kube = x.kube.DeepCopy()
	return &x
}

func inParallel(fns ...reconciliationStep) reconciliationStep {
	return func(ctx context.Context, kr *kubeReconciliation) error {
		var g errgroup.Group
		for _, fn := range fns {
			fn := fn
			g.Go(func() error {
				return fn(ctx, kr.Copy())
			})
		}
		return g.Wait()
	}
}

func (r *KubernetesReconciler) handleKubeAPIServer(ctx context.Context, kr *kubeReconciliation) error {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kr.kube.Name + "-kube-apiserver",
			Namespace: kr.kube.Namespace,
		},
	}
	kapi := &kubernetes.KubeAPIServer{
		Image: "k8s.gcr.io/kube-apiserver:" + kr.kube.Spec.Version,
		// TODO: support splitting etcd.
		EtcdAddress:           kr.kube.Name + "-etcd-0-client",
		AdvertiseAddress:      kr.kube.Status.InternalEndpoint.Address,
		ServiceClusterIPRange: kr.kube.Spec.ServiceClusterIPRange,
		AllowPrivileged:       true,
		TLSSecret:             kr.kubeCerts.apiserverCert.name,
		KubeletClientSecret:   kr.kubeCerts.apiserverKubeletClient.name,
		FrontProxySecret:      kr.kubeCerts.frontProxyClient.name,
		ServiceAccountSecret:  kr.kubeCerts.saCert.name,
	}
	if kr.kube.Status.ExternalEndpoint != nil {
		kapi.AdvertiseAddress = kr.kube.Status.ExternalEndpoint.Address
	}
	kSpec := kapi.Spec()
	if _, err := r.CreateOrUpdate(ctx, dep, func() error {
		addOwnerReferenceIfRequired(kr.kube, dep)
		copyLabels(kr.kube, &dep.ObjectMeta)
		dep.Labels["app"] = "kube-apiserver"
		dep.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: dep.Labels,
		}
		dep.Spec.Template.Labels = dep.Labels
		dep.Spec.Template.Spec = *kSpec
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (r *KubernetesReconciler) handleEtcd(ctx context.Context, kr *kubeReconciliation) error {
	etcd := &etcdapi.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kr.kube.Name + "-etcd-0",
			Namespace: kr.kube.Namespace,
		},
	}
	if _, err := r.CreateOrUpdate(ctx, etcd, func() error {
		addOwnerReferenceIfRequired(kr.kube, etcd)
		copyLabels(kr.kube, &etcd.ObjectMeta)
		if etcd.CreationTimestamp.IsZero() {
			kr.kube.Spec.Etcd.DeepCopyInto(&etcd.Spec)
		}

		etcd.Spec.TLS = nil
		/*etcd.Spec.TLS = &etcdapi.TLSPolicy{
			Static: &etcdapi.StaticTLS{
				Member: &etcdapi.MemberSecret{
					PeerSecret:   kr.kubeCerts.etcdPeer.name,
					ServerSecret: kr.kubeCerts.etcdServer.name,
				},
				OperatorSecret: kr.kubeCerts.etcdOperatorClient.name,
			},
		}*/
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (r *KubernetesReconciler) handleServices(ctx context.Context, kr *kubeReconciliation) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kr.kube.Name + "-kubernetes",
			Namespace: kr.kube.Namespace,
		},
	}
	if _, err := r.CreateOrUpdate(ctx, svc, func() error {
		addOwnerReferenceIfRequired(kr.kube, svc)
		copyLabels(kr.kube, &svc.ObjectMeta)
		if svc.CreationTimestamp.IsZero() {
			kr.kube.Spec.APIServiceSpec.DeepCopyInto(&svc.Spec)
		}
		svc.Spec.Ports = []corev1.ServicePort{
			{
				Name:       "https",
				Protocol:   corev1.ProtocolTCP,
				Port:       6443,
				TargetPort: intstr.FromInt(6443),
			},
		}
		svc.Spec.Selector = cloneLabels(kr.kube.Labels)
		svc.Spec.Selector["app"] = "kube-apiserver"
		return nil
	}); err != nil {
		return errors.Wrap(err, "failed to create/update kubernetes svc")
	}

	origStatus := kr.kube.Status.DeepCopy()
	if ep := svc.Status.LoadBalancer.Ingress; len(ep) != 0 {
		kr.kube.Status.ExternalEndpoint = &opapi.Endpoint{
			Address: ep[0].IP,
			Port:    6443,
		}
	}
	kr.kube.Status.InternalEndpoint.Address = svc.Spec.ClusterIP
	kr.kube.Status.InternalEndpoint.Port = 6443

	if reflect.DeepEqual(kr.kube.Status, origStatus) {
		return nil
	}
	return errors.Wrap(r.Status().Update(ctx, kr.kube), "failed to assign endpoints to kubernetes.status")
}

func (r *KubernetesReconciler) CreateOrUpdate(ctx context.Context, obj runtime.Object, f controllerutil.MutateFn) (controllerutil.OperationResult, error) {
	return controllerutil.CreateOrUpdate(ctx, r, obj, f)
}

type metaAccessor interface {
	metav1.Object
	runtime.Object
}

func addOwnerReferenceIfRequired(owner *opapi.Kubernetes, target metaAccessor) {
	for _, or := range target.GetOwnerReferences() {
		if or.UID == owner.GetUID() {
			return
		}
	}
	gvk := owner.GetObjectKind().GroupVersionKind()
	api, kind := gvk.ToAPIVersionAndKind()
	t := true
	or := metav1.OwnerReference{
		APIVersion: api,
		Kind:       kind,
		Name:       owner.GetName(),
		UID:        owner.GetUID(),
		Controller: &t,
	}
	target.SetOwnerReferences(append(target.GetOwnerReferences(), or))
}

func (r *KubernetesReconciler) createOrUpdateCert(ctx context.Context, kr *kubeReconciliation, c *cert) error {
	cmCert := &cmapi.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: kr.kube.Namespace,
			Name:      c.name,
		},
	}
	_, err := r.CreateOrUpdate(ctx, cmCert, func() error {
		addOwnerReferenceIfRequired(kr.kube, cmCert)
		copyLabels(kr.kube, &cmCert.ObjectMeta)
		c.spec().DeepCopyInto(&cmCert.Spec)
		return nil
	})
	return err
}

type cert struct {
	issuer string
	name   string
	cn     string
	isCA   bool
	usages []cmapi.KeyUsage
	sans   []string
	ips    []string
	org    []string
}

func (c *cert) spec() *cmapi.CertificateSpec {
	return &cmapi.CertificateSpec{
		SecretName: c.name,
		IssuerRef: cmmeta.ObjectReference{
			Kind: "Issuer",
			Name: c.issuer,
		},
		CommonName: c.cn,
		Duration: &metav1.Duration{
			Duration: 30 * 24 * time.Hour,
		},
		RenewBefore: &metav1.Duration{
			Duration: 20 * 24 * time.Hour,
		},
		KeySize:      4096,
		KeyAlgorithm: cmapi.RSAKeyAlgorithm,
		Usages:       append(c.usages, cmapi.DefaultKeyUsages()...),
		IsCA:         c.isCA,
		DNSNames:     c.sans,
		IPAddresses:  c.ips,
		Organization: c.org,
	}
}

func cloneLabels(l map[string]string) map[string]string {
	m := make(map[string]string)
	for k, v := range l {
		m[k] = v
	}
	return m
}

func copyLabels(k *opapi.Kubernetes, om *metav1.ObjectMeta) {
	if om.Labels == nil {
		om.Labels = make(map[string]string)
	}
	for key, v := range k.Labels {
		om.Labels[key] = v
	}
}

func (r *KubernetesReconciler) createOrUpdateIssuer(ctx context.Context, kr *kubeReconciliation, name string, is *cmapi.IssuerSpec) error {
	issuer := &cmapi.Issuer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: kr.kube.Namespace,
			Name:      name,
		},
	}
	_, err := r.CreateOrUpdate(ctx, issuer, func() error {
		addOwnerReferenceIfRequired(kr.kube, issuer)
		copyLabels(kr.kube, &issuer.ObjectMeta)
		if issuer.CreationTimestamp.IsZero() {
			is.DeepCopyInto(&issuer.Spec)
		}
		return nil
	})
	return err
}

type kubeCerts struct {
	rootIssuer string

	adminCert              *cert
	kubeCACert             *cert
	saCert                 *cert
	apiserverCert          *cert
	apiserverKubeletClient *cert
	schedulerCert          *cert
	controllerManagerCert  *cert

	frontProxyIssuer *cert
	frontProxyClient *cert

	etcdIssuer              *cert
	etcdPeer                *cert
	etcdOperatorClient      *cert
	etcdKubeAPIServerClient *cert
	etcdServer              *cert
}

func (kc *kubeCerts) allCerts() []*cert {
	return []*cert{
		kc.kubeCACert,
		kc.adminCert,
		kc.saCert,
		kc.apiserverCert,
		kc.apiserverKubeletClient,
		kc.schedulerCert,
		kc.controllerManagerCert,
		kc.frontProxyIssuer,
		kc.frontProxyClient,
		kc.etcdIssuer,
		kc.etcdPeer,
		kc.etcdOperatorClient,
		kc.etcdKubeAPIServerClient,
		kc.etcdServer,
	}
}

func newKubeCerts(k *opapi.Kubernetes) *kubeCerts {
	var (
		apiIPs           []string
		rootIssuer       = fmt.Sprintf("%v-ca", k.Name)
		kubernetesIssuer = fmt.Sprintf("%v-kube-ca", k.Name)
		frontProxyIssuer = fmt.Sprintf("%v-front-proxy", k.Name)
		etcdIssuer       = fmt.Sprintf("%v-etcd-ca", k.Name)
		apiDNSs          = []string{
			"kubernetes",
			"kubernetes.default",
			"kubernetes.default.svc",
			"kubernetes.default.svc." + k.Spec.ClusterDomain,
		}
	)
	apiIPs = append(apiIPs, k.Status.InternalEndpoint.Address)
	if k.Status.ExternalEndpoint != nil {
		apiIPs = append(apiIPs, k.Status.ExternalEndpoint.Address)
	}
	return &kubeCerts{
		rootIssuer: rootIssuer,
		kubeCACert: &cert{
			issuer: rootIssuer,
			name:   kubernetesIssuer,
			cn:     k.Name,
			isCA:   true,
		},
		saCert: &cert{
			issuer: kubernetesIssuer,
			name:   fmt.Sprintf("%v-sa-cert", k.Name),
			cn:     "service-account-signer",
		},
		adminCert: &cert{
			issuer: kubernetesIssuer,
			name:   fmt.Sprintf("%v-admin-cert", k.Name),
			cn:     "kubernetes-admin",
			org: []string{
				"system:masters",
			},
		},
		apiserverCert: &cert{
			issuer: kubernetesIssuer,
			name:   fmt.Sprintf("%v-apiserver", k.Name),
			cn:     k.Name,
			usages: []cmapi.KeyUsage{
				cmapi.UsageServerAuth,
			},
			sans: apiDNSs,
			ips:  apiIPs,
		},
		apiserverKubeletClient: &cert{
			issuer: kubernetesIssuer,
			name:   fmt.Sprintf("%v-apiserver-kubelet-client", k.Name),
			cn:     "kube-apiserver-kubelet-client",
			usages: []cmapi.KeyUsage{
				cmapi.UsageServerAuth,
			},
			org: []string{
				"system:masters",
			},
		},
		schedulerCert: &cert{
			issuer: kubernetesIssuer,
			cn:     "system:kube-scheduler",
			name:   fmt.Sprintf("%v-kube-scheduler", k.Name),
			usages: []cmapi.KeyUsage{
				cmapi.UsageServerAuth,
			},
		},
		controllerManagerCert: &cert{
			issuer: kubernetesIssuer,
			cn:     "system:kube-controller-manager",
			name:   fmt.Sprintf("%v-kube-controller-manager", k.Name),
			usages: []cmapi.KeyUsage{
				cmapi.UsageServerAuth,
			},
		},
		frontProxyIssuer: &cert{
			issuer: kubernetesIssuer,
			name:   frontProxyIssuer,
			cn:     "front-proxy-ca",
			isCA:   true,
		},
		frontProxyClient: &cert{
			issuer: frontProxyIssuer,
			name:   fmt.Sprintf("%v-front-proxy-client", k.Name),
			cn:     "front-proxy-client",
			usages: []cmapi.KeyUsage{
				cmapi.UsageClientAuth,
			},
		},
		etcdIssuer: &cert{
			issuer: kubernetesIssuer,
			name:   etcdIssuer,
			cn:     "etcd-ca",
			isCA:   true,
		},
		etcdOperatorClient: &cert{
			issuer: etcdIssuer,
			name:   fmt.Sprintf("%v-etcd-operator-client", k.Name),
			cn:     "etcd-operator",
			usages: []cmapi.KeyUsage{
				cmapi.UsageClientAuth,
			},
		},
		etcdPeer: &cert{
			issuer: etcdIssuer,
			name:   fmt.Sprintf("%v-etcd-peer", k.Name),
			cn:     "etcd-peer",
			usages: []cmapi.KeyUsage{
				cmapi.UsageClientAuth,
				cmapi.UsageServerAuth,
			},
		},
		etcdKubeAPIServerClient: &cert{
			issuer: etcdIssuer,
			name:   fmt.Sprintf("%v-etcd-apiserver-client", k.Name),
			cn:     "kube-apiserver",
			usages: []cmapi.KeyUsage{
				cmapi.UsageClientAuth,
			},
		},
		etcdServer: &cert{
			issuer: etcdIssuer,
			name:   fmt.Sprintf("%v-etcd-server", k.Name),
			cn:     "etcd-server",
			usages: []cmapi.KeyUsage{
				cmapi.UsageClientAuth,
				cmapi.UsageServerAuth,
			},
		},
	}
}

func (r *KubernetesReconciler) handleCerts(ctx context.Context, kr *kubeReconciliation) error {
	kc := newKubeCerts(kr.kube)
	if err := r.createOrUpdateIssuer(ctx, kr, kc.rootIssuer, &cmapi.IssuerSpec{
		IssuerConfig: cmapi.IssuerConfig{
			CA:         kr.kube.Spec.RootIssuer.CA,
			Vault:      kr.kube.Spec.RootIssuer.Vault,
			SelfSigned: kr.kube.Spec.RootIssuer.SelfSigned,
			Venafi:     kr.kube.Spec.RootIssuer.Venafi,
		},
	}); err != nil {
		return errors.Wrap(err, "failed to create/update rootIssuer")
	}

	var eg errgroup.Group
	for _, cert := range kc.allCerts() {
		cert := cert
		eg.Go(func() error {
			if err := r.createOrUpdateCert(ctx, kr, cert); err != nil {
				return errors.Wrapf(err, "failed to create/update %v", cert.name)
			}
			if !cert.isCA {
				return nil
			}
			is := &cmapi.IssuerSpec{
				IssuerConfig: cmapi.IssuerConfig{
					CA: &cmapi.CAIssuer{
						SecretName: cert.name,
					},
				},
			}
			return errors.Wrapf(r.createOrUpdateIssuer(ctx, kr, cert.name, is), "failed to create/update %v issuer", cert.name)
		})
	}
	return eg.Wait()
}

func (r *KubernetesReconciler) handleDelete(ctx context.Context, k *opapi.Kubernetes) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

func (r *KubernetesReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&opapi.Kubernetes{}).
		Owns(&cmapi.Certificate{}).
		Owns(&cmapi.Issuer{}).
		Owns(&etcdapi.EtcdCluster{}).
		Complete(r)
}
