#!/usr/bin/env bash
# update-deepcopy.sh — regenerate pkg/apis deepcopy via controller-gen.
#
# The package uses kubebuilder's `object:generate` annotation and its
# committed zz_generated.deepcopy.go files are controller-gen output;
# the previous code-generator (deepcopy-gen) invocation was a silent
# no-op for them — it greps +k8s:deepcopy-gen= markers this package
# never carried (worklog 0870 finding), so a stale generated file could
# not be detected or refreshed by `make deepcopy`.
set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

go run sigs.k8s.io/controller-tools/cmd/controller-gen \
    object:headerFile="${SCRIPT_ROOT}/hack/boilerplate.deepcopy.go.txt" \
    paths="./pkg/apis/..."
