# schematic: base=bookworm@2026.09.0
FROM ghcr.io/lenaxia/llmsafespaces/base:2026.09.0
USER root

RUN apt-get update && apt-get install -y --no-install-recommends \
    podman uidmap podman-compose podman-docker \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p "/etc/containers" && printf %s "W2NvbnRhaW5lcnNdCm5ldG5zID0gImhvc3QiCg==" | base64 -d > "/etc/containers/containers.conf" && chmod 0644 "/etc/containers/containers.conf"
RUN mkdir -p "/etc/containers" && printf %s "W3N0b3JhZ2VdCmRyaXZlciA9ICJ2ZnMiCnJ1bnJvb3QgPSAiL3NhbmRib3gtcnVudGltZS9jb250YWluZXJzL3J1biIKZ3JhcGhyb290ID0gIi9ob21lL3NhbmRib3gvLmxvY2FsL3NoYXJlL2NvbnRhaW5lcnMvc3RvcmFnZSIK" | base64 -d > "/etc/containers/storage.conf" && chmod 0644 "/etc/containers/storage.conf"
RUN mkdir -p "/etc/profile.d" && printf %s "ZXhwb3J0IFhER19SVU5USU1FX0RJUj0iJHtYREdfUlVOVElNRV9ESVI6LS9zYW5kYm94LXJ1bnRpbWUvcnVufSIKbWtkaXIgLXAgIiRYREdfUlVOVElNRV9ESVIiIDI+L2Rldi9udWxsIHx8IHRydWUK" | base64 -d > "/etc/profile.d/podman.sh" && chmod 0644 "/etc/profile.d/podman.sh"
RUN mkdir -p "/etc" && printf %s "c2FuZGJveDoxMDAwMDA6NjU1MzYK" | base64 -d > "/etc/subgid" && chmod 0644 "/etc/subgid"
RUN mkdir -p "/etc" && printf %s "c2FuZGJveDoxMDAwMDA6NjU1MzYK" | base64 -d > "/etc/subuid" && chmod 0644 "/etc/subuid"

USER sandbox
WORKDIR /workspace
