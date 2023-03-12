/*
Copyright 2023.

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

package etcd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	// "github.com/cert-manager/cert-manager/pkg/apis/certmanager"
	// cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	// cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	etcdv1beta2 "github.com/weiyuanke/etcd-operator/apis/etcd/v1beta2"
)

var (
	llog = ctrl.Log.WithName("EtcdClusterReconciler")
)

type Purpose string

const (
	namePrefix    = "etcdoperator"
	finalizerName = "etcd-operator/finalizer"
	podLabelKey   = "etcd-operator-component"

	SelfSignedIssuer     = "etcd-operator-selfsigned-issuer"
	CertManagerNamespace = "cert-manager"

	EtcdCA     = Purpose("etcd-ca")
	EtcdIssuer = Purpose("etcd-issuer")
	EtcdServer = Purpose("etcd-server")
	EtcdClient = Purpose("etcd-client")
)

// EtcdClusterReconciler reconciles a EtcdCluster object
type EtcdClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=etcd.hcs.io,resources=etcdclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=etcd.hcs.io,resources=etcdclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=etcd.hcs.io,resources=etcdclusters/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the EtcdCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *EtcdClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	var etcdCluster etcdv1beta2.EtcdCluster
	if err := r.Get(ctx, req.NamespacedName, &etcdCluster); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// delete etcdCluster
	if !etcdCluster.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&etcdCluster, finalizerName) {
			controllerutil.RemoveFinalizer(&etcdCluster, finalizerName)
			if err := r.Update(ctx, &etcdCluster); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	etcdCluster.SetDefaults()
	if err := etcdCluster.Spec.Validate(); err != nil {
		return ctrl.Result{}, err
	}

	// add finalizer
	if !controllerutil.ContainsFinalizer(&etcdCluster, finalizerName) {
		controllerutil.AddFinalizer(&etcdCluster, finalizerName)
		if err := r.Update(ctx, &etcdCluster); err != nil {
			return ctrl.Result{}, err
		}
	}

	// if err := r.preparePKIWithCertManager(ctx, &etcdCluster); err != nil {
	// 	return ctrl.Result{}, err
	// }

	// pod := r.createEtcdPod(&etcdCluster)
	// if err := r.Create(ctx, pod); err != nil {
	// 	return ctrl.Result{}, err
	// }

	// sync service
	if err := r.syncEtcdService(ctx, &etcdCluster); err != nil {
		return ctrl.Result{}, err
	}

	// sync pod
	if err := r.syncEtcdPod(ctx, &etcdCluster); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// func (r *EtcdClusterReconciler) createEtcdPod(c *etcdv1beta2.EtcdCluster) *v1.Pod {
// 	pod := &v1.Pod{
// 		ObjectMeta: metav1.ObjectMeta{
// 			Namespace: c.Namespace,
// 			Name:      c.Name,
// 		},
// 	}
// 	return pod
// }

// func (r *EtcdClusterReconciler) preparePKIWithCertManager(ctx context.Context, c *etcdv1beta2.EtcdCluster) error {
// 	certManagerCRS := []client.Object{
// 		// clusterissuer for etcd
// 		&cmapi.ClusterIssuer{
// 			ObjectMeta: metav1.ObjectMeta{
// 				Name: SelfSignedIssuer,
// 			},
// 			Spec: cmapi.IssuerSpec{
// 				IssuerConfig: cmapi.IssuerConfig{
// 					SelfSigned: &cmapi.SelfSignedIssuer{},
// 				},
// 			},
// 		},
// 		// create etcd ca
// 		&cmapi.Certificate{
// 			ObjectMeta: metav1.ObjectMeta{
// 				Name:      Name(c.Name, EtcdCA),
// 				Namespace: c.Namespace,
// 				OwnerReferences: []metav1.OwnerReference{
// 					c.AsOwner(),
// 				},
// 			},
// 			Spec: cmapi.CertificateSpec{
// 				IsCA:       true,
// 				CommonName: Name(c.Name, EtcdCA),
// 				SecretName: Name(c.Name, EtcdCA),
// 				PrivateKey: &cmapi.CertificatePrivateKey{
// 					Algorithm: cmapi.ECDSAKeyAlgorithm,
// 					Size:      256,
// 				},
// 				IssuerRef: cmmeta.ObjectReference{
// 					Name:  SelfSignedIssuer,
// 					Kind:  "ClusterIssuer",
// 					Group: certmanager.GroupName,
// 				},
// 			},
// 		},
// 		// etcd issuer
// 		&cmapi.Issuer{
// 			ObjectMeta: metav1.ObjectMeta{
// 				Name:      Name(c.Name, EtcdIssuer),
// 				Namespace: c.Namespace,
// 				OwnerReferences: []metav1.OwnerReference{
// 					c.AsOwner(),
// 				},
// 			},
// 			Spec: cmapi.IssuerSpec{
// 				IssuerConfig: cmapi.IssuerConfig{
// 					CA: &cmapi.CAIssuer{
// 						SecretName: Name(c.Name, EtcdCA),
// 					},
// 				},
// 			},
// 		},
// 		// server
// 		&cmapi.Certificate{
// 			ObjectMeta: metav1.ObjectMeta{
// 				Name:      Name(c.Name, EtcdServer),
// 				Namespace: c.Namespace,
// 				OwnerReferences: []metav1.OwnerReference{
// 					c.AsOwner(),
// 				},
// 			},
// 			Spec: cmapi.CertificateSpec{
// 				IssuerRef: cmmeta.ObjectReference{
// 					Name:  Name(c.Name, EtcdIssuer),
// 					Kind:  "Issuer",
// 					Group: certmanager.GroupName,
// 				},
// 				SecretName: Name(c.Name, EtcdServer),
// 				Duration: &metav1.Duration{
// 					Duration: time.Hour * 24 * 365 * 10,
// 				},
// 				DNSNames: []string{
// 					"www.go.com",
// 				},
// 			},
// 		},
// 	}
// 	for _, obj := range certManagerCRS {
// 		if err := r.Create(ctx, obj); err != nil {
// 			if errors.IsAlreadyExists(err) {
// 				continue
// 			}
// 			return err
// 		}
// 	}

// 	return nil
// }

func (r *EtcdClusterReconciler) syncEtcdPod(ctx context.Context, c *etcdv1beta2.EtcdCluster) error {
	var initCluster string
	for index := 1; index <= c.Spec.Size; index++ {
		initCluster = initCluster + fmt.Sprintf("%s=http://%s:2380,", Name(namePrefix, c.Name, strconv.Itoa(index)), Name(namePrefix, c.Name, strconv.Itoa(index)))
	}
	initCluster = strings.Trim(initCluster, ",")

	for index := 1; index <= c.Spec.Size; index++ {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: c.Namespace,
				Name:      Name(namePrefix, c.Name, strconv.Itoa(index)),
				OwnerReferences: []metav1.OwnerReference{
					c.AsOwner(),
				},
				Labels: map[string]string{
					podLabelKey: Name(namePrefix, c.Name, strconv.Itoa(index)),
				},
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Image:           "k8s.gcr.io/etcd:3.5.1-0",
						ImagePullPolicy: v1.PullIfNotPresent,
						Name:            "etcd",
						Command: []string{
							"etcd",
							"--data-dir=/var/lib/etcd",
							"--listen-client-urls=http://0.0.0.0:2379",
							"--listen-peer-urls=http://0.0.0.0:2380",
						},
						Env: []v1.EnvVar{
							{
								Name:  "ETCD_NAME",
								Value: Name(namePrefix, c.Name, strconv.Itoa(index)),
							},
							{
								Name:  "ETCD_ADVERTISE_CLIENT_URLS",
								Value: fmt.Sprintf("http://%s:2379", Name(namePrefix, c.Name, strconv.Itoa(index))),
							},
							{
								Name:  "ETCD_INITIAL_ADVERTISE_PEER_URLS",
								Value: fmt.Sprintf("http://%s:2380", Name(namePrefix, c.Name, strconv.Itoa(index))),
							},
							{
								Name:  "ETCD_INITIAL_CLUSTER",
								Value: initCluster,
							},
							{
								Name:  "ETCD_INITIAL_CLUSTER_STATE",
								Value: "new",
							},
							{
								Name:  "ETCD_INITIAL_CLUSTER_TOKEN",
								Value: c.Name,
							},
						},
					},
				},
			},
		}
		if err := r.Create(ctx, pod); err != nil {
			if !errors.IsAlreadyExists(err) {
				return err
			}
		}
	}

	return nil
}

func (r *EtcdClusterReconciler) syncEtcdService(ctx context.Context, c *etcdv1beta2.EtcdCluster) error {
	for index := 1; index <= c.Spec.Size; index++ {
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: c.Namespace,
				Name:      Name(namePrefix, c.Name, strconv.Itoa(index)),
				OwnerReferences: []metav1.OwnerReference{
					c.AsOwner(),
				},
			},
			Spec: v1.ServiceSpec{
				Selector: map[string]string{
					podLabelKey: Name(namePrefix, c.Name, strconv.Itoa(index)),
				},
				Type:      v1.ServiceTypeClusterIP,
				ClusterIP: v1.ClusterIPNone,
				Ports: []v1.ServicePort{
					{
						Name:       "client",
						Protocol:   v1.ProtocolTCP,
						Port:       2379,
						TargetPort: intstr.FromInt(2379),
					},
					{
						Name:       "peer",
						Protocol:   v1.ProtocolTCP,
						Port:       2380,
						TargetPort: intstr.FromInt(2380),
					},
				},
			},
		}
		if err := r.Create(ctx, svc); err != nil {
			if !errors.IsAlreadyExists(err) {
				return err
			}
		}
	}

	return nil
}

func Name(prefix string, clusterName string, suffix string) string {
	return fmt.Sprintf("%s-%s-%s", prefix, clusterName, suffix)
}

// SetupWithManager sets up the controller with the Manager.
func (r *EtcdClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&etcdv1beta2.EtcdCluster{}).
		Complete(r)
}
