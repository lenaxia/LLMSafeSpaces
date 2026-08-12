// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package common

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// SetCondition updates or creates a condition in the provided slice
func SetCondition(conditions *[]metav1.Condition, conditionType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.NewTime(time.Now())
	existingCondition := FindCondition(*conditions, conditionType)

	if existingCondition == nil {
		// Create new condition
		newCondition := metav1.Condition{
			Type:               conditionType,
			Status:             status,
			LastTransitionTime: now,
			Reason:             reason,
			Message:            message,
		}
		*conditions = append(*conditions, newCondition)
		return
	}

	// Update existing condition
	if existingCondition.Status != status {
		existingCondition.LastTransitionTime = now
	}
	existingCondition.Status = status
	existingCondition.Reason = reason
	existingCondition.Message = message
}

// FindCondition finds a condition by type in the provided slice
func FindCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// IsConditionTrue checks if a condition with the given type exists and has status True
func IsConditionTrue(conditions []metav1.Condition, conditionType string) bool {
	condition := FindCondition(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

// AddFinalizer adds a finalizer to an object if it doesn't already exist
func AddFinalizer(obj client.Object, finalizer string) bool {
	if !controllerutil.ContainsFinalizer(obj, finalizer) {
		controllerutil.AddFinalizer(obj, finalizer)
		return true
	}
	return false
}

// RemoveFinalizer removes a finalizer from an object if it exists
func RemoveFinalizer(obj client.Object, finalizer string) bool {
	if controllerutil.ContainsFinalizer(obj, finalizer) {
		controllerutil.RemoveFinalizer(obj, finalizer)
		return true
	}
	return false
}

// IsPodReady checks if a pod is ready
func IsPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func GenerateRandomString(length int) string {
	b := make([]byte, (length+1)/2)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure means the system entropy pool is
		// exhausted or /dev/urandom is unreadable. This is a fatal
		// condition — falling back to a timestamp would produce a
		// predictable credential (the workspace-agent password).
		// Panic rather than silently degrade security.
		panic(fmt.Errorf("crypto/rand.Read failed: %w", err))
	}
	return hex.EncodeToString(b)[:length]
}
