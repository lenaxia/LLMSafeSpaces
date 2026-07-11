# Quickstart

Get LLMSafeSpaces running on a local [`kind`](https://kind.sigs.k8s.io/) cluster in ~10 minutes. You'll register a user, create a workspace, bind an LLM credential, and drive an agent session end-to-end.

## Prerequisites

| Tool | Version | Why |
|---|---|---|
| `go` | 1.25+ | Build the API and controller |
| `docker` | any recent | kind runs on it |
| `kind` | 0.20+ | Local Kubernetes cluster |
| `kubectl` | 1.28+ | Talk to the cluster |
| `helm` | 3.12+ | Deploy the chart |
| `curl` + `jq` | any | API requests |

Verify:

```bash
go version          # go1.25+
docker version     # any
kind version       # kind v0.20+
kubectl version --client --short
helm version --short
```

## 1. Clone and bootstrap

```bash
git clone https://github.com/lenaxia/LLMSafeSpaces.git
cd LLMSafeSpaces

# Creates a kind cluster, builds all images, deploys the chart
./local/bootstrap.sh
```

This takes ~5 minutes the first time (image builds dominate). When it finishes you should see:

```bash
$ kubectl get pods -n llmsafespaces
NAME                                READY   STATUS    RESTARTS   AGE
llmsafespaces-api-xxx               1/1     Running   0          2m
llmsafespaces-controller-xxx        1/1     Running   0          2m
llmsafespaces-frontend-xxx          1/1     Running   0          2m
postgres-xxx                        1/1     Running   0          2m
redis-xxx                           1/1     Running   0          2m
```

## 2. Port-forward the API

```bash
kubectl port-forward -n llmsafespaces svc/llmsafespaces-api 8080:8080 &
```

The API is now at `http://localhost:8080`.

## 3. Register and authenticate

```bash
API=http://localhost:8080

# Register a user (returns a JWT)
curl -sX POST "$API/api/v1/auth/register" \
  -H "Content-Type: application/json" \
  -d '{"email":"alice@example.com","password":"hunter2hunter2","username":"alice"}'

# Login to get a token you'll reuse
TOKEN=$(curl -sX POST "$API/api/v1/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"email":"alice@example.com","password":"hunter2hunter2"}' \
  | jq -r '.token')

echo "Token: ${TOKEN:0:20}..."
```

## 4. Create a workspace

```bash
WS=$(curl -sX POST "$API/api/v1/workspaces" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-workspace","runtime":"base","storageSize":"1Gi"}' \
  | jq -r '.id')
echo "workspace: $WS"

# Activate it (creates the pod)
curl -sX POST "$API/api/v1/workspaces/$WS/activate" \
  -H "Authorization: Bearer $TOKEN"

# Wait for it to come up
while [ "$(curl -s -H "Authorization: Bearer $TOKEN" \
    "$API/api/v1/workspaces/$WS/status" | jq -r .phase)" != "Active" ]; do
  sleep 2
done
echo "workspace active"
```

## 5. Store an LLM provider credential

The platform doesn't ship API keys — you bring your own. Create a secret holding your LLM gateway credentials and bind it to the workspace:

```bash
SECRET_ID=$(curl -sX POST "$API/api/v1/secrets" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-llm-key",
    "type": "llm-provider",
    "value": "{\"providerID\":\"litellm\",\"apiKey\":\"sk-...\",\"baseURL\":\"https://your-llm-gateway/v1\"}"
  }' | jq -r '.id')

curl -sX PUT "$API/api/v1/workspaces/$WS/bindings" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"secretIds\":[\"$SECRET_ID\"]}"
```

The credential is encrypted client-side with your password-derived DEK, stored in PostgreSQL, and only decrypted into tmpfs at `/sandbox-runtime` when the workspace pod boots. See [Secret Management](../architecture/secrets.md) for the crypto details.

## 6. Drive a session

```bash
# Create a session
SID=$(curl -sX POST "$API/api/v1/workspaces/$WS/sessions/new" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  | jq -r '.sessionId')

# Send a prompt
curl -X POST "$API/api/v1/workspaces/$WS/sessions/$SID/message" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model":   {"providerID":"litellm","modelID":"default"},
    "parts":   [{"type":"text","text":"Reply with exactly the word: PONG"}]
  }' \
  | jq '.parts[] | select(.type=="text") | .text'

# → "PONG"
```

You now have a working agent workspace. The agent has a shell, can write to `/workspace`, and persists its session history in the PVC across suspend/resume cycles.

## 7. Suspend and resume

```bash
# Suspend (deletes pod, keeps PVC)
curl -X POST "$API/api/v1/workspaces/$WS/suspend" \
  -H "Authorization: Bearer $TOKEN"

# Resume (re-creates pod, re-attaches PVC)
curl -X POST "$API/api/v1/workspaces/$WS/activate" \
  -H "Authorization: Bearer $TOKEN"
```

Session history in the PVC survives. The whole cycle takes ~22 seconds.

## Tear down

```bash
./local/teardown.sh
```

Removes the kind cluster and all resources.

## Next

- [Concepts](concepts.md) — the data model in depth
- [Installation](../operator/installation.md) — production deployment
- [REST API](../api/rest.md) — full endpoint reference
