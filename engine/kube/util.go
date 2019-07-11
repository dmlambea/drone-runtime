// Copyright 2019 Drone.IO Inc. All rights reserved.
// Use of this source code is governed by the Drone Non-Commercial License
// that can be found in the LICENSE file.

package kube

import (
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/drone/drone-runtime/engine"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	envAutomountServiceAccountToken = "PLUGIN_AUTOMOUNTSERVICEACCOUNTTOKEN"
)

// TODO(bradrydzewski) enable container resource limits.

// helper function converts environment variable
// string data to kubernetes variables. Special care is to be put
// on kubernetes-specific envvars, designed to tune the pod.
func toEnv(spec *engine.Spec, step *engine.Step) []v1.EnvVar {
	var to []v1.EnvVar
	for k, v := range step.Envs {
		if k != envAutomountServiceAccountToken {
			to = append(to, v1.EnvVar{Name: k, Value: v})
		}
	}
	to = append(to, v1.EnvVar{
		Name: "KUBERNETES_NODE",
		ValueFrom: &v1.EnvVarSource{
			FieldRef: &v1.ObjectFieldSelector{
				FieldPath: "spec.nodeName",
			},
		},
	})
	for _, secret := range step.Secrets {
		sec, ok := engine.LookupSecret(spec, secret.Name)
		if !ok {
			continue
		}
		optional := true
		to = append(to, v1.EnvVar{
			Name: secret.Env,
			ValueFrom: &v1.EnvVarSource{
				SecretKeyRef: &v1.SecretKeySelector{
					LocalObjectReference: v1.LocalObjectReference{
						Name: sec.Metadata.UID,
					},
					Key:      sec.Metadata.UID,
					Optional: &optional,
				},
			},
		})
	}
	return to
}

// helper function converts the engine pull policy
// to the kubernetes pull policy constant.
func toPullPolicy(from engine.PullPolicy) v1.PullPolicy {
	switch from {
	case engine.PullAlways:
		return v1.PullAlways
	case engine.PullNever:
		return v1.PullNever
	case engine.PullIfNotExists:
		return v1.PullIfNotPresent
	default:
		return v1.PullIfNotPresent
	}
}

// helper function converts the engine secret object
// to the kubernetes secret object.
func toSecret(spec *engine.Spec, from *engine.Secret) *v1.Secret {
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: from.Metadata.UID,
		},
		Type: "Opaque",
		StringData: map[string]string{
			from.Metadata.UID: from.Data,
		},
	}
}

func toConfigVolumes(spec *engine.Spec, step *engine.Step) []v1.Volume {
	var to []v1.Volume
	for _, mount := range step.Files {
		file, ok := engine.LookupFile(spec, mount.Name)
		if !ok {
			continue
		}
		mode := int32(mount.Mode)
		volume := v1.Volume{Name: file.Metadata.UID}

		optional := false
		volume.ConfigMap = &v1.ConfigMapVolumeSource{
			LocalObjectReference: v1.LocalObjectReference{
				Name: file.Metadata.UID,
			},
			Optional: &optional,
			Items: []v1.KeyToPath{
				{
					Key:  file.Metadata.UID,
					Path: path.Base(mount.Path), // use the base path. document this.
					Mode: &mode,
				},
			},
		}
		to = append(to, volume)
	}
	return to
}

func toConfigMounts(spec *engine.Spec, step *engine.Step) []v1.VolumeMount {
	var to []v1.VolumeMount
	for _, mount := range step.Files {
		file, ok := engine.LookupFile(spec, mount.Name)
		if !ok {
			continue
		}
		volume := v1.VolumeMount{
			Name:      file.Metadata.UID,
			MountPath: path.Dir(mount.Path), // mount the config map here, using the base path
		}
		to = append(to, volume)
	}
	return to
}

func toVolumes(spec *engine.Spec, step *engine.Step) []v1.Volume {
	var to []v1.Volume
	for _, mount := range step.Volumes {
		vol, ok := engine.LookupVolume(spec, mount.Name)
		if !ok {
			continue
		}
		switch {
		case vol.EmptyDir != nil, vol.HostPath != nil:
			// volume.EmptyDir = &v1.EmptyDirVolumeSource{}

			// NOTE the empty_dir cannot be shared across multiple
			// pods so we emulate its behavior, and mount a temp
			// directory on the host machine that can be shared
			// between pods. This means we are responsible for deleting
			// these directories.
			to = append(to, toHostPathVolume(spec, vol))
		case vol.Secret != nil:
			to = append(to, toSecretVolumes(spec, vol)...)
		}
	}
	return to
}

func toHostPathVolume(spec *engine.Spec, vol *engine.Volume) (hostPathVolume v1.Volume) {
	var path string
	if vol.EmptyDir != nil {
		path = filepath.Join("/tmp", "drone", spec.Metadata.Namespace, vol.Metadata.UID)
	} else {
		path = vol.HostPath.Path
	}
	srcType := v1.HostPathDirectoryOrCreate
	hostPathVolume.Name = vol.Metadata.UID
	hostPathVolume.HostPath = &v1.HostPathVolumeSource{
		Path: path,
		Type: &srcType,
	}
	return
}

// secret volumes must be created one per secret, due to the current
// structure of secrets in spec
func toSecretVolumes(spec *engine.Spec, vol *engine.Volume) (volumeList []v1.Volume) {
	for _, item := range vol.Secret.Items {
		secretName := fmt.Sprintf("%s-%s-%s", vol.Metadata.Name, vol.Secret.Name, item.Key)
		sec, ok := engine.LookupSecret(spec, secretName)
		if !ok {
			continue
		}
		secretVolume := v1.Volume{
			Name: fmt.Sprintf("%s-%s", vol.Metadata.UID, item.Key),
		}
		secretVolume.Secret = &v1.SecretVolumeSource{
			SecretName: sec.Metadata.UID,
			Items: []v1.KeyToPath{
				v1.KeyToPath{
					Key:  sec.Metadata.UID,
					Path: item.Path,
					Mode: item.Mode,
				},
			},
		}
		volumeList = append(volumeList, secretVolume)
	}
	return volumeList
}

func toVolumeMounts(spec *engine.Spec, step *engine.Step) []v1.VolumeMount {
	var to []v1.VolumeMount
	for _, mount := range step.Volumes {
		vol, ok := engine.LookupVolume(spec, mount.Name)
		if !ok {
			continue
		}
		switch {
		case vol.Secret != nil:
			for _, item := range vol.Secret.Items {
				to = append(to, v1.VolumeMount{
					Name:      fmt.Sprintf("%s-%s", vol.Metadata.UID, item.Key),
					MountPath: fmt.Sprintf("%s/%s", mount.Path, item.Path),
					SubPath:   item.Path,
				})
			}
		default:
			to = append(to, v1.VolumeMount{
				Name:      vol.Metadata.UID,
				MountPath: mount.Path,
			})
		}

	}
	return to
}

func toPorts(step *engine.Step) []v1.ContainerPort {
	if len(step.Docker.Ports) == 0 {
		return nil
	}
	var ports []v1.ContainerPort
	for _, port := range step.Docker.Ports {
		ports = append(ports, v1.ContainerPort{
			ContainerPort: int32(port.Port),
		})
	}
	return ports
}

// helper function returns a kubernetes namespace
// for the given specification.
func toNamespace(spec *engine.Spec) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   spec.Metadata.Namespace,
			Labels: spec.Metadata.Labels,
		},
	}
}

func toResources(step *engine.Step) v1.ResourceRequirements {
	var resources v1.ResourceRequirements
	if step.Resources != nil && step.Resources.Limits != nil {
		resources.Limits = v1.ResourceList{}
		if step.Resources.Limits.Memory > int64(0) {
			resources.Limits[v1.ResourceMemory] = *resource.NewQuantity(
				step.Resources.Limits.Memory, resource.BinarySI)
		}
		if step.Resources.Limits.CPU > int64(0) {
			resources.Limits[v1.ResourceCPU] = *resource.NewMilliQuantity(
				step.Resources.Limits.CPU, resource.DecimalSI)
		}
	}
	if step.Resources != nil && step.Resources.Requests != nil {
		resources.Requests = v1.ResourceList{}
		if step.Resources.Requests.Memory > int64(0) {
			resources.Requests[v1.ResourceMemory] = *resource.NewQuantity(
				step.Resources.Requests.Memory, resource.BinarySI)
		}
		if step.Resources.Requests.CPU > int64(0) {
			resources.Requests[v1.ResourceCPU] = *resource.NewMilliQuantity(
				step.Resources.Requests.CPU, resource.DecimalSI)
		}
	}
	return resources
}

// helper function returns a kubernetes pod for the
// given step and specification.
func toPod(spec *engine.Spec, step *engine.Step) *v1.Pod {
	var volumes []v1.Volume
	volumes = append(volumes, toVolumes(spec, step)...)
	volumes = append(volumes, toConfigVolumes(spec, step)...)

	var mounts []v1.VolumeMount
	mounts = append(mounts, toVolumeMounts(spec, step)...)
	mounts = append(mounts, toConfigMounts(spec, step)...)

	var pullSecrets []v1.LocalObjectReference
	if len(spec.Docker.Auths) > 0 {
		pullSecrets = []v1.LocalObjectReference{{
			Name: "docker-auth-config", // TODO move name to a const
		}}
	}

	automountServiceAccountToken := false
	for k, v := range step.Envs {
		if k == envAutomountServiceAccountToken {
			automountServiceAccountToken, _ = strconv.ParseBool(v)
			break
		}
	}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      step.Metadata.UID,
			Namespace: step.Metadata.Namespace,
			Labels:    step.Metadata.Labels,
		},
		Spec: v1.PodSpec{
			AutomountServiceAccountToken: &automountServiceAccountToken,
			RestartPolicy:                v1.RestartPolicyNever,
			Containers: []v1.Container{{
				Name:            step.Metadata.UID,
				Image:           step.Docker.Image,
				ImagePullPolicy: toPullPolicy(step.Docker.PullPolicy),
				Command:         step.Docker.Command,
				Args:            step.Docker.Args,
				WorkingDir:      step.WorkingDir,
				SecurityContext: &v1.SecurityContext{
					Privileged: &step.Docker.Privileged,
				},
				Env:          toEnv(spec, step),
				VolumeMounts: mounts,
				Ports:        toPorts(step),
				Resources:    toResources(step),
			}},
			ImagePullSecrets: pullSecrets,
			Volumes:          volumes,
		},
	}
}

// helper function returns a kubernetes service for the
// given step and specification.
func toService(spec *engine.Spec, step *engine.Step) *v1.Service {
	var ports []v1.ServicePort
	for _, p := range step.Docker.Ports {
		source := p.Port
		target := p.Host
		if target == 0 {
			target = source
		}
		ports = append(ports, v1.ServicePort{
			Name: strconv.Itoa(source),
			Port: int32(source),
			TargetPort: intstr.IntOrString{
				IntVal: int32(target),
			},
		})
	}
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      toDNS(step.Metadata.Name),
			Namespace: step.Metadata.Namespace,
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeClusterIP,
			Selector: map[string]string{
				"io.drone.step.name": step.Metadata.Name,
			},
			Ports: ports,
		},
	}
}

func toDNS(i string) string {
	return strings.Replace(i, "_", "-", -1)
}

func stringptr(v string) *string {
	return &v
}
