package main

import (
	"encoding/json"
	"fmt"
	"log"
	"path"
	"strconv"
	"strings"

	"k8s.io/api/admission/v1beta1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
)

const defaultRegion = "us-east-1"

func cleanName(name string) string {
	return strings.ReplaceAll(name, "_", "-")
}

func (s *server) addMount(name, accessKey, secretKey, endpoint, region, bucket, mountPath string) []map[string]interface{} {
	patches := make([]map[string]interface{}, 0)

	// Add volume definition
	patches = append(patches, map[string]interface{}{
		"op":   "add",
		"path": "/spec/volumes/-",
		"value": v1.Volume{
			Name: name,
			VolumeSource: v1.VolumeSource{
				FlexVolume: &v1.FlexVolumeSource{
					Driver: "informaticslab/goofys-flex-volume",
					Options: map[string]string{
						"bucket":     bucket,
						"endpoint":   endpoint,
						"region":     region,
						"access-key": accessKey,
						"secret-key": secretKey,
						"uid":        "1000",
						"gid":        "100",
					},
				},
			},
		},
	})

	// Add VolumeMount
	patches = append(patches, map[string]interface{}{
		"op":   "add",
		"path": "/spec/containers/0/volumeMounts/-",
		"value": v1.VolumeMount{
			Name:      name,
			MountPath: mountPath,
		},
	})

	return patches
}

func (s *server) addBoathouseMount(name, vaultPath, endpoint, region, bucket, mountPath string, containerIndex int) []map[string]interface{} {
	patches := make([]map[string]interface{}, 0)

	// Add volume definition
	patches = append(patches, map[string]interface{}{
		"op":   "add",
		"path": "/spec/volumes/-",
		"value": v1.Volume{
			Name: name,
			VolumeSource: v1.VolumeSource{
				FlexVolume: &v1.FlexVolumeSource{
					Driver: "statcan.gc.ca/boathouse",
					Options: map[string]string{
						"bucket":     bucket,
						"endpoint":   endpoint,
						"region":     region,
						"vault-path": vaultPath,
						"vault-ttl":  "24h",
						"uid":        "1000",
						"gid":        "100",
					},
				},
			},
		},
	})

	// Add VolumeMount
	patches = append(patches, map[string]interface{}{
		"op":   "add",
		"path": fmt.Sprintf("/spec/containers/%d/volumeMounts/-", containerIndex),
		"value": v1.VolumeMount{
			Name:      name,
			MountPath: mountPath,
		},
	})

	return patches
}

func (s *server) addInstance(instance, mount, endpoint, region, profile, base string) []map[string]interface{} {
	patches := make([]map[string]interface{}, 0)

	// Attempt to request a token from Vault for Minio
	creds, err := s.vault.Logical().Read(fmt.Sprintf("%s/keys/profile-%s", mount, profile))
	if err != nil {
		klog.Warningf("unable to obtain MinIO token at %s/%s: %v", mount, profile, err)
		return patches
	}

	accessKey := creds.Data["accessKeyId"].(string)
	secretKey := creds.Data["secretAccessKey"].(string)

	// Mount private
	patches = append(patches, s.addMount(fmt.Sprintf("%s-private", instance), accessKey, secretKey, endpoint, region, profile, path.Join(base, "private"))...)

	// Mount shared
	patches = append(patches, s.addMount(fmt.Sprintf("%s-shared", instance), accessKey, secretKey, endpoint, region, "shared", path.Join(base, "shared"))...)

	return patches
}

func (s *server) addBoathouseInstance(instance, mount, endpoint, region, profile, base string, containerIndex int) []map[string]interface{} {
	patches := make([]map[string]interface{}, 0)

	vaultPath := fmt.Sprintf("%s/keys/profile-%s", mount, profile)

	// Mount private
	patches = append(patches, s.addBoathouseMount(fmt.Sprintf("%s-private", instance), vaultPath, endpoint, region, profile, path.Join(base, "private"), containerIndex)...)

	// Mount shared
	patches = append(patches, s.addBoathouseMount(fmt.Sprintf("%s-shared", instance), vaultPath, endpoint, region, "shared", path.Join(base, "shared"), containerIndex)...)

	return patches
}

func (s *server) mutate(request v1beta1.AdmissionRequest) (v1beta1.AdmissionResponse, error) {
	response := v1beta1.AdmissionResponse{}

	// Default response
	response.Allowed = true
	response.UID = request.UID

	patch := v1beta1.PatchTypeJSONPatch
	response.PatchType = &patch

	patches := make([]map[string]interface{}, 0)

	// Decode the pod object
	var err error
	pod := v1.Pod{}
	if err = json.Unmarshal(request.Object.Raw, &pod); err != nil {
		return response, fmt.Errorf("unable to decode Pod %w", err)
	}

	log.Printf("Check pod for notebook %s/%s", pod.Namespace, pod.Name)

	// Only inject when matching label
	inject := false
	containerIndex := 0

	profile := cleanName(pod.Namespace)

	// If we have the right annotations
	if val, ok := pod.ObjectMeta.Annotations["data.statcan.gc.ca/inject-boathouse"]; ok {
		bval, err := strconv.ParseBool(val)
		if err != nil {
			return response, fmt.Errorf("unable to decode data.statcan.gc.ca/boathouse-inject annotation %w", err)
		}
		inject = bval
	}

	// If we have a Argo workflow, then lets run the logic
	if _, ok := pod.ObjectMeta.Labels["workflows.argoproj.io/workflow"]; ok {
		// Check the name of the first container in the pod.
		// If it's called "wait", then we want to add the mount to the second container.
		if pod.Spec.Containers[0].Name == "wait" {
			containerIndex = 1
		} else {
			containerIndex = 0
		}
	}

	// TEMP: Until boathouse supports the Protected B configuration,
	// we will not operated on pods with a Protected B classification.
	if val, ok := pod.ObjectMeta.Labels["data.statcan.gc.ca/classification"]; ok {
		if val == "protected-b" {
			inject = false
		}
	}

	if inject {
		for _, instance := range instances {
			patches = append(patches,
				s.addBoathouseInstance(
					strings.Replace(instance.Name, "_", "-", -1),
					instance.Name,
					instance.ExternalUrl,
					defaultRegion,
					profile,
					fmt.Sprintf("/home/jovyan/minio/%s", instance.Short),
					containerIndex)...)
		}

		response.AuditAnnotations = map[string]string{
			"goofys-injector": "Added MinIO volume mounts",
		}
		response.Patch, err = json.Marshal(patches)
		if err != nil {
			return response, err
		}

		response.Result = &metav1.Status{
			Status: metav1.StatusSuccess,
		}
	}

	return response, nil
}
