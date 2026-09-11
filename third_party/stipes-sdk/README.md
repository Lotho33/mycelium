# fungobox-sdk

Go SDK for writing [FunGoBox](https://github.com/Lotho33/fungobox) plugins.

FunGoBox plugins are **standalone executables** that communicate with the core over a local gRPC connection using JSON encoding. Any language that can open a TCP socket and speak JSON can be a plugin — Go, Python, Rust, Node.js, etc.

## Installation (Go plugins)

```sh
go get github.com/Lotho33/fungobox-sdk@latest
```

## Plugin types

| Type | What it does |
|------|-------------|
| **Provider** | Supplies content — browsing, search, stream resolution |
| **Enricher** | Post-processes items from providers — metadata, filtering, reordering |

---

## Writing a Provider plugin (Go)

```go
package main

import (
    "context"
    sdk "github.com/Lotho33/fungobox-sdk/sdk"
)

const pluginID = "com.example.myprovider"

type MyProvider struct {
    apiKey string
}

func (p *MyProvider) Init(settings map[string]string) error {
    p.apiKey = settings["api_key"]
    return nil
}

func (p *MyProvider) Setup() (bool, error) {
    return p.apiKey != "", nil
}

func (p *MyProvider) Catalog() ([]sdk.SearchItem, error) {
    return []sdk.SearchItem{
        {ItemID: "movie-1", Category: "movies"},
    }, nil
}

func (p *MyProvider) Search(ctx context.Context, query string, page int) ([]*sdk.MediaItem, error) {
    return []*sdk.MediaItem{
        {ID: "movie-1", Title: "Example Movie", MediaType: "movie"},
    }, nil
}

func (p *MyProvider) Browse(ctx context.Context, dirID string) ([]*sdk.MediaItem, error) {
    return nil, nil
}

func (p *MyProvider) Resolve(playableID string) (sdk.ResolveResponse, error) {
    return sdk.ResolveResponse{StreamURL: "https://example.com/stream.m3u8"}, nil
}

func main() {
    sdk.ServeProvider(pluginID, &MyProvider{})
}
```

## Writing an Enricher plugin (Go)

```go
package main

import sdk "github.com/Lotho33/fungobox-sdk/sdk"

type MyEnricher struct{}

func (e *MyEnricher) Init(settings map[string]string) error  { return nil }
func (e *MyEnricher) Setup() (bool, error)                   { return true, nil }

func (e *MyEnricher) Enrich(items []sdk.MediaItem) ([]sdk.MediaItem, error) {
    for i := range items {
        if items[i].Plot == "" {
            items[i].Plot = "No description available."
        }
    }
    return items, nil
}

func main() {
    sdk.ServeEnricher("com.example.myenricher", &MyEnricher{})
}
```

---

## MediaItem fields

| Field | JSON key | Type | Notes |
|-------|----------|------|-------|
| `ID` | `id` | string | Unique ID within the provider |
| `Title` | `title` | string | Display title |
| `ProviderID` | `provider_id` | string | Set automatically by the core |
| `MediaType` | `mediatype` | string | `"movie"`, `"tvshow"`, `"episode"`, `"music"` … |
| `Category` | `category` | string | Provider-defined category slug |
| `Plot` | `plot` | string | Description / synopsis |
| `Year` | `year` | int | Release year |
| `Rating` | `rating` | float64 | 0–10 |
| `Genres` | `genres` | []string | |
| `Poster` | `poster` | string | URL |
| `Fanart` | `fanart` | string | URL |
| `Metadata` | `metadata` | map[string]string | Arbitrary extra data |
| `IsDir` | `is_dir` | bool | `true` for browsable directories |
| `SeasonNumber` | `season_number` | int | TV only |
| `EpisodeNumber` | `episode_number` | int | TV only |
| `IsExternal` | `is_external` | bool | Stream hosted externally |

---

## Non-Go plugins

Any language is supported. The plugin must:

1. **Check the magic env variable** — exit immediately if `FUNGOBOX_MAGIC` ≠ `fungobox_plugin_v2`.
2. **Bind a TCP listener** on `127.0.0.1:0` (random port).
3. **Write one JSON line to stdout** — the core reads this to discover the port:
   ```json
   {"id":"com.example.myplugin","type":"provider","port":51234,"ready":true}
   ```
4. **Serve gRPC with JSON encoding** on that port. Use the `.proto` file in [`proto/fungobox.proto`](proto/fungobox.proto) as the service contract.

> **JSON codec**: FunGoBox core connects with a custom gRPC codec that serialises messages as JSON (not protobuf binary). Your gRPC server must also accept JSON-encoded frames. With Python `grpcio` this is done by registering a custom codec; see the example below.

### Python example (provider)

```python
#!/usr/bin/env python3
"""Minimal FunGoBox provider plugin in Python."""

import json
import os
import sys
from concurrent import futures

import grpc

# -- JSON codec for grpcio ---------------------------------------------------
import grpc.experimental.gevent  # optional, only if using gevent

class _JsonCodec:
    def encode(self, message):
        return json.dumps(message).encode()

    def decode(self, data, type_):
        return json.loads(data)

# -- Handshake ---------------------------------------------------------------
MAGIC_KEY = "FUNGOBOX_MAGIC"
MAGIC_VAL = "fungobox_plugin_v2"
PLUGIN_ID = "com.example.myplugin"

if os.environ.get(MAGIC_KEY) != MAGIC_VAL:
    sys.stderr.write("[plugin] missing FUNGOBOX_MAGIC — run via FunGoBox core\n")
    sys.exit(1)

# -- gRPC server -------------------------------------------------------------
# (use grpcio-tools + fungobox.proto to generate the stubs, or implement
#  the raw handler dict manually — see grpcio docs for "generic_handlers")

import socket

sock = socket.socket()
sock.bind(("127.0.0.1", 0))
port = sock.getsockname()[1]
sock.close()

announcement = json.dumps({
    "id": PLUGIN_ID,
    "type": "provider",
    "port": port,
    "ready": True,
})
print(announcement, flush=True)

server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
# TODO: add_ProviderServicer_to_server(MyProvider(), server)
server.add_insecure_port(f"127.0.0.1:{port}")
server.start()
server.wait_for_termination()
```

Generate Python stubs from the proto file:
```sh
pip install grpcio-tools
python -m grpc_tools.protoc \
  -I proto \
  --python_out=. \
  --grpc_python_out=. \
  proto/fungobox.proto
```

### Plugin ZIP layout

```
my_plugin/
  manifest.json          ← {"id":"..","type":"provider","name":"..","version":"0.1.0"}
  linux-amd64/
    run.sh               ← entry point launched by the core
    main.py              ← (or compiled binary)
    _venv/               ← pre-built virtualenv (pip install --target _venv/)
  windows-amd64/
    run.bat
    main.py
    _venv/
```

`run.sh`:
```sh
#!/bin/sh
exec "$(dirname "$0")/_venv/bin/python" "$(dirname "$0")/main.py"
```

`run.bat`:
```bat
@echo off
"%~dp0_venv\Scripts\python.exe" "%~dp0main.py"
```

---

## Protocol version

The current protocol version is **`fungobox_plugin_v2`** (exported as `sdk.ProtocolVersion`).  
A plugin compiled against a different protocol version will be refused by the core at startup.

---

## License

MIT — see [LICENSE](LICENSE).
