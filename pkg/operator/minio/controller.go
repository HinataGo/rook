/*
Copyright 2018 The Rook Authors. All rights reserved.

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

// Package minio to manage a Minio object store.
package minio

import (
	"fmt"
	"reflect"

	"github.com/coreos/pkg/capnslog"
	opkit "github.com/rook/operator-kit"
	miniov1alpha1 "github.com/rook/rook/pkg/apis/minio.rook.io/v1alpha1"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/operator/k8sutil"
	"k8s.io/api/apps/v1beta2"
	"k8s.io/api/core/v1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
)

// TODO: A lot of these constants are specific to the KubeCon demo. Let's
// revisit these and determine what should be specified in the resource spec.
const (
	objectStoreDataDirTemplate  = "/data/%s"
	objectStoreDataEmptyDirName = "minio-data"
	customResourceName          = "objectstore"
	customResourceNamePlural    = "objectstores"
	minioCtrName                = "minio"
	minioLabel                  = "minio"
	minioPVCName                = "minio-pvc"
	minioServerSuffixFmt        = "%s.svc.%s" // namespace.svc.clusterDomain, e.g., default.svc.cluster.local
	minioPort                   = int32(9000)
)

var logger = capnslog.NewPackageLogger("github.com/rook/rook", "minio-op-object")

// ObjectStoreResource represents the object store custom resource
var ObjectStoreResource = opkit.CustomResource{
	Name:    customResourceName,
	Plural:  customResourceNamePlural,
	Group:   miniov1alpha1.CustomResourceGroup,
	Version: miniov1alpha1.Version,
	Scope:   apiextensionsv1beta1.NamespaceScoped,
	Kind:    reflect.TypeOf(miniov1alpha1.ObjectStore{}).Name(),
}

// MinioController represents a controller object for object store custom resources
type MinioController struct {
	context   *clusterd.Context
	rookImage string
}

// NewMinioController create controller for watching object store custom resources created
func NewMinioController(context *clusterd.Context, rookImage string) *MinioController {
	return &MinioController{
		context:   context,
		rookImage: rookImage,
	}
}

// StartWatch watches for instances of ObjectStore custom resources and acts on them
func (c *MinioController) StartWatch(namespace string, stopCh chan struct{}) error {
	resourceHandlerFuncs := cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onAdd,
		UpdateFunc: c.onUpdate,
		DeleteFunc: c.onDelete,
	}

	logger.Infof("start watching object store resources in namespace %s", namespace)
	watcher := opkit.NewWatcher(ObjectStoreResource, namespace, resourceHandlerFuncs, c.context.RookClientset.MinioV1alpha1().RESTClient())
	go watcher.Watch(&miniov1alpha1.ObjectStore{}, stopCh)

	return nil
}

func (c *MinioController) makeMinioHeadlessService(name, namespace string, spec miniov1alpha1.ObjectStoreSpec, ownerRef meta_v1.OwnerReference) (*v1.Service, error) {
	coreV1Client := c.context.Clientset.CoreV1()

	svc, err := coreV1Client.Services(namespace).Create(&v1.Service{
		ObjectMeta: meta_v1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{k8sutil.AppAttr: minioLabel},
		},
		Spec: v1.ServiceSpec{
			Selector:  map[string]string{k8sutil.AppAttr: minioLabel},
			Ports:     []v1.ServicePort{{Port: minioPort}},
			ClusterIP: v1.ClusterIPNone,
		},
	})
	k8sutil.SetOwnerRef(c.context.Clientset, namespace, &svc.ObjectMeta, &ownerRef)

	return svc, err
}

func (c *MinioController) buildMinioCtrArgs(statefulSetPrefix, headlessServiceName, namespace, clusterDomain string, serverCount int32, volumeMounts []v1.VolumeMount) []string {
	args := []string{"server"}
	for i := int32(0); i < serverCount; i++ {
		for _, mount := range volumeMounts {
			args = append(args, makeServerAddress(statefulSetPrefix, headlessServiceName, namespace, clusterDomain, i, getPVCDataDir(mount.Name)))
		}
	}

	logger.Infof("Building Minio container args: %v", args)
	return args
}

// Generates the full server address for the given server params, e.g., http://my-store-0.my-store.rook-minio.svc.cluster.local/data
func makeServerAddress(statefulSetPrefix, headlessServiceName, namespace, clusterDomain string, serverNum int32, pvcDataDir string) string {
	if clusterDomain == "" {
		clusterDomain = miniov1alpha1.ClusterDomainDefault
	}

	dnsSuffix := fmt.Sprintf(minioServerSuffixFmt, namespace, clusterDomain)
	return fmt.Sprintf("http://%s-%d.%s.%s%s", statefulSetPrefix, serverNum, headlessServiceName, dnsSuffix, pvcDataDir)
}

func (c *MinioController) makeMinioPodSpec(name, namespace string, ctrName string, ctrImage string, clusterDomain string, envVars map[string]string, numServers int32, volumeClaims []v1.PersistentVolumeClaim) v1.PodTemplateSpec {
	var env []v1.EnvVar
	for k, v := range envVars {
		env = append(env, v1.EnvVar{Name: k, Value: v})
	}

	volumes := []v1.Volume{}
	volumeMounts := []v1.VolumeMount{}
	if len(volumeClaims) > 0 {
		for i := range volumeClaims {
			volumeMounts = append(volumeMounts, v1.VolumeMount{
				Name:      volumeClaims[i].GetName(),
				MountPath: getPVCDataDir(volumeClaims[i].GetName()),
			})
		}
	} else {
		volumes = append(volumes, v1.Volume{
			Name: objectStoreDataEmptyDirName,
			VolumeSource: v1.VolumeSource{
				// TODO should the size limit be configurable (only) on empty dir?
				EmptyDir: &v1.EmptyDirVolumeSource{},
			},
		})
		volumeMounts = append(volumeMounts, v1.VolumeMount{
			Name:      objectStoreDataEmptyDirName,
			MountPath: fmt.Sprintf(objectStoreDataDirTemplate, objectStoreDataEmptyDirName),
		})
	}

	podSpec := v1.PodTemplateSpec{
		ObjectMeta: meta_v1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{k8sutil.AppAttr: minioLabel},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:         ctrName,
					Image:        ctrImage,
					Env:          env,
					Command:      []string{"/usr/bin/minio"},
					Ports:        []v1.ContainerPort{{ContainerPort: minioPort}},
					Args:         c.buildMinioCtrArgs(name, name, namespace, clusterDomain, numServers, volumeMounts),
					VolumeMounts: volumeMounts,
				},
			},
			Volumes: volumes,
		},
	}

	return podSpec
}

func (c *MinioController) getAccessCredentials(secretName, namespace string) (string, string, error) {
	coreV1Client := c.context.Clientset.CoreV1()
	var getOpts meta_v1.GetOptions
	val, err := coreV1Client.Secrets(namespace).Get(secretName, getOpts)
	if err != nil {
		logger.Errorf("Unable to get secret with name=%s in namespace=%s: %v", secretName, namespace, err)
		return "", "", err
	}

	return string(val.Data["username"]), string(val.Data["password"]), nil
}

func validateObjectStoreSpec(spec miniov1alpha1.ObjectStoreSpec) error {
	// Verify node count.
	count := spec.Storage.NodeCount
	if count < 4 || count%2 != 0 {
		return fmt.Errorf("node count must be greater than 3 and even")
	}

	return nil
}

func (c *MinioController) makeMinioStatefulSet(name, namespace string, spec miniov1alpha1.ObjectStoreSpec, ownerRef meta_v1.OwnerReference) (*v1beta2.StatefulSet, error) {
	appsClient := c.context.Clientset.AppsV1beta2()

	accessKey, secretKey, err := c.getAccessCredentials(spec.Credentials.Name, spec.Credentials.Namespace)
	if err != nil {
		return nil, err
	}

	envVars := map[string]string{
		"MINIO_ACCESS_KEY": accessKey,
		"MINIO_SECRET_KEY": secretKey,
	}

	podSpec := c.makeMinioPodSpec(name, namespace, minioCtrName, c.rookImage, spec.ClusterDomain, envVars, int32(spec.Storage.NodeCount), spec.Storage.VolumeClaimTemplates)

	nodeCount := int32(spec.Storage.NodeCount)
	ss := v1beta2.StatefulSet{
		ObjectMeta: meta_v1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{k8sutil.AppAttr: minioLabel},
		},
		Spec: v1beta2.StatefulSetSpec{
			Replicas: &nodeCount,
			Selector: &meta_v1.LabelSelector{
				MatchLabels: map[string]string{k8sutil.AppAttr: minioLabel},
			},
			Template:             podSpec,
			VolumeClaimTemplates: spec.Storage.VolumeClaimTemplates,
			ServiceName:          name,
			// TODO: liveness probe
		},
	}
	k8sutil.SetOwnerRef(c.context.Clientset, namespace, &ss.ObjectMeta, &ownerRef)

	return appsClient.StatefulSets(namespace).Create(&ss)
}

func (c *MinioController) onAdd(obj interface{}) {
	objectstore := obj.(*miniov1alpha1.ObjectStore).DeepCopy()

	ownerRef := meta_v1.OwnerReference{
		APIVersion: ObjectStoreResource.Version,
		Kind:       ObjectStoreResource.Kind,
		Name:       objectstore.Namespace,
		UID:        types.UID(objectstore.ObjectMeta.UID),
	}

	// Validate object store config.
	err := validateObjectStoreSpec(objectstore.Spec)
	if err != nil {
		logger.Errorf("failed to validate object store config")
		return
	}

	// Create the headless service.
	logger.Infof("Creating Minio headless service %s in namespace %s.", objectstore.Name, objectstore.Namespace)
	_, err = c.makeMinioHeadlessService(objectstore.Name, objectstore.Namespace, objectstore.Spec, ownerRef)
	if err != nil {
		logger.Errorf("failed to create minio headless service: %v", err)
		return
	}
	logger.Infof("Finished creating Minio headless service %s in namespace %s.", objectstore.Name, objectstore.Namespace)

	// Create the stateful set.
	logger.Infof("Creating Minio stateful set %s.", objectstore.Name)
	_, err = c.makeMinioStatefulSet(objectstore.Name, objectstore.Namespace, objectstore.Spec, ownerRef)
	if err != nil {
		logger.Errorf("failed to create minio stateful set: %v", err)
		return
	}
	logger.Infof("Finished creating Minio stateful set %s in namespace %s.", objectstore.Name, objectstore.Namespace)
}

func (c *MinioController) onUpdate(oldObj, newObj interface{}) {
	oldStore := oldObj.(*miniov1alpha1.ObjectStore).DeepCopy()
	newStore := newObj.(*miniov1alpha1.ObjectStore).DeepCopy()

	_ = oldStore
	_ = newStore

	logger.Infof("Received update on object store %s in namespace %s. This is currently unsupported.", oldStore.Name, oldStore.Namespace)
}

func (c *MinioController) onDelete(obj interface{}) {
	objectstore := obj.(*miniov1alpha1.ObjectStore).DeepCopy()
	logger.Infof("Delete Minio object store %s", objectstore.Name)

	// Cleanup is handled by the owner references set in 'onAdd' and the k8s garbage collector.
}

func getPVCDataDir(pvcName string) string {
	return fmt.Sprintf(objectStoreDataDirTemplate, pvcName)
}
