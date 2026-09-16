#!/usr/bin/env bash
# release-preflight.sh — issue #1237's pre-flight for a candidate config
# bump (the ops-repo HelmRelease values that deploy this chart). Run it
# against the candidate values file BEFORE merging any coordinated bump:
#
#   ./local/release-preflight.sh path/to/candidate-values.yaml
#
# Four checks (one per incident class):
#   P1  every image tag in the candidate RESOLVES in the registry
#       (manifest HEAD 200) — incident 2026-09-02 (base:0.26.0 never
#       existed; fleet-wide ImagePullBackOff)
#   P2  every digest-pinned delivery ref belongs to the NAMED image's
#       index, and no delivery pin reuses a platform component digest —
#       incident 2026-09-01 (agentd lines refreshed with a controller
#       digest). Optional per-arch binarySHA256* pins are verified
#       against the index's CI-stamped annotations.
#   P3  runtimeEnvironments.base.image.tag is CalVer YYYY.MM.x and
#       equals the catalog seed default row — the single source
#       (design 0053 D5/S4). Drift is a red light.
#   P4  the opencode coordinate matches the repo's platform-validated
#       pin (runtimes/opencode/Dockerfile ARG OPENCODE_VERSION); a
#       candidate shipping an unvalidated opencode is the 2026-08-29
#       incident class. With --opencode-bin <path> the behavioral
#       contract script (local/opencode-binary-contract.sh) is executed
#       against that binary as part of the pre-flight.
#
# Network checks (P1, the online half of P2) hit the registry API
# ($GHCR_API, default https://ghcr.io) and REQUIRE reachability — a
# pre-flight that silently skips them is not a pre-flight. --offline
# explicitly degrades to the offline-only checks with a loud warning
# (air-gapped use; never for a merge gate).
#
# Exit: 0 = all requested checks green; 1 = at least one red light.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${LSS_REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"
GHCR_API="${GHCR_API:-https://ghcr.io}"
SEED_FILE="${REPO_ROOT}/api/internal/imagefactory/catalog.seed.yaml"
OPENCODE_DOCKERFILE="${REPO_ROOT}/runtimes/opencode/Dockerfile"
CONTRACT_SCRIPT="${SCRIPT_DIR}/opencode-binary-contract.sh"

OFFLINE=0
OPENCODE_BIN=""
CANDIDATE=""
for arg in "$@"; do
  case "${arg}" in
    --offline)        OFFLINE=1 ;;
    --opencode-bin=*) OPENCODE_BIN="${arg#--opencode-bin=}" ;;
    -h|--help)        sed -n '2,30p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)                if [ -n "${CANDIDATE}" ]; then
                        echo "usage: $0 <candidate-values.yaml> [--offline] [--opencode-bin=<path>]" >&2
                        exit 2
                      fi
                      CANDIDATE="${arg}" ;;
  esac
done
if [ -z "${CANDIDATE}" ] || [ ! -f "${CANDIDATE}" ]; then
  echo "usage: $0 <candidate-values.yaml> [--offline] [--opencode-bin=<path>]" >&2
  [ -n "${CANDIDATE}" ] && echo "preflight: no such file: ${CANDIDATE}" >&2
  exit 2
fi

FAIL=0
ok()  { printf '[preflight] ok: %s\n' "$*"; }
die() { printf '[preflight] FAIL: %s\n' "$*" >&2; FAIL=1; }

# ---- parse the candidate values into full-path keys -----------------------
# Emits "<dotted.path>\t<value>" for every scalar, tracking YAML block
# nesting by indentation (the values files in this ecosystem are
# 2-space indented, comments skipped). Values are de-quoted.
flatten_values() {
  awk '
    function strip(v) { sub(/^[ \t]+/, "", v); sub(/[ \t]+$/, "", v);
      if (length(v) >= 2 && (substr(v,1,1) == "\"" && substr(v,length(v),1) == "\"")) v = substr(v,2,length(v)-2);
      return v }
    {
      raw = $0; sub(/\r$/, "", raw)
      if (raw ~ /^[ \t]*$/) next
      if (raw ~ /^[ \t]*#/) next
      match(raw, /^ */); ind = RLENGTH
      body = substr(raw, ind+1)
      if (body ~ /:[ ]*$/) { k = body; sub(/:[ ]*$/, "", k)
        while (n > 0 && ind_[n] >= ind) n--
        n++; key_[n] = k; ind_[n] = ind; next }
      c = index(body, ":")
      if (c < 2) next
      k = substr(body, 1, c-1); v = strip(substr(body, c+1))
      path = ""
      for (i = 1; i <= n; i++) path = path key_[i] "."
      print path k "\t" v
    }' "$1"
}

declare -A VALS
while IFS=$'\t' read -r k v; do
  VALS["${k}"]="${v}"
done < <(flatten_values "${CANDIDATE}")

val() { printf '%s' "${VALS["$1"]-}"; }

# repo/tag decomposition for digest-first delivery refs: strip the
# @digest, then split an optional ":tag" off the remainder (the repo
# hostname itself contains no colon).
delivery_repo() { local r="${1%%@*}"; printf '%s' "${r%:*}"; }
delivery_tag() { local r="${1%%@*}"; case "${r}" in *:*) printf '%s' "${r##*:}" ;; *) printf '' ;; esac; }

# ---- P3 (offline): base tag == catalog seed default row, CalVer ----------
SEED_NAME="$(awk '/^bases:/{inb=1;next} inb && /^  - name: /{sub(/^  - name: /,"");sub(/[ \t]*$/,"");print;exit}' "${SEED_FILE}")"
SEED_VERSION="$(awk '/^bases:/{inb=1;next} inb && /^  - name: /{inrow=1;next} inrow && /^    version:/{gsub(/"/,"",$2);print $2;exit}' "${SEED_FILE}")"
BASE_TAG="$(val runtimeEnvironments.base.image.tag)"
BASE_REPO="$(val runtimeEnvironments.base.image.repository)"
BASE_DIGEST="$(val runtimeEnvironments.base.image.digest)"

if [ -n "${BASE_DIGEST}" ]; then
  ok "P3 base is digest-pinned (${BASE_REPO}@${BASE_DIGEST}) — CalVer check skipped (digest pin wins)"
elif [ -z "${BASE_TAG}" ]; then
  die "P3 runtimeEnvironments.base.image.tag is empty — an empty tag fails the chart render (design 0053 D5/S4); set the catalog seed row's version"
elif [[ ! "${BASE_TAG}" =~ ^[0-9]{4}\.(0[1-9]|1[0-2])\.[0-9]+$ ]]; then
  die "P3 base tag '${BASE_TAG}' is not CalVer YYYY.MM.x — the base is off the platform train (incident 2026-09-02: base:0.26.0 never existed)"
elif [ "${BASE_TAG}" != "${SEED_VERSION}" ]; then
  die "P3 base tag drift: candidate says '${BASE_TAG}', catalog seed default row (${SEED_NAME}) says '${SEED_VERSION}' — the seed row is the single source"
else
  ok "P3 base tag '${BASE_TAG}' matches the catalog seed default row (${SEED_NAME})"
fi

# ---- P2 (offline half): no delivery pin equals a platform digest ----------
AGENTD_REF="$(val controller.agentdDelivery.image)"
OPENCODE_REF="$(val controller.opencodeDelivery.image)"
CTRL_DIGEST="$(val controller.image.digest)"
API_DIGEST="$(val api.image.digest)"
FE_DIGEST="$(val frontend.image.digest)"
ROUTER_DIGEST="$(val controller.inferenceRelay.router.image.digest)"

# Both delivery refs are mandatory pins (design 0053 §4.5 — the chart
# render fails on an empty one): a candidate missing them cannot deploy,
# and skipping the ref here would be a silent PASS on an undeployable
# candidate. opencode's empty-ref die lives in P4 below; agentd's is here.
[ -z "${AGENTD_REF}" ] && die "P2 controller.agentdDelivery.image is empty — the delivery pin is mandatory (design 0053 §4.5)"

ref_digest() { case "$1" in *@sha256:*) printf '%s' "${1##*@sha256:}" ;; *) printf '' ;; esac; }

PLATFORM_DIGESTS=()
[ -n "${CTRL_DIGEST}" ]   && PLATFORM_DIGESTS+=("${CTRL_DIGEST#sha256:}")
[ -n "${API_DIGEST}" ]    && PLATFORM_DIGESTS+=("${API_DIGEST#sha256:}")
[ -n "${FE_DIGEST}" ]     && PLATFORM_DIGESTS+=("${FE_DIGEST#sha256:}")
[ -n "${ROUTER_DIGEST}" ] && PLATFORM_DIGESTS+=("${ROUTER_DIGEST#sha256:}")

DELIVERY_PINS=(
  "controller.agentdDelivery.image:${AGENTD_REF}"
  "controller.opencodeDelivery.image:${OPENCODE_REF}"
  "controller.agentdDelivery.binarySHA256Amd64:$(val controller.agentdDelivery.binarySHA256Amd64)"
  "controller.agentdDelivery.binarySHA256Arm64:$(val controller.agentdDelivery.binarySHA256Arm64)"
  "controller.opencodeDelivery.binarySHA256Amd64:$(val controller.opencodeDelivery.binarySHA256Amd64)"
  "controller.opencodeDelivery.binarySHA256Arm64:$(val controller.opencodeDelivery.binarySHA256Arm64)"
)
P2_COLLISION=0
for pin in "${DELIVERY_PINS[@]}"; do
  pin_key="${pin%%:*}"; pin_val="${pin#*:}"
  pin_hex="$(ref_digest "${pin_val}")"
  [ -z "${pin_hex}" ] && [ "${pin_val}" = "" ] && continue
  [ -z "${pin_hex}" ] && pin_hex="${pin_val}"
  [ -z "${pin_hex}" ] && continue
  for pd in "${PLATFORM_DIGESTS[@]-}"; do
    [ -z "${pd}" ] && continue
    if [ "${pin_hex}" = "${pd}" ]; then
      die "P2 ${pin_key} reuses a platform component digest (${pin_hex}) — delivery pins come from the merge-agentd/merge-opencode values blocks (incident 2026-09-01)"
      P2_COLLISION=1
    fi
  done
done
AGENTD_HEX="$(ref_digest "${AGENTD_REF}")"
OPENCODE_HEX="$(ref_digest "${OPENCODE_REF}")"
if [ -n "${AGENTD_HEX}" ] && [ "${AGENTD_HEX}" = "${OPENCODE_HEX}" ]; then
  die "P2 agentdDelivery and opencodeDelivery carry the same digest (${AGENTD_HEX}) — separate artifacts by design (design 0053 §5)"
  P2_COLLISION=1
fi
[ "${P2_COLLISION}" -eq 0 ] && ok "P2 offline: no delivery pin collides with a platform component digest"

# ---- P4 (offline): opencode coordinate matches the repo pin ---------------
# `|| true`: a Dockerfile without the ARG (or a missing file) must flow the
# empty value into the die below — under pipefail the bare grep|head|cut
# pipeline would otherwise abort the script before the P4 message prints.
PINNED_OPENCODE="$(grep -oE '^ARG OPENCODE_VERSION=[0-9.]+' "${OPENCODE_DOCKERFILE}" | head -1 | cut -d= -f2 || true)"
OPENCODE_TAG="$(delivery_tag "${OPENCODE_REF}")"
if [ -z "${PINNED_OPENCODE}" ]; then
  die "P4 cannot read the repo opencode pin (${OPENCODE_DOCKERFILE}) — parser drift?"
elif [ -z "${OPENCODE_REF}" ]; then
  die "P4 controller.opencodeDelivery.image is empty — the delivery pin is mandatory (design 0053 §4.5)"
elif [ -n "${OPENCODE_TAG}" ] && [ "${OPENCODE_TAG}" != "${PINNED_OPENCODE}" ]; then
  die "P4 candidate opencode '${OPENCODE_TAG}' != repo-validated pin '${PINNED_OPENCODE}' — the platform-validated coordinate is runtimes/opencode/Dockerfile; shipping an unvalidated opencode is the 2026-08-29 incident class"
elif [ -z "${OPENCODE_TAG}" ]; then
  # Digest-only is the mainline shape (release.yml merge-opencode emits
  # @digest with no tag) and the index annotations carry the PLATFORM
  # version, not the opencode version — so no ref↔pin comparison is
  # possible here. Report exactly what is and is not verified; the
  # version↔digest binding is procedural: paste the digest from the
  # merge-opencode values block of the release that built this pin.
  ok "P4 opencode ref is digest-only — the ref carries no upstream version to compare against the repo pin (${PINNED_OPENCODE}); verified mechanically: P1 index membership (+ P2 annotations when pinned); the version binding is procedural: copy the digest from the merge-opencode values block of the release that built pin ${PINNED_OPENCODE} (checklist §2)"
else
  ok "P4 opencode coordinate aligned with the repo pin (${PINNED_OPENCODE}); contract gates: goldens + REFRESH.md + ci fixture-freshness + local/opencode-binary-contract.sh must be green for this pin"
fi
if [ -n "${OPENCODE_BIN}" ]; then
  if [ ! -x "${OPENCODE_BIN}" ] && ! command -v "${OPENCODE_BIN}" >/dev/null 2>&1; then
    die "P4 --opencode-bin '${OPENCODE_BIN}' is not executable"
  else
    if OPENCODE_BIN="${OPENCODE_BIN}" bash "${CONTRACT_SCRIPT}"; then
      ok "P4 behavioral contract script green against ${OPENCODE_BIN}"
    else
      die "P4 behavioral contract script FAILED against ${OPENCODE_BIN} — do NOT bump (local/opencode-binary-contract.sh)"
    fi
  fi
fi

# ---- P1 + P2 (online): registry resolution & index membership -------------
if [ "${OFFLINE}" -eq 1 ]; then
  printf '[preflight] WARNING: --offline — registry checks (P1 resolution, P2 index membership) SKIPPED; never use as a merge gate\n' >&2
  if [ "${FAIL}" -ne 0 ]; then
    printf '[preflight] RESULT: FAIL (offline checks)\n' >&2
    exit 1
  fi
  printf '[preflight] RESULT: PASS (offline checks only)\n'
  exit 0
fi

token_for() { curl -sf -m 10 "${GHCR_API}/token?scope=repository:${1}:pull" | sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'; }

# head_manifest <repo> <tag-or-digest> -> "code digest" on stdout
head_manifest() {
  local repo="$1" ref="$2" tok out code digest
  tok="$(token_for "${repo}")"
  [ -z "${tok}" ] && { echo "000 -"; return; }
  out="$(curl -s -m 15 -o /dev/null -D - \
    -H "Authorization: Bearer ${tok}" \
    -H "Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json" \
    "${GHCR_API}/v2/${repo}/manifests/${ref}")" || { echo "000 -"; return; }
  code="$(printf '%s' "${out}" | awk 'NR==1{print $2}')"
  digest="$(printf '%s' "${out}" | awk 'tolower($1)=="docker-content-digest:"{sub(/\r$/,"",$2);print $2;exit}')"
  echo "${code:-000} ${digest:--}"
}

# index_annotations <repo> <tag-or-digest> <artifact-name> -> "<amd64hex|-> <arm64hex|->"
# Reads the CI-stamped per-arch binary sha256 annotations from the image
# index (release.yml merge jobs stamp dev.llmsafespaces/<name>.sha256-{amd64,arm64};
# the controller resolves UNSET pins from the same annotations at startup).
index_annotations() {
  local repo="$1" ref="$2" name="$3" tok body
  tok="$(token_for "${repo}")"
  [ -z "${tok}" ] && return 1
  body="$(curl -sf -m 15 \
    -H "Authorization: Bearer ${tok}" \
    -H "Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json" \
    "${GHCR_API}/v2/${repo}/manifests/${ref}")" || return 1
  printf '%s' "${body}" | PIN_NAME="${name}" python3 -c '
import json, os, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
a = d.get("annotations") or {}
prefix = "dev.llmsafespaces/" + os.environ["PIN_NAME"] + ".sha256-"
print(a.get(prefix + "amd64") or "-", a.get(prefix + "arm64") or "-")
'
}

strip_registry() { case "$1" in ghcr.io/*) printf '%s' "${1#ghcr.io/}" ;; *) printf '%s' "$1" ;; esac; }

# collect refs to resolve: repository + (tag | digest) per component.
CHECK_REFS=(
  "api.image:$(val api.image.repository):$(val api.image.tag):$(val api.image.digest)"
  "controller.image:$(val controller.image.repository):$(val controller.image.tag):$(val controller.image.digest)"
  "frontend.image:$(val frontend.image.repository):$(val frontend.image.tag):$(val frontend.image.digest)"
  "mcp.image:$(val mcp.image.repository):$(val mcp.image.tag):"
  "base:$(val runtimeEnvironments.base.image.repository):${BASE_TAG}:${BASE_DIGEST}"
  "agentdDelivery:$(delivery_repo "${AGENTD_REF}"):$(delivery_tag "${AGENTD_REF}"):$(ref_digest "${AGENTD_REF}")"
  "opencodeDelivery:$(delivery_repo "${OPENCODE_REF}"):$(delivery_tag "${OPENCODE_REF}"):$(ref_digest "${OPENCODE_REF}")"
)
[ -n "$(val controller.inferenceRelay.router.image.repository)" ] && \
  CHECK_REFS+=("router.image:$(val controller.inferenceRelay.router.image.repository):$(val controller.inferenceRelay.router.image.tag):$(val controller.inferenceRelay.router.image.digest)")

declare -A RESOLVED_DIGEST
for entry in "${CHECK_REFS[@]}"; do
  IFS=':' read -r label repo tag digest <<<"${entry}"
  [ -z "${repo}" ] && continue
  repo="$(strip_registry "${repo}")"
  if [ -n "${digest}" ]; then
    case "${digest}" in sha256:*) ;; *) digest="sha256:${digest}" ;; esac
    read -r code _ <<<"$(head_manifest "${repo}" "${digest}")"
    if [ "${code}" != "200" ]; then
      die "P1 ${label} digest ${repo}@${digest} does not resolve (${code}) — the digest does not belong to this image's index (incident 2026-09-01 class)"
    else
      ok "P1 ${label} ${repo}@${digest} resolves"
      # Record membership so the digest-only coherence branch below can
      # report "verified by P1" ONLY when P1 actually resolved it.
      RESOLVED_DIGEST["${label}"]="${digest}"
    fi
    if [ -n "${tag}" ]; then
      read -r tcode idx <<<"$(head_manifest "${repo}" "${tag}")"
      if [ "${tcode}" != "200" ]; then
        die "P1 ${label} tag ${repo}:${tag} does not resolve (${tcode}) — the registry does not have this tag (incident 2026-09-02 class)"
      elif [ "${idx}" = "-" ] || [ -z "${idx}" ]; then
        RESOLVED_DIGEST["${label}"]="${idx}"
        # tag resolves but the registry returned no index digest header —
        # nothing to compare the pin against; never claim an unverified
        # match (verify_pin_coherence skips on the same condition).
      elif [ "${digest}" != "${idx}" ]; then
        die "P2 ${label} pin ${digest} is not the current index digest of ${repo}:${tag} (${idx}) — stale or foreign digest"
      else
        RESOLVED_DIGEST["${label}"]="${idx}"
        ok "P2 ${label} pin matches the live index digest of ${repo}:${tag}"
      fi
    fi
  elif [ -n "${tag}" ]; then
    read -r code idx <<<"$(head_manifest "${repo}" "${tag}")"
    if [ "${code}" != "200" ]; then
      die "P1 ${label} tag ${repo}:${tag} does not resolve (${code}) — the registry does not have this tag (incident 2026-09-02 class)"
    else
      RESOLVED_DIGEST["${label}"]="${idx}"
      ok "P1 ${label} ${repo}:${tag} resolves (index ${idx})"
    fi
  else
    die "P1 ${label} has neither tag nor digest — nothing to resolve"
  fi
done

# P2 online: an explicit delivery digest must be the CURRENT index digest
# of the named repo when a tag is also present (tag+digest coherence).
verify_pin_coherence() {
  local label="$1" ref="$2" repo="$3" tag="$4" hex="$5"
  # An absent ref is a mandatory-pin failure the P2/P4 offline dies already
  # reported — never paper over it with an ok here.
  [ -z "${ref}" ] && return 0
  [ -z "${hex}" ] && { ok "P2 ${label} carries no explicit digest — index annotations resolve at controller startup"; return; }
  if [ -z "${tag}" ]; then
    # digest-only pin: membership was proven only if P1 actually resolved
    # the digest — never claim "verified by P1" right after P1 refuted it.
    if [ "${RESOLVED_DIGEST["${label}"]-}" = "sha256:${hex}" ]; then
      ok "P2 ${label} digest-only pin (${repo}@${hex}) — membership verified by P1 resolution"
    fi
    return 0
  fi
  # tag+digest pin: the P1 loop above already compared this exact pair
  # against the live index and printed its verdict (the match ok, the
  # stale/foreign die, or the tag-resolution failure) — repeating it here
  # would double-report every coherent pin.
  return 0
}
verify_pin_coherence "agentdDelivery" "${AGENTD_REF}" "$(delivery_repo "${AGENTD_REF}")" "$(delivery_tag "${AGENTD_REF}")" "${AGENTD_HEX}"
verify_pin_coherence "opencodeDelivery" "${OPENCODE_REF}" "$(delivery_repo "${OPENCODE_REF}")" "$(delivery_tag "${OPENCODE_REF}")" "${OPENCODE_HEX}"

# verify_binary_pins <label> <repo> <ref> <artifact-name> <amd64-pin> <arm64-pin>
# Break-glass binarySHA256* pins are overrides the controller uses as-is
# (it resolves only UNSET pins from the annotations) — so a stale or
# foreign paste would reach every pod. The CI-stamped index annotations
# are the ground truth: a pin that disagrees fails, and a SET pin with
# no annotation on its arch fails too (skip = green is the wrong
# direction — the incident-2026-09-01 class). An index with NO
# annotations at all is the documented break-glass posture for
# un-annotated images (values.yaml caveat) — noted, not failed.
verify_binary_pins() {
  local label="$1" repo="$2" ref="$3" name="$4" amd64="$5" arm64="$6" out idx_amd64 idx_arm64 bad
  # An absent ref is a mandatory-pin failure the P2/P4 offline dies already
  # reported — the annotations line below would be false for an empty ref.
  [ -z "${ref}" ] && return 0
  [ -z "${amd64}" ] && [ -z "${arm64}" ] && { ok "P2 ${label}: no explicit binarySHA256* pins — resolved from the index annotations at controller startup"; return; }
  if ! out="$(index_annotations "${repo}" "${ref}" "${name}")"; then
    die "P2 ${label}: cannot read index annotations from ${repo}@${ref} — cannot verify the break-glass binary pins"
    return
  fi
  idx_amd64="$(printf '%s' "${out}" | awk '{print $1}')"
  idx_arm64="$(printf '%s' "${out}" | awk '{print $2}')"
  if [ "${idx_amd64}" = "-" ] && [ "${idx_arm64}" = "-" ]; then
    ok "P2 ${label}: index carries no annotations — break-glass posture (values.yaml caveat); pins used as-is by the controller"
    return
  fi
  bad=0
  local verified=0
  if [ -n "${amd64}" ]; then
    if [ "${idx_amd64}" = "-" ]; then
      die "P2 ${label}.binarySHA256Amd64 is set but the index carries no amd64 annotation — cannot verify, and the controller uses explicit pins as-is (fail-loud: remove the pin or annotate the index)"
      bad=1
    elif [ "${amd64}" != "${idx_amd64}" ]; then
      die "P2 ${label}.binarySHA256Amd64 (${amd64}) disagrees with the CI-stamped index annotation (${idx_amd64}) — stale or foreign per-arch pin (incident 2026-09-01 class)"
      bad=1
    else
      verified=$((verified+1))
    fi
  fi
  if [ -n "${arm64}" ]; then
    if [ "${idx_arm64}" = "-" ]; then
      die "P2 ${label}.binarySHA256Arm64 is set but the index carries no arm64 annotation — cannot verify, and the controller uses explicit pins as-is (fail-loud: remove the pin or annotate the index)"
      bad=1
    elif [ "${arm64}" != "${idx_arm64}" ]; then
      die "P2 ${label}.binarySHA256Arm64 (${arm64}) disagrees with the CI-stamped index annotation (${idx_arm64}) — stale or foreign per-arch pin (incident 2026-09-01 class)"
      bad=1
    else
      verified=$((verified+1))
    fi
  fi
  if [ "${bad}" -eq 0 ]; then
    ok "P2 ${label}: explicit binarySHA256* pins match the CI-stamped index annotations (${verified} verified)"
  fi
  return 0
}

delivery_ref() { # the manifest ref to inspect: digest when pinned, else tag
  local ref="$1" hex; hex="$(ref_digest "${ref}")"
  if [ -n "${hex}" ]; then printf 'sha256:%s' "${hex}"; else delivery_tag "${ref}"; fi
}
verify_binary_pins "agentdDelivery" "$(strip_registry "$(delivery_repo "${AGENTD_REF}")")" "$(delivery_ref "${AGENTD_REF}")" "agentd" \
  "$(val controller.agentdDelivery.binarySHA256Amd64)" "$(val controller.agentdDelivery.binarySHA256Arm64)"
verify_binary_pins "opencodeDelivery" "$(strip_registry "$(delivery_repo "${OPENCODE_REF}")")" "$(delivery_ref "${OPENCODE_REF}")" "opencode" \
  "$(val controller.opencodeDelivery.binarySHA256Amd64)" "$(val controller.opencodeDelivery.binarySHA256Arm64)"

if [ "${FAIL}" -ne 0 ]; then
  printf '[preflight] RESULT: FAIL — do NOT merge; see failures above\n' >&2
  exit 1
fi
printf '[preflight] RESULT: PASS — candidate coordinates all resolve and align\n'
