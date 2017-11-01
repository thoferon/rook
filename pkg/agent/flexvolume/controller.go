/*
Copyright 2017 The Rook Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

Some of the code below came from https://github.com/coreos/etcd-operator
which also has the apache 2.0 license.
*/

// Package flexvolume to manage Kubernetes storage attach events.
package flexvolume

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/coreos/pkg/capnslog"
	"github.com/rook/rook/pkg/agent/flexvolume/crd"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/operator/agent"
	"github.com/rook/rook/pkg/operator/cluster"
	"github.com/rook/rook/pkg/operator/k8sutil"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/pkg/util/version"
)

const (
	StorageClassKey       = "storageClass"
	PoolKey               = "pool"
	ImageKey              = "image"
	kubeletDefaultRootDir = "/var/lib/kubelet"
)

var driverLogger = capnslog.NewPackageLogger("github.com/rook/rook", "rook-flexdriver")

// FlexvolumeController handles all events from the Flexvolume driver
type FlexvolumeController struct {
	clientset                  kubernetes.Interface
	volumeManager              VolumeManager
	volumeAttachmentController crd.VolumeAttachmentController
}

func newFlexvolumeController(context *clusterd.Context, volumeAttachmentCRDClient rest.Interface, manager VolumeManager) (*FlexvolumeController, error) {

	var controller crd.VolumeAttachmentController
	// CRD is available on v1.7.0. TPR became deprecated on v1.7.0
	// Remove this code when TPR is not longer supported
	kubeVersion, err := k8sutil.GetK8SVersion(context.Clientset)
	if err != nil {
		return nil, fmt.Errorf("Error getting server version: %v", err)
	}
	if kubeVersion.AtLeast(version.MustParseSemantic(serverVersionV170)) {
		controller = crd.New(volumeAttachmentCRDClient)
	} else {
		controller = crd.NewTPR(context.Clientset)
	}

	return &FlexvolumeController{
		clientset:                  context.Clientset,
		volumeManager:              manager,
		volumeAttachmentController: controller,
	}, nil
}

// Attach attaches rook volume to the node
func (c *FlexvolumeController) Attach(attachOpts AttachOptions, devicePath *string) error {

	namespace := os.Getenv(k8sutil.PodNamespaceEnvVar)
	node := os.Getenv(k8sutil.NodeNameEnvVar)

	// Name of CRD is the PV name. This is done so that the CRD can be use for fencing
	crdName := attachOpts.VolumeName

	// Check if this volume has been attached
	volumeattachObj, err := c.volumeAttachmentController.Get(namespace, crdName)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get volume CRD %s. %+v", crdName, err)
		}
		// No volumeattach CRD for this volume found. Create one
		volumeattachObj = crd.NewVolumeAttachment(crdName, namespace, node, attachOpts.PodNamespace, attachOpts.Pod,
			attachOpts.MountDir, strings.ToLower(attachOpts.RW) == ReadOnly)
		logger.Infof("Creating Volume attach Resource %s/%s: %+v", volumeattachObj.Namespace, volumeattachObj.Name, attachOpts)
		err = c.volumeAttachmentController.Create(volumeattachObj)
		if err != nil {
			if !errors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create volume CRD %s. %+v", crdName, err)
			}
			// Some other attacher beat us in this race. Kubernetes will retry again.
			return fmt.Errorf("failed to attach volume %s for pod %s/%s. Volume is already attached by a different pod",
				crdName, attachOpts.PodNamespace, attachOpts.Pod)
		}
	} else {
		// Volume has already been attached.
		// find if the attachment object has been previously created.
		// This could be in the case of a multiple attachment for ROs or
		// it could be the the VolumeAttachment record was created previously and
		// the attach operation failed and Kubernetes retried.
		found := false
		for _, a := range volumeattachObj.Attachments {
			if a.MountDir == attachOpts.MountDir {
				found = true
			}
		}

		if !found {
			// Check if there is already an attachment with RW.
			index := getPodRWAttachmentObject(volumeattachObj)
			if index != -1 {
				// check if the RW attachment is orphaned.
				attachment := &volumeattachObj.Attachments[index]
				pod, err := c.clientset.Core().Pods(attachment.PodNamespace).Get(attachment.PodName, metav1.GetOptions{})
				if err != nil {
					if !errors.IsNotFound(err) {
						return fmt.Errorf("failed to get pod CRD %s/%s. %+v", attachment.PodNamespace, attachment.PodName, err)
					}

					// Attachment is orphaned. Update attachment record and proceed with attaching
					attachment.Node = node
					attachment.MountDir = attachOpts.MountDir
					attachment.PodNamespace = attachOpts.PodNamespace
					attachment.PodName = attachOpts.Pod
					attachment.ReadOnly = attachOpts.RW == ReadOnly
					err = c.volumeAttachmentController.Update(volumeattachObj)
					if err != nil {
						return fmt.Errorf("failed to update volume CRD %s. %+v", crdName, err)
					}
				} else {
					// Attachment is not orphaned. Original pod still exists. Dont attach.
					return fmt.Errorf("failed to attach volume %s for pod %s/%s. Volume is already attached by pod %s/%s. Status %+v",
						crdName, attachOpts.PodNamespace, attachOpts.Pod, attachment.PodNamespace, attachment.PodName, pod.Status.Phase)
				}
			} else {
				// No RW attachment found. Check if this is a RW attachment request.
				// We only support RW once attachment. No mixing either with RO
				if attachOpts.RW == "rw" && len(volumeattachObj.Attachments) > 0 {
					return fmt.Errorf("failed to attach volume %s for pod %s/%s. Volume is already attached by one or more pods",
						crdName, attachOpts.PodNamespace, attachOpts.Pod)
				}

				// Create a new attachment record and proceed with attaching
				newAttach := crd.Attachment{
					Node:         node,
					PodNamespace: attachOpts.PodNamespace,
					PodName:      attachOpts.Pod,
					MountDir:     attachOpts.MountDir,
					ReadOnly:     attachOpts.RW == ReadOnly,
				}
				volumeattachObj.Attachments = append(volumeattachObj.Attachments, newAttach)
				err = c.volumeAttachmentController.Update(volumeattachObj)
				if err != nil {
					return fmt.Errorf("failed to update volume CRD %s. %+v", crdName, err)
				}
			}
		}
	}
	*devicePath, err = c.volumeManager.Attach(attachOpts.Image, attachOpts.Pool, attachOpts.ClusterName)
	if err != nil {
		return fmt.Errorf("failed to attach volume %s/%s: %+v", attachOpts.Pool, attachOpts.Image, err)
	}
	return nil
}

// Detach detaches a rook volume to the node
func (c *FlexvolumeController) Detach(detachOpts AttachOptions, _ *struct{} /* void reply */) error {

	err := c.volumeManager.Detach(detachOpts.Image, detachOpts.Pool, detachOpts.ClusterName)
	if err != nil {
		return fmt.Errorf("Failed to detach volume %s/%s: %+v", detachOpts.Pool, detachOpts.Image, err)
	}

	namespace := os.Getenv(k8sutil.PodNamespaceEnvVar)
	crdName := detachOpts.VolumeName
	volumeAttach, err := c.volumeAttachmentController.Get(namespace, crdName)
	if len(volumeAttach.Attachments) == 0 {
		logger.Infof("Deleting VolumeAttachment CRD %s/%s", namespace, crdName)
		return c.volumeAttachmentController.Delete(namespace, crdName)
	}
	return nil
}

// RemoveAttachmentObject removes the attachment from the VolumeAttachment CRD and returns whether the volume is safe to detach
func (c *FlexvolumeController) RemoveAttachmentObject(detachOpts AttachOptions, safeToDetach *bool) error {
	namespace := os.Getenv(k8sutil.PodNamespaceEnvVar)
	crdName := detachOpts.VolumeName
	logger.Infof("Deleting attachment for mountDir %s from Volume attach CRD %s/%s", detachOpts.MountDir, namespace, crdName)
	volumeAttach, err := c.volumeAttachmentController.Get(namespace, crdName)
	if err != nil {
		return fmt.Errorf("failed to get Volume attach CRD %s/%s: %+v", namespace, crdName, err)
	}
	node := os.Getenv(k8sutil.NodeNameEnvVar)
	nodeAttachmentCount := 0
	needUpdate := false
	for i, v := range volumeAttach.Attachments {
		if v.Node == node {
			nodeAttachmentCount++
			if v.MountDir == detachOpts.MountDir {
				// Deleting slice
				volumeAttach.Attachments = append(volumeAttach.Attachments[:i], volumeAttach.Attachments[i+1:]...)
				needUpdate = true
			}
		}
	}

	if needUpdate {
		// only one attachment on this node, which is the one that got removed.
		if nodeAttachmentCount == 1 {
			*safeToDetach = true
		}
		return c.volumeAttachmentController.Update(volumeAttach)
	}
	return fmt.Errorf("VolumeAttachment CRD %s found but attachment to the mountDir %s was not found", crdName, detachOpts.MountDir)
}

// Log logs messages from the driver
func (c *FlexvolumeController) Log(message LogMessage, _ *struct{} /* void reply */) error {
	if message.IsError {
		driverLogger.Error(message.Message)
	} else {
		driverLogger.Info(message.Message)
	}
	return nil
}

func (c *FlexvolumeController) parseClusterName(storageClassName string) (string, error) {
	sc, err := c.clientset.Storage().StorageClasses().Get(storageClassName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	clusterName, ok := sc.Parameters["clusterName"]
	if !ok {
		// Defaults to rook if not found
		logger.Infof("clusterName not specified in the storage class %s. Defaulting to '%s'", storageClassName, cluster.DefaultClusterName)
		return cluster.DefaultClusterName, nil
	}
	return clusterName, nil
}

// GetAttachInfoFromMountDir obtain pod and volume information from the mountDir. K8s does not provide
// all necessary information to detach a volume (https://github.com/kubernetes/kubernetes/issues/52590).
// So we are hacking a bit and by parsing it from mountDir
func (c *FlexvolumeController) GetAttachInfoFromMountDir(mountDir string, attachOptions *AttachOptions) error {

	if attachOptions.PodID == "" {
		podID, pvName, err := getPodAndPVNameFromMountDir(mountDir)
		if err != nil {
			return err
		}
		attachOptions.PodID = podID
		attachOptions.VolumeName = pvName
	}

	pv, err := c.clientset.CoreV1().PersistentVolumes().Get(attachOptions.VolumeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get persistent volume %s: %+v", attachOptions.VolumeName, err)
	}

	if attachOptions.PodNamespace == "" {
		// pod namespace should be the same as the PVC namespace
		attachOptions.PodNamespace = pv.Spec.ClaimRef.Namespace
	}

	node := os.Getenv(k8sutil.NodeNameEnvVar)
	if attachOptions.Pod == "" {
		// Find all pods scheduled to this node
		opts := metav1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("spec.nodeName", node).String(),
		}
		pods, err := c.clientset.Core().Pods(attachOptions.PodNamespace).List(opts)
		if err != nil {
			return fmt.Errorf("failed to get pods in namespace %s: %+v", attachOptions.PodNamespace, err)
		}

		pod := findPodByID(pods, types.UID(attachOptions.PodID))
		if pod != nil {
			attachOptions.Pod = pod.GetName()
		}
	}

	if attachOptions.Image == "" {
		attachOptions.Image = pv.Spec.PersistentVolumeSource.FlexVolume.Options[ImageKey]
	}
	if attachOptions.Pool == "" {
		attachOptions.Pool = pv.Spec.PersistentVolumeSource.FlexVolume.Options[PoolKey]
	}
	if attachOptions.StorageClass == "" {
		attachOptions.StorageClass = pv.Spec.PersistentVolumeSource.FlexVolume.Options[StorageClassKey]
	}
	attachOptions.ClusterName, err = c.parseClusterName(attachOptions.StorageClass)
	if err != nil {
		return fmt.Errorf("Failed to parse clusterName from storageClass %s: %+v", attachOptions.StorageClass, err)
	}
	return nil
}

// GetGlobalMountPath generate the global mount path where the device path is mounted.
// It is based on the kubelet root dir, which defaults to /var/lib/kubelet
func (c *FlexvolumeController) GetGlobalMountPath(volumeName string, globalMountPath *string) error {
	*globalMountPath = path.Join(c.getKubeletRootDir(), "plugins", FlexvolumeVendor, FlexvolumeDriver, "mounts", volumeName)
	return nil
}

// getKubeletRootDir queries the kubelet configuration to find the kubelet root dir. Defaults to /var/lib/kubelet
func (c *FlexvolumeController) getKubeletRootDir() string {
	nodeConfigURI, err := k8sutil.NodeConfigURI()
	if err != nil {
		logger.Warningf(err.Error())
		return kubeletDefaultRootDir
	}

	// determining where the path of the kubelet root dir and flexvolume dir on the node
	nodeConfig, err := c.clientset.Core().RESTClient().Get().RequestURI(nodeConfigURI).DoRaw()
	if err != nil {
		logger.Warningf("unable to query node configuration: %v", err)
		return kubeletDefaultRootDir
	}
	configKubelet := agent.NodeConfigKubelet{}
	if err := json.Unmarshal(nodeConfig, &configKubelet); err != nil {
		logger.Warningf("unable to parse node config from Kubelet: %+v", err)
		return kubeletDefaultRootDir
	}

	if configKubelet.ComponentConfig.RootDirectory == "" {
		return kubeletDefaultRootDir
	}
	return configKubelet.ComponentConfig.RootDirectory
}

// getPodAndPVNameFromMountDir parses pod information from the mountDir
func getPodAndPVNameFromMountDir(mountDir string) (string, string, error) {
	// mountDir is in the form of <rootDir>/pods/<podID>/volumes/rook.io~rook/<pv name>
	filepath.Clean(mountDir)
	token := strings.Split(mountDir, string(filepath.Separator))
	// token lenght should at least size 5
	length := len(token)
	if length < 5 {
		return "", "", fmt.Errorf("failed to parse mountDir %s for CRD name and podID", mountDir)
	}
	return token[length-4], token[length-1], nil
}

func findPodByID(pods *v1.PodList, podUID types.UID) *v1.Pod {
	for i := range pods.Items {
		if pods.Items[i].GetUID() == podUID {
			return &(pods.Items[i])
		}
	}
	return nil
}

// getPodRWAttachmentObject loops through the list of attachments of the VolumeAttachment
// resource and returns the index of the first RW attachment object
func getPodRWAttachmentObject(volumeAttachmentObject crd.VolumeAttachment) int {
	for i, a := range volumeAttachmentObject.Attachments {
		if !a.ReadOnly {
			return i
		}
	}
	return -1
}
