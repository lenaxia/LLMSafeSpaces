// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

import "time"

// ResourceRequirements defines resource limits for a sandbox
type ResourceRequirements struct {
	// CPU resource limit
	CPU string `json:"cpu,omitempty"`

	// Memory resource limit
	Memory string `json:"memory,omitempty"`

	// GPU resource limit
	GPU string `json:"gpu,omitempty"`
}

// NetworkAccess defines network access configuration
type NetworkAccess struct {
	// Egress rules
	Egress []EgressRule `json:"egress,omitempty"`

	// Allow ingress traffic to sandbox
	Ingress bool `json:"ingress,omitempty"`

	// DevPreview enables the authenticated HTTP/WS tunnel for viewing
	// in-workspace dev servers (Vite, Next, etc.) from a browser (Epic 66).
	DevPreview bool `json:"devPreview,omitempty"`
}

// EgressRule defines an egress rule
type EgressRule struct {
	// Domain name for egress filtering
	Domain string `json:"domain"`

	// Ports allowed for this domain
	Ports []PortRule `json:"ports,omitempty"`
}

// PortRule defines a port rule
type PortRule struct {
	// Port number
	Port int `json:"port"`

	// Protocol (TCP or UDP)
	Protocol string `json:"protocol,omitempty"`
}

// FilesystemConfig defines filesystem configuration
type FilesystemConfig struct {
	// Mount root filesystem as read-only
	ReadOnlyRoot bool `json:"readOnlyRoot,omitempty"`

	// Paths that should be writable
	WritablePaths []string `json:"writablePaths,omitempty"`
}

// StorageConfig defines storage configuration
type StorageConfig struct {
	// Enable persistent storage
	Persistent bool `json:"persistent,omitempty"`

	// Size of persistent volume
	VolumeSize string `json:"volumeSize,omitempty"`
}

// SecurityContext defines security context
type SecurityContext struct {
	// User ID to run container processes
	RunAsUser int64 `json:"runAsUser,omitempty"`

	// Group ID to run container processes
	RunAsGroup int64 `json:"runAsGroup,omitempty"`

	// Seccomp profile
	SeccompProfile string `json:"seccompProfile,omitempty"`

	// AppArmor profile
	AppArmorProfile string `json:"appArmorProfile,omitempty"`

	// Allow privilege escalation
	AllowPrivilegeEscalation bool `json:"allowPrivilegeEscalation,omitempty"`
}

// ProfileReference defines a reference to a RuntimeEnvironment
type ProfileReference struct {
	// Name of RuntimeEnvironment to use
	Name string `json:"name"`

	// Namespace of RuntimeEnvironment
	Namespace string `json:"namespace,omitempty"`
}

// ContainerStateValue represents the state of a container
type ContainerStateValue string

const (
	ContainerStateRunning    ContainerStateValue = "Running"
	ContainerStateTerminated ContainerStateValue = "Terminated"
	ContainerStateWaiting    ContainerStateValue = "Waiting"
	ContainerStateUnknown    ContainerStateValue = "Unknown"
)

// ContainerStatus represents the status of a container
type ContainerStatus struct {
	// Container name
	Name string `json:"name"`

	// Whether the container is ready
	Ready bool `json:"ready"`

	// Number of times the container has been restarted
	RestartCount int32 `json:"restartCount"`

	// Container state
	State ContainerStateValue `json:"state"`

	// Time when the container started
	StartedAt *time.Time `json:"startedAt,omitempty"`

	// Time when the container finished
	FinishedAt *time.Time `json:"finishedAt,omitempty"`

	// Exit code if terminated
	ExitCode int32 `json:"exitCode,omitempty"`

	// Reason for current state
	Reason string `json:"reason,omitempty"`

	// Message regarding current state
	Message string `json:"message,omitempty"`
}

// NetworkInfo represents network information for a sandbox
type NetworkInfo struct {
	// Pod IP address
	PodIP string `json:"podIP,omitempty"`

	// Host IP address
	HostIP string `json:"hostIP,omitempty"`

	// Whether ingress is allowed
	Ingress bool `json:"ingress"`

	// Allowed egress domains
	EgressDomains []string `json:"egressDomains,omitempty"`
}
