package resourcemerge

import (
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// EnsureConfigMap ensures that the existing matches the required.
// modified is set to true when existing had to be updated with required.
func EnsureConfigMap(modified *bool, existing *corev1.ConfigMap, required corev1.ConfigMap) {
	EnsureObjectMeta(modified, &existing.ObjectMeta, required.ObjectMeta)

	mergeMap(modified, &existing.Data, required.Data)
}

// ensurePodTemplateSpec ensures that the existing matches the required.
// modified is set to true when existing had to be updated with required.
func ensurePodTemplateSpec(modified *bool, existing *corev1.PodTemplateSpec, required corev1.PodTemplateSpec) {
	EnsureObjectMeta(modified, &existing.ObjectMeta, required.ObjectMeta)

	ensurePodSpec(modified, &existing.Spec, required.Spec)
}

func ensurePodSpec(modified *bool, existing *corev1.PodSpec, required corev1.PodSpec) {
	ensureContainers(modified, &existing.InitContainers, required.InitContainers)
	ensureContainers(modified, &existing.Containers, required.Containers)

	// any volume we specify, we require.
	for _, required := range required.Volumes {
		var existingCurr *corev1.Volume
		for j, curr := range existing.Volumes {
			if curr.Name == required.Name {
				existingCurr = &existing.Volumes[j]
				break
			}
		}
		if existingCurr == nil {
			*modified = true
			existing.Volumes = append(existing.Volumes, corev1.Volume{})
			existingCurr = &existing.Volumes[len(existing.Volumes)-1]
		}
		ensureVolume(modified, existingCurr, required)
	}

	if len(required.RestartPolicy) > 0 {
		if existing.RestartPolicy != required.RestartPolicy {
			*modified = true
			existing.RestartPolicy = required.RestartPolicy
		}
	}

	setStringIfSet(modified, &existing.ServiceAccountName, required.ServiceAccountName)
	setBool(modified, &existing.HostNetwork, required.HostNetwork)
	mergeMap(modified, &existing.NodeSelector, required.NodeSelector)
	ensurePodSecurityContextPtr(modified, &existing.SecurityContext, required.SecurityContext)
	ensureAffinityPtr(modified, &existing.Affinity, required.Affinity)
	ensureTolerations(modified, &existing.Tolerations, required.Tolerations)
	setStringIfSet(modified, &existing.PriorityClassName, required.PriorityClassName)
	setInt32Ptr(modified, &existing.Priority, required.Priority)
	setBoolPtr(modified, &existing.ShareProcessNamespace, required.ShareProcessNamespace)
	ensureDNSPolicy(modified, &existing.DNSPolicy, required.DNSPolicy)
	setInt64Ptr(modified, &existing.TerminationGracePeriodSeconds, required.TerminationGracePeriodSeconds)
}

func ensureContainers(modified *bool, existing *[]corev1.Container, required []corev1.Container) {
	for i := len(*existing) - 1; i >= 0; i-- {
		existingContainer := &(*existing)[i]
		var existingCurr *corev1.Container
		for _, requiredContainer := range required {
			if existingContainer.Name == requiredContainer.Name {
				existingCurr = &(*existing)[i]
				ensureContainer(modified, existingCurr, requiredContainer)
				break
			}
		}
		if existingCurr == nil {
			*modified = true
			*existing = append((*existing)[:i], (*existing)[i+1:]...)
		}
	}

	for _, requiredContainer := range required {
		match := false
		for _, existingContainer := range *existing {
			if existingContainer.Name == requiredContainer.Name {
				match = true
				break
			}
		}
		if !match {
			*modified = true
			*existing = append(*existing, requiredContainer)
		}
	}
}

func ensureContainer(modified *bool, existing *corev1.Container, required corev1.Container) {
	setStringIfSet(modified, &existing.Name, required.Name)
	setStringIfSet(modified, &existing.Image, required.Image)

	// if you want modify the launch, you need to modify it in the config, not in the launch args
	setStringSlice(modified, &existing.Command, required.Command)
	setStringSlice(modified, &existing.Args, required.Args)
	ensureEnvVar(modified, &existing.Env, required.Env)
	ensureEnvFromSource(modified, &existing.EnvFrom, required.EnvFrom)
	setStringIfSet(modified, &existing.WorkingDir, required.WorkingDir)
	ensureResourceRequirements(modified, &existing.Resources, required.Resources)
	ensureContainerPorts(modified, &existing.Ports, required.Ports)

	// any volume mount we specify, we require
	for _, required := range required.VolumeMounts {
		var existingCurr *corev1.VolumeMount
		for j, curr := range existing.VolumeMounts {
			if curr.Name == required.Name {
				existingCurr = &existing.VolumeMounts[j]
				break
			}
		}
		if existingCurr == nil {
			*modified = true
			existing.VolumeMounts = append(existing.VolumeMounts, corev1.VolumeMount{})
			existingCurr = &existing.VolumeMounts[len(existing.VolumeMounts)-1]
		}
		ensureVolumeMount(modified, existingCurr, required)
	}

	ensureProbePtr(modified, &existing.LivenessProbe, required.LivenessProbe)
	ensureProbePtr(modified, &existing.ReadinessProbe, required.ReadinessProbe)

	// our security context should always win
	ensureSecurityContextPtr(modified, &existing.SecurityContext, required.SecurityContext)
}

func ensureEnvVar(modified *bool, existing *[]corev1.EnvVar, required []corev1.EnvVar) {
	if required == nil {
		return
	}
	if !equality.Semantic.DeepEqual(required, *existing) {
		*existing = required
		*modified = true
	}
}

func ensureEnvFromSource(modified *bool, existing *[]corev1.EnvFromSource, required []corev1.EnvFromSource) {
	if required == nil {
		return
	}
	if !equality.Semantic.DeepEqual(required, *existing) {
		*existing = required
		*modified = true
	}
}

func ensureProbePtr(modified *bool, existing **corev1.Probe, required *corev1.Probe) {
	if *existing == nil && required == nil {
		return
	}
	if *existing == nil || required == nil {
		*modified = true
		*existing = required
		return
	}
	ensureProbe(modified, *existing, *required)
}

func ensureProbe(modified *bool, existing *corev1.Probe, required corev1.Probe) {
	setInt32(modified, &existing.InitialDelaySeconds, required.InitialDelaySeconds)
	setInt32(modified, &existing.TimeoutSeconds, required.TimeoutSeconds)
	setInt32(modified, &existing.PeriodSeconds, required.PeriodSeconds)
	setInt32(modified, &existing.SuccessThreshold, required.SuccessThreshold)
	setInt32(modified, &existing.FailureThreshold, required.FailureThreshold)

	ensureProbeHandler(modified, &existing.ProbeHandler, required.ProbeHandler)
}

func ensureProbeHandler(modified *bool, existing *corev1.ProbeHandler, required corev1.ProbeHandler) {
	if !equality.Semantic.DeepEqual(required, *existing) {
		*modified = true
		*existing = required
	}
}

func ensureContainerPorts(modified *bool, existing *[]corev1.ContainerPort, required []corev1.ContainerPort) {
	for i := len(*existing) - 1; i >= 0; i-- {
		existingContainerPort := &(*existing)[i]
		var existingCurr *corev1.ContainerPort
		for _, requiredContainerPort := range required {
			if existingContainerPort.Name == requiredContainerPort.Name {
				existingCurr = &(*existing)[i]
				ensureContainerPort(modified, existingCurr, requiredContainerPort)
				break
			}
		}
		if existingCurr == nil {
			*modified = true
			*existing = append((*existing)[:i], (*existing)[i+1:]...)
		}
	}
	for _, requiredContainerPort := range required {
		match := false
		for _, existingContainerPort := range *existing {
			if existingContainerPort.Name == requiredContainerPort.Name {
				match = true
				break
			}
		}
		if !match {
			*modified = true
			*existing = append(*existing, requiredContainerPort)
		}
	}
}

func ensureContainerPort(modified *bool, existing *corev1.ContainerPort, required corev1.ContainerPort) {
	if !equality.Semantic.DeepEqual(required, *existing) {
		*modified = true
		*existing = required
	}
}

func EnsureServicePorts(modified *bool, existing *[]corev1.ServicePort, required []corev1.ServicePort) {
	for i := len(*existing) - 1; i >= 0; i-- {
		existingServicePort := &(*existing)[i]
		var existingCurr *corev1.ServicePort
		for _, requiredServicePort := range required {
			if existingServicePort.Name == requiredServicePort.Name {
				existingCurr = &(*existing)[i]
				ensureServicePort(modified, existingCurr, requiredServicePort)
				break
			}
		}
		if existingCurr == nil {
			*modified = true
			*existing = append((*existing)[:i], (*existing)[i+1:]...)
		}
	}

	for _, requiredServicePort := range required {
		match := false
		for _, existingServicePort := range *existing {
			if existingServicePort.Name == requiredServicePort.Name {
				match = true
				break
			}
		}
		if !match {
			*modified = true
			*existing = append(*existing, requiredServicePort)
		}
	}
}

func ensureServicePort(modified *bool, existing *corev1.ServicePort, required corev1.ServicePort) {
	ensureServicePortDefaults(&required)
	if !equality.Semantic.DeepEqual(required, *existing) {
		*modified = true
		*existing = required
	}
}

func ensureServicePortDefaults(servicePort *corev1.ServicePort) {
	if servicePort.Protocol == "" {
		servicePort.Protocol = corev1.ProtocolTCP
	}
}

func ensureVolumeMount(modified *bool, existing *corev1.VolumeMount, required corev1.VolumeMount) {
	if !equality.Semantic.DeepEqual(required, *existing) {
		*modified = true
		*existing = required
	}
}

func ensureVolume(modified *bool, existing *corev1.Volume, required corev1.Volume) {
	if !equality.Semantic.DeepEqual(required, *existing) {
		*modified = true
		*existing = required
	}
}

func ensureSecurityContextPtr(modified *bool, existing **corev1.SecurityContext, required *corev1.SecurityContext) {
	// if we have no required, then we don't care what someone else has set
	if required == nil {
		return
	}

	if *existing == nil {
		*modified = true
		*existing = required
		return
	}
	ensureSecurityContext(modified, *existing, *required)
}

func ensureSecurityContext(modified *bool, existing *corev1.SecurityContext, required corev1.SecurityContext) {
	ensureCapabilitiesPtr(modified, &existing.Capabilities, required.Capabilities)
	ensureSELinuxOptionsPtr(modified, &existing.SELinuxOptions, required.SELinuxOptions)
	setBoolPtr(modified, &existing.Privileged, required.Privileged)
	setInt64Ptr(modified, &existing.RunAsUser, required.RunAsUser)
	setBoolPtr(modified, &existing.RunAsNonRoot, required.RunAsNonRoot)
	setBoolPtr(modified, &existing.ReadOnlyRootFilesystem, required.ReadOnlyRootFilesystem)
	setBoolPtr(modified, &existing.AllowPrivilegeEscalation, required.AllowPrivilegeEscalation)
}

func ensureCapabilitiesPtr(modified *bool, existing **corev1.Capabilities, required *corev1.Capabilities) {
	// if we have no required, then we don't care what someone else has set
	if required == nil {
		return
	}

	if *existing == nil {
		*modified = true
		*existing = required
		return
	}
	ensureCapabilities(modified, *existing, *required)
}

func ensureCapabilities(modified *bool, existing *corev1.Capabilities, required corev1.Capabilities) {
	// any Add we specify, we require.
	for _, required := range required.Add {
		found := false
		for _, curr := range existing.Add {
			if equality.Semantic.DeepEqual(curr, required) {
				found = true
				break
			}
		}
		if !found {
			*modified = true
			existing.Add = append(existing.Add, required)
		}
	}

	// any Drop we specify, we require.
	for _, required := range required.Drop {
		found := false
		for _, curr := range existing.Drop {
			if equality.Semantic.DeepEqual(curr, required) {
				found = true
				break
			}
		}
		if !found {
			*modified = true
			existing.Drop = append(existing.Drop, required)
		}
	}
}

func setStringSliceIfSet(modified *bool, existing *[]string, required []string) {
	if required == nil {
		return
	}
	if !equality.Semantic.DeepEqual(required, *existing) {
		*existing = required
		*modified = true
	}
}

func setStringSlice(modified *bool, existing *[]string, required []string) {
	if !reflect.DeepEqual(required, *existing) {
		*existing = required
		*modified = true
	}
}

func mergeStringSlice(modified *bool, existing *[]string, required []string) {
	for _, required := range required {
		found := false
		for _, curr := range *existing {
			if required == curr {
				found = true
				break
			}
		}
		if !found {
			*modified = true
			*existing = append(*existing, required)
		}
	}
}

func ensureTolerations(modified *bool, existing *[]corev1.Toleration, required []corev1.Toleration) {
	for ridx := range required {
		found := false
		for eidx := range *existing {
			if required[ridx].Key == (*existing)[eidx].Key {
				found = true
				if !equality.Semantic.DeepEqual((*existing)[eidx], required[ridx]) {
					*modified = true
					(*existing)[eidx] = required[ridx]
				}
				break
			}
		}
		if !found {
			*modified = true
			*existing = append(*existing, required[ridx])
		}
	}
}

func ensureAffinityPtr(modified *bool, existing **corev1.Affinity, required *corev1.Affinity) {
	// if we have no required, then we don't care what someone else has set
	if required == nil {
		return
	}

	if *existing == nil {
		*modified = true
		*existing = required
		return
	}
	ensureAffinity(modified, *existing, *required)
}

func ensureAffinity(modified *bool, existing *corev1.Affinity, required corev1.Affinity) {
	if !equality.Semantic.DeepEqual(existing.NodeAffinity, required.NodeAffinity) {
		*modified = true
		(*existing).NodeAffinity = required.NodeAffinity
	}
	if !equality.Semantic.DeepEqual(existing.PodAffinity, required.PodAffinity) {
		*modified = true
		(*existing).PodAffinity = required.PodAffinity
	}
	if !equality.Semantic.DeepEqual(existing.PodAntiAffinity, required.PodAntiAffinity) {
		*modified = true
		(*existing).PodAntiAffinity = required.PodAntiAffinity
	}
}

func ensurePodSecurityContextPtr(modified *bool, existing **corev1.PodSecurityContext, required *corev1.PodSecurityContext) {
	// if we have no required, then we don't care what someone else has set
	if required == nil {
		return
	}

	if *existing == nil {
		*modified = true
		*existing = required
		return
	}
	ensurePodSecurityContext(modified, *existing, *required)
}

func ensurePodSecurityContext(modified *bool, existing *corev1.PodSecurityContext, required corev1.PodSecurityContext) {
	ensureSELinuxOptionsPtr(modified, &existing.SELinuxOptions, required.SELinuxOptions)
	setInt64Ptr(modified, &existing.RunAsUser, required.RunAsUser)
	setInt64Ptr(modified, &existing.RunAsGroup, required.RunAsGroup)
	setBoolPtr(modified, &existing.RunAsNonRoot, required.RunAsNonRoot)

	// any SupplementalGroups we specify, we require.
	for _, required := range required.SupplementalGroups {
		found := false
		for _, curr := range existing.SupplementalGroups {
			if curr == required {
				found = true
				break
			}
		}
		if !found {
			*modified = true
			existing.SupplementalGroups = append(existing.SupplementalGroups, required)
		}
	}

	setInt64Ptr(modified, &existing.FSGroup, required.FSGroup)

	// any SupplementalGroups we specify, we require.
	for _, required := range required.Sysctls {
		found := false
		for j, curr := range existing.Sysctls {
			if curr.Name == required.Name {
				found = true
				if curr.Value != required.Value {
					*modified = true
					existing.Sysctls[j] = required
				}
				break
			}
		}
		if !found {
			*modified = true
			existing.Sysctls = append(existing.Sysctls, required)
		}
	}
}

func ensureSELinuxOptionsPtr(modified *bool, existing **corev1.SELinuxOptions, required *corev1.SELinuxOptions) {
	// if we have no required, then we don't care what someone else has set
	if required == nil {
		return
	}

	if *existing == nil {
		*modified = true
		*existing = required
		return
	}
	ensureSELinuxOptions(modified, *existing, *required)
}

func ensureSELinuxOptions(modified *bool, existing *corev1.SELinuxOptions, required corev1.SELinuxOptions) {
	setStringIfSet(modified, &existing.User, required.User)
	setStringIfSet(modified, &existing.Role, required.Role)
	setStringIfSet(modified, &existing.Type, required.Type)
	setStringIfSet(modified, &existing.Level, required.Level)
}

func ensureResourceRequirements(modified *bool, existing *corev1.ResourceRequirements, required corev1.ResourceRequirements) {
	ensureResourceList(modified, &existing.Limits, &required.Limits)
	ensureResourceList(modified, &existing.Requests, &required.Requests)
}

func ensureResourceList(modified *bool, existing *corev1.ResourceList, required *corev1.ResourceList) {
	if !equality.Semantic.DeepEqual(existing, required) {
		*modified = true
		required.DeepCopyInto(existing)
	}
}

func ensureDNSPolicy(modified *bool, existing *corev1.DNSPolicy, required corev1.DNSPolicy) {
	if !equality.Semantic.DeepEqual(required, *existing) {
		*modified = true
		*existing = required
	}
}

func setBool(modified *bool, existing *bool, required bool) {
	if required != *existing {
		*existing = required
		*modified = true
	}
}

func setBoolPtr(modified *bool, existing **bool, required *bool) {
	// if we have no required, then we don't care what someone else has set
	if required == nil {
		return
	}

	if *existing == nil {
		*modified = true
		*existing = required
		return
	}
	setBool(modified, *existing, *required)
}

func setInt32(modified *bool, existing *int32, required int32) {
	if required != *existing {
		*existing = required
		*modified = true
	}
}

func setInt32Ptr(modified *bool, existing **int32, required *int32) {
	if *existing == nil && required == nil {
		return
	}
	if *existing == nil || (required == nil && *existing != nil) {
		*modified = true
		*existing = required
		return
	}
	setInt32(modified, *existing, *required)
}

func setInt64(modified *bool, existing *int64, required int64) {
	if required != *existing {
		*existing = required
		*modified = true
	}
}

func setInt64Ptr(modified *bool, existing **int64, required *int64) {
	// if we have no required, then we don't care what someone else has set
	if required == nil {
		return
	}

	if *existing == nil {
		*modified = true
		*existing = required
		return
	}
	setInt64(modified, *existing, *required)
}
