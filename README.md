# redisping
Redis network diagnosis tool

## What it does
- Send Redis `PING`/`PONG` to measure RTT between nodes (works well for Redis Cluster pods)
- Text-first output with per-try RTT and summary (min/avg/max, p50/p90/p99, loss)
- Single static Go binary; no runtime deps; works outside the cluster
- Supports password-authenticated Redis servers
- Can push itself into a Kubernetes pod (via `kubectl exec`) to test pod-to-pod RTT without pre-deploying an image

## Build
```bash
go build -o redisping
```

## Usage
```bash
redisping --addr 10.0.0.5:6379 --count 50 --timeout 500ms --interval 100ms --password secret
```

Flags:
- `--addr` target host:port (positional argument also accepted)
- `--count` number of ping attempts (default 20)
- `--timeout` per-request timeout (default 1s)
- `--interval` delay between requests (default 100ms)
- `--password` Redis AUTH password (optional)

### Run inside a pod (no pre-deploy)
Requires a working `kubectl` context. The CLI will stream its own binary into the target pod and execute it there.

```bash
# From your laptop to run inside pod redis-0 (default ns)
redisping attach --pod redis-0 --addr redis-1.redis:6379 --count 30

# Namespaced pod and password
redisping attach --pod cache/redis-0 --addr redis-1.redis:6379 --password secret

# Specify container when pod has multiple
redisping attach --pod cache/redis-0 --container redis --addr redis-1.redis:6379

# Custom kubectl path
redisping attach --pod redis-0 --addr redis-1.redis:6379 --kubectl /usr/local/bin/kubectl
```

Notes:
- Progress line now updates in-place and shows source pod/IP and target.
- Ctrl+C prints a partial summary of collected samples before exit.

Example for multiple nodes (run per target):
```bash
redisping 10.0.0.11:6379
redisping 10.0.0.12:6379
```
