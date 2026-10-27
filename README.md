# LuckyAgent application

`github.com/yurika0211/luckyagent-app` owns the full application's CLI, HTTP and
channel operations, embedding SDK and integration assembly. The launcher and
the public Agent core live in `yurika0211/lucky-agent`; backends and shared
configuration live in `luckyagent-runtime`.

| Package | Responsibility |
| --- | --- |
| `cli/lhcmd` | Cobra command surface, REPL, sessions, gateway startup and runtime management |
| `apiapp`, `api/grpc` | Agent-facing HTTP/gRPC operations and runtime assembly |
| `channelapp/telegram`, `qqofficial`, `websocket` | Product commands, bindings, approvals and run execution |
| `appservice` | Host adapters for the runtime module's neutral application services |
| `bootstrap` | Prepare components, inject the Agent, explicitly start services and transfer ownership |
| `sdk` | Full in-process runtime SDK, including configuration/home initialization |
| `cmd/*-bench`, `swebench`, `examples` | Product benchmarks, evaluation and examples |
| `server`, `websocket`, `gateway/*` | Compatibility bindings for application construction |

This module imports the standalone public Agent module
`github.com/yurika0211/lucky-agent/agent`. That leaf module does not import this
application. The root repository's separate launcher module imports the CLI,
so it can still build `lh` without a module dependency cycle.

Generic HTTP/SSE/WebSocket/gRPC transport remains in `luckyagent-api`. Platform
connections and protocols remain in `luckyagent-gateway`. Those transport
modules do not import the Agent. HTTP routes, public wire field names and
configuration keys retain their ownership and compatibility contracts.

The API reliability contracts, including request retries, stream terminal
states, local authentication and gRPC integration, are documented in
[API execution and recovery](docs/api-reliability.md).
Session history errors, deletion cancellation and queued WebSocket handle
ownership are described in [session reliability](docs/session-reliability.md).
Optional runtime-owned previews are exposed through HTTP and CLI as described
in [session recaps](docs/session-recap.md); generation requires the companion
Agent/runtime changes.

`lh plugin` manages declarative plugin installations without constructing an
Agent. Enabled plugins add namespaced commands to CLI, HTTP and gateway turns;
execution and inherited policy belong to the public Agent module. See
[plugin application integration](docs/plugins.md).

## Embed the complete runtime

```go
import "github.com/yurika0211/luckyagent-app/sdk"

instance, err := sdk.New(sdk.Config{HomeDir: "/var/lib/myapp/luckyagent", APIKey: key})
if err != nil {
    return err
}
defer instance.Close()
answer, err := instance.Chat(ctx, "hello")
```

For explicit resource injection use the public Agent module's
`NewWithDependencies`; for default application preparation use `bootstrap.New`.
See [the SDK guide](sdk/README.md) for the full embedding API.

See [runtime request and recovery options](docs/runtime-options.md) for the
dependency-backed cache key, lazy WebSocket recovery and X11 capture behavior.

## Validation

```bash
GOWORK=off go test -mod=readonly -race -p 2 ./...
GOWORK=off go vet ./...
```

Build the shipped executable from the LuckyAgent launcher repository with
`GOWORK=off go build -o lh ./cmd/la`. This application module provides the command
implementation, not another copy of the launcher.
