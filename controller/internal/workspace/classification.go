package workspace

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

type FailureClass string

const (
	FailureClassNone           FailureClass = ""
	FailureClassInfrastructure FailureClass = "Infrastructure"
	FailureClassResource       FailureClass = "Resource"
	FailureClassProcess        FailureClass = "Process"
	FailureClassConfiguration  FailureClass = "Configuration"
)

type PodObservation struct {
	Exists             bool
	Phase              corev1.PodPhase
	Reason             string
	Message            string
	ContainerReason    string
	ContainerExitCode  int32
	ContainerOOMKilled bool
	CrashLoop          bool
	Unschedulable      bool
	Scheduled          bool
}

func classifyFailure(obs PodObservation) FailureClass {
	if !obs.Exists {
		return FailureClassInfrastructure
	}
	if obs.ContainerOOMKilled {
		return FailureClassResource
	}
	// Ephemeral-storage eviction → Resource (must check before general Evicted)
	if obs.Reason == "Evicted" && strings.Contains(obs.Message, "ephemeral") {
		return FailureClassResource
	}
	switch obs.Reason {
	case "Evicted", "Preempting", "NodeShutdown", "NodeAffinity",
		"Terminated", "DeadlineExceeded", "GracefulNodeShutdown",
		"DisruptionTarget":
		return FailureClassInfrastructure
	}
	if obs.Unschedulable {
		return FailureClassInfrastructure
	}
	switch obs.ContainerReason {
	case "ImagePullBackOff", "ErrImagePull", "InvalidImageName",
		"ErrImageNeverPull", "CreateContainerConfigError",
		"CreateContainerError":
		return FailureClassConfiguration
	}
	switch obs.ContainerReason {
	case "CrashLoopBackOff", "RunContainerError", "StartError",
		"ContainerCannotRun", "PostStartHookError", "BackOff":
		return FailureClassProcess
	}
	if obs.CrashLoop {
		return FailureClassProcess
	}
	return FailureClassProcess
}

func observePod(pod *corev1.Pod) PodObservation {
	if pod == nil {
		return PodObservation{Exists: false}
	}
	obs := PodObservation{
		Exists:  true,
		Phase:   pod.Status.Phase,
		Reason:  pod.Status.Reason,
		Message: pod.Status.Message,
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled {
			if cond.Status == corev1.ConditionTrue {
				obs.Scheduled = true
			} else if cond.Reason == "Unschedulable" {
				obs.Unschedulable = true
			}
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			if obs.ContainerReason == "" {
				obs.ContainerReason = cs.State.Waiting.Reason
			}
			if cs.State.Waiting.Reason == "CrashLoopBackOff" {
				obs.CrashLoop = true
			}
		}
		if cs.State.Terminated != nil {
			if obs.ContainerReason == "" {
				obs.ContainerReason = cs.State.Terminated.Reason
			}
			obs.ContainerExitCode = cs.State.Terminated.ExitCode
			if cs.State.Terminated.Reason == "OOMKilled" {
				obs.ContainerOOMKilled = true
			}
		}
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Waiting != nil && obs.ContainerReason == "" {
			obs.ContainerReason = cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil {
			if cs.State.Terminated.Reason == "OOMKilled" {
				obs.ContainerOOMKilled = true
			}
			if obs.ContainerReason == "" {
				obs.ContainerReason = cs.State.Terminated.Reason
			}
		}
	}
	return obs
}

// containerHasEverStarted reports whether a single container status
// represents a container that the kubelet has ever launched. A container
// that has ever run has non-nil State.Running or State.Terminated, or a
// non-nil LastTerminationState (it ran, crashed, and is now Waiting). A
// container that the kubelet has never reached — typically because volume
// mounting failed — has only State.Waiting and nil LastTerminationState.
func containerHasEverStarted(cs corev1.ContainerStatus) bool {
	if cs.State.Running != nil || cs.State.Terminated != nil {
		return true
	}
	if cs.LastTerminationState.Running != nil ||
		cs.LastTerminationState.Terminated != nil {
		return true
	}
	return false
}

// noContainerHasEverStarted reports whether every init and main container
// on the pod has never been launched by the kubelet. This is the reliable
// signal that kubelet cannot make progress on the pod — the canonical
// case is a FailedMount event where volume mounting blocks all container
// creation. The pod still has non-zero container status entries (each in
// pure Waiting state), which is why a naive "len(status)==0" check misses
// it. See issue #699.
func noContainerHasEverStarted(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for i := range pod.Status.InitContainerStatuses {
		if containerHasEverStarted(pod.Status.InitContainerStatuses[i]) {
			return false
		}
	}
	for i := range pod.Status.ContainerStatuses {
		if containerHasEverStarted(pod.Status.ContainerStatuses[i]) {
			return false
		}
	}
	return true
}
