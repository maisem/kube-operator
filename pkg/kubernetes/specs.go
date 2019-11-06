package kubernetes

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	corev1 "k8s.io/api/core/v1"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api/v1"
)

var (
	baseContainer = corev1.Container{
		ImagePullPolicy: corev1.PullIfNotPresent,
		LivenessProbe: &corev1.Probe{
			FailureThreshold:    0,
			InitialDelaySeconds: 15,
			TimeoutSeconds:      15,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				//corev1.ResourceCPU: resource.MustParse("250m"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				MountPath: "/etc/ssl/certs",
				Name:      "ca-certs",
				ReadOnly:  true,
			},
			{
				MountPath: "/etc/ca-certificates",
				Name:      "etc-ca-certificates",
				ReadOnly:  true,
			},
			{
				MountPath: "/usr/share/ca-certificates",
				Name:      "usr-share-ca-certificates",
				ReadOnly:  true,
			},
		},
	}

	notTrue     = false
	dirOrCreate = corev1.HostPathDirectoryOrCreate

	basePodSpec = &corev1.PodSpec{
		AutomountServiceAccountToken: &notTrue,
		HostNetwork:                  false,
		Volumes: []corev1.Volume{
			{
				Name: "ca-certs",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: "/etc/ssl/certs",
						Type: &dirOrCreate,
					},
				},
			},
			{
				Name: "etc-ca-certificates",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: "/etc/ca-certificates",
						Type: &dirOrCreate,
					},
				},
			},
			{
				Name: "usr-share-ca-certificates",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: "/usr/share/ca-certificates",
						Type: &dirOrCreate,
					},
				},
			},
		},
	}
)

type KubeScheduler struct {
	Image            string
	KubeConfigSecret string
}

func (ks *KubeScheduler) Spec() *corev1.PodSpec {
	ps := basePodSpec.DeepCopy()
	c := baseContainer.DeepCopy()

	c.Image = ks.Image
	c.Name = "kube-scheduler"
	c.LivenessProbe.HTTPGet = &corev1.HTTPGetAction{
		Path:   "/healthz",
		Port:   intstr.FromInt(10251),
		Scheme: corev1.URISchemeHTTP,
	}

	c.Command = []string{"kube-scheduler"}
	c.Args = []string{
		"--authentication-kubeconfig=/etc/kubernetes/pki/kubeconfig/kubeconfig",
		"--authorization-kubeconfig=/etc/kubernetes/pki/kubeconfig/kubeconfig",
		"--kubeconfig=/etc/kubernetes/pki/kubeconfig/kubeconfig",

		"--bind-address=127.0.0.1",
		"--leader-elect=true",
	}

	addSecretVolume(ps, c, ks.KubeConfigSecret, "/etc/kubernetes/pki/kubeconfig")

	ps.Containers = append(ps.Containers, *c)
	return ps
}

type KubeControllerManager struct {
	Image                 string
	CASecret              string
	FrontProxySecret      string
	ServiceAccountSecret  string
	KubeConfigSecret      string
	EnableBootstrapTokens bool
}

func (kcm *KubeControllerManager) Spec() *corev1.PodSpec {
	ps := basePodSpec.DeepCopy()
	c := baseContainer.DeepCopy()

	c.Image = kcm.Image
	c.Name = "kube-controller-manager"
	c.LivenessProbe.HTTPGet = &corev1.HTTPGetAction{
		Path:   "/healthz",
		Port:   intstr.FromInt(10252),
		Scheme: corev1.URISchemeHTTP,
	}

	c.Command = []string{"kube-controller-manager"}
	c.Args = []string{
		"--bind-address=127.0.0.1",
		"--leader-elect=true",
		"--use-service-account-credentials=true",

		"--authentication-kubeconfig=/etc/kubernetes/pki/kubeconfig/kubeconfig",
		"--authorization-kubeconfig=/etc/kubernetes/pki/kubeconfig/kubeconfig",
		"--kubeconfig=/etc/kubernetes/pki/kubeconfig/kubeconfig",

		"--root-ca-file=/etc/kubernetes/pki/ca/ca.crt",
		"--client-ca-file=/etc/kubernetes/pki/ca/ca.crt",
		"--cluster-signing-cert-file=/etc/kubernetes/pki/ca/tls.crt",
		"--cluster-signing-key-file=/etc/kubernetes/pki/ca/tls.key",

		"--requestheader-client-ca-file=/etc/kubernetes/pki/front-proxy/ca.crt",

		"--service-account-private-key-file=/etc/kubernetes/pki/service-account/tls.key",
	}

	if kcm.EnableBootstrapTokens {
		c.Args = append(c.Args, "--controllers=*,bootstrapsigner,tokencleaner")
	}

	addSecretVolume(ps, c, kcm.CASecret, "/etc/kubernetes/pki/ca")
	addSecretVolume(ps, c, kcm.FrontProxySecret, "/etc/kubernetes/pki/front-proxy")
	addSecretVolume(ps, c, kcm.ServiceAccountSecret, "/etc/kubernetes/pki/service-account")
	addSecretVolume(ps, c, kcm.KubeConfigSecret, "/etc/kubernetes/pki/kubeconfig")

	ps.Containers = append(ps.Containers, *c)
	return ps
}

func addSecretVolume(ps *corev1.PodSpec, c *corev1.Container, secret, path string) {
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
		Name:      secret,
		MountPath: path,
	})
	ps.Volumes = append(ps.Volumes, corev1.Volume{
		Name: secret,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: secret,
			},
		},
	})
}

type KubeConfig struct {
	Address    string
	CA         []byte
	ClientCert []byte
	ClientKey  []byte
}

func (kc *KubeConfig) AsYaml() ([]byte, error) {
	return yaml.Marshal(kc.Build())
}

func (kc *KubeConfig) Build() *clientcmdapi.Config {
	return &clientcmdapi.Config{
		CurrentContext: "context",
		Clusters: []clientcmdapi.NamedCluster{
			{

				Name: "cluster",
				Cluster: clientcmdapi.Cluster{
					Server:                   kc.Address,
					CertificateAuthorityData: kc.CA,
				},
			},
		},
		AuthInfos: []clientcmdapi.NamedAuthInfo{
			{
				Name: "user",
				AuthInfo: clientcmdapi.AuthInfo{
					ClientCertificateData: kc.ClientCert,
					ClientKeyData:         kc.ClientKey,
				},
			},
		},
		Contexts: []clientcmdapi.NamedContext{
			{
				Name: "context",
				Context: clientcmdapi.Context{
					Cluster:  "cluster",
					AuthInfo: "user",
				},
			},
		},
	}
}

type KubeAPIServer struct {
	Image                 string
	AdvertiseAddress      string
	ServiceClusterIPRange string
	AllowPrivileged       bool

	EtcdAddress          string
	EtcdClientSecret     string
	TLSSecret            string
	KubeletClientSecret  string
	FrontProxySecret     string
	ServiceAccountSecret string
}

func (k *KubeAPIServer) Spec() *corev1.PodSpec {
	ps := basePodSpec.DeepCopy()
	c := baseContainer.DeepCopy()
	c.Image = k.Image
	c.Name = "kube-apiserver"
	c.LivenessProbe.HTTPGet = &corev1.HTTPGetAction{
		Path:   "/healthz",
		Port:   intstr.FromInt(6443),
		Scheme: corev1.URISchemeHTTPS,
	}
	c.Command = []string{"kube-apiserver"}
	c.Args = []string{
		fmt.Sprintf("--advertise-address=%v", k.AdvertiseAddress),
		fmt.Sprintf("--allow-privileged=%v", k.AllowPrivileged),
		fmt.Sprintf("--service-cluster-ip-range=%v", k.ServiceClusterIPRange),
		"--authorization-mode=Node,RBAC",
		"--enable-admission-plugins=NodeRestriction",
		"--enable-bootstrap-token-auth=true",
		"--insecure-port=0",
		"--secure-port=6443",

		"--kubelet-client-certificate=/etc/kubernetes/pki/kubelet-client/tls.crt",
		"--kubelet-client-key=/etc/kubernetes/pki/kubelet-client/tls.key",
		"--kubelet-preferred-address-types=InternalIP,ExternalIP,Hostname",

		"--requestheader-client-ca-file=/etc/kubernetes/pki/front-proxy/ca.crt",
		"--proxy-client-cert-file=/etc/kubernetes/pki/front-proxy/tls.crt",
		"--proxy-client-key-file=/etc/kubernetes/pki/front-proxy/tls.key",
		"--requestheader-allowed-names=front-proxy-client",
		"--requestheader-extra-headers-prefix=X-Remote-Extra-",
		"--requestheader-group-headers=X-Remote-Group",
		"--requestheader-username-headers=X-Remote-User",

		"--service-account-key-file=/etc/kubernetes/pki/service-account/tls.crt",

		"--client-ca-file=/etc/kubernetes/pki/tls/ca.crt",
		"--tls-cert-file=/etc/kubernetes/pki/tls/tls.crt",
		"--tls-private-key-file=/etc/kubernetes/pki/tls/tls.key",
	}

	addSecretVolume(ps, c, k.TLSSecret, "/etc/kubernetes/pki/tls")
	addSecretVolume(ps, c, k.FrontProxySecret, "/etc/kubernetes/pki/front-proxy")
	addSecretVolume(ps, c, k.ServiceAccountSecret, "/etc/kubernetes/pki/service-account")
	addSecretVolume(ps, c, k.KubeletClientSecret, "/etc/kubernetes/pki/kubelet-client")

	if k.EtcdClientSecret != "" {
		c.Args = append(c.Args,
			fmt.Sprintf("--etcd-servers=https://%v:2379", k.EtcdAddress),
			"--etcd-cafile=/etc/kubernetes/pki/etcd/ca.crt",
			"--etcd-certfile=/etc/kubernetes/pki/etcd/tls.crt",
			"--etcd-keyfile=/etc/kubernetes/pki/etcd/tls.key",
		)
		addSecretVolume(ps, c, k.EtcdClientSecret, "/etc/kubernetes/pki/etcd")
	} else {
		c.Args = append(c.Args,
			fmt.Sprintf("--etcd-servers=http://%v:2379", k.EtcdAddress),
		)
	}

	ps.Containers = append(ps.Containers, *c)
	return ps
}
