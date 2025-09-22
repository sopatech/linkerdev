# linkerdev

A Telepresence-like development tool that enables seamless local development with Kubernetes services using Linkerd traffic diversion and TUN interface packet interception. Designed for clusters with Linkerd service mesh.

## Overview

linkerdev consists of two components:
- **CLI**: Local command-line tool that manages the development environment and TUN interface
- **Relay**: In-cluster component that handles bidirectional traffic forwarding

## Features

- 🎯 **True Service Hijacking**: Automatically diverts traffic from original services to your local development environment
- 🔄 **Bidirectional Traffic**: Handles both inbound (cluster→local) and outbound (local→cluster) traffic
- 🔗 **Linkerd Integration**: Uses Linkerd TrafficSplit for automatic TCP traffic diversion
- 🌐 **TUN Interface**: Intercepts local outbound connections for transparent cluster access
- 🔧 **Easy Setup**: Simple installation and configuration
- 🚀 **No Manual Configuration**: Other services can call original service names without changes

## Installation

### Using Go Install (Recommended)

```bash
go install github.com/sopatech/linkerdev/cmd/linkerdev@latest
```

This will download, build, and install the latest version to your `$GOPATH/bin` directory.

### From GitHub Releases

1. Download the latest release from [GitHub Releases](https://github.com/sopatech/linkerdev/releases)
2. Extract the `linkerdev` binary to your PATH

### From Source

```bash
git clone https://github.com/sopatech/linkerdev.git
cd linkerdev
go install ./cmd/linkerdev
```

### Install the Relay Component

After installing the CLI, install the relay component in your cluster:

```bash
sudo linkerdev install
```

## Prerequisites

- **Kubernetes cluster** with Linkerd service mesh installed
- **Root privileges** (for TUN interface setup)
- **macOS or Linux** (TUN interface support)

## Quick Start

1. **Install the relay component** (one-time setup):
   ```bash
   sudo linkerdev install
   ```

2. **Run your service with linkerdev** (requires root for TUN interface):
   ```bash
   sudo linkerdev -svc api-service.apps -p 8080 go run main.go
   ```

3. **That's it!** Other services in the cluster calling `api-service` will now automatically reach your local development version.

## Usage

### Basic Usage

```bash
linkerdev -svc <service-name> -p <port> <command>
```

### Examples

```bash
# Run a Go API service
linkerdev -svc api-service.apps -p 8080 go run ./cmd/api

# Run a Node.js service
linkerdev -svc web-service.apps -p 3000 npm start

# Run a Python service
linkerdev -svc python-service.apps -p 8000 python app.py
```

### Commands

- `linkerdev install` - Install the relay component in your cluster
- `linkerdev uninstall` - Remove the relay component
- `linkerdev clean` - Clean up leftover Kubernetes resources
- `linkerdev version` - Show version information
- `linkerdev help` - Show help message

## How It Works

### Inbound Traffic (Cluster → Local)
1. **Service Creation**: Creates a `-dev` service (e.g., `api-service-dev`) pointing to the relay pod
2. **TrafficSplit**: Uses Linkerd TrafficSplit to divert 100% of traffic from `api-service` to `api-service-dev`
3. **Relay Forwarding**: Relay receives cluster traffic and forwards it to your local app via control connection

### Outbound Traffic (Local → Cluster)
1. **TUN Interface**: Creates a TUN interface to intercept local app's outbound connections
2. **Packet Interception**: Captures packets when your local app tries to reach cluster services
3. **Relay Connection**: Sends outbound connect requests to relay, which connects to actual cluster services
4. **Bidirectional Forwarding**: Relay forwards data between local app and cluster services

### Key Benefits
- **No Manual Configuration**: Other services can call original service names without changes
- **Universal TCP Support**: Works with any TCP protocol (HTTP, gRPC, databases, custom protocols)
- **Automatic Cleanup**: When linkerdev exits, TrafficSplit is deleted and traffic flows back to original services
- **True Service Hijacking**: Works like Telepresence with automatic traffic diversion

## Architecture

```
┌─────────────────┐    ┌──────────────────┐    ┌─────────────────┐
│   Local Dev     │    │   Kubernetes     │    │   Cluster       │
│   Environment   │    │   Cluster        │    │   Services      │
│                 │    │                  │    │                 │
│  ┌───────────┐  │    │  ┌────────────┐  │    │  ┌───────────┐  │
│  │   Your    │◄─┼────┤  │  linkerdev │  │◄───┤  │   NATS    │  │
│  │   API     │  │    │  │   Relay    │  │    │  │  Service  │  │
│  │(port 8080)│  │    │  │            │  │    │  │           │  │
│  └───────────┘  │    │  └────────────┘  │    │  └───────────┘  │
│        │        │    │         │        │    │                 │
│  ┌───────────┐  │    │  ┌────────────┐  │    │  ┌───────────┐  │
│  │    TUN    │  │    │  │   Linkerd  │  │    │  │  Other    │  │
│  │ Interface │  │    │  │TrafficSplit│  │    │  │ Services  │  │
│  │           │  │    │  │            │  │    │  │           │  │
│  └───────────┘  │    │  └────────────┘  │    │  └───────────┘  │
└─────────────────┘    └──────────────────┘    └─────────────────┘
        │                        │
        └─── Outbound Traffic ───┘
        └─── Inbound Traffic ────┘
```

### Traffic Flow:
- **Inbound**: Cluster Services → Linkerd TrafficSplit → Dev Service → Relay → Local App
- **Outbound**: Local App → TUN Interface → Relay → Cluster Services

## TrafficSplit Strategy

linkerdev uses Linkerd's **TrafficSplit** resource to achieve true service hijacking:

### How TrafficSplit Works:
1. **Original Service**: `api-service` (unchanged, continues to exist)
2. **Dev Service**: `api-service-dev` (created by linkerdev, points to relay)
3. **TrafficSplit**: Routes 100% of traffic from `api-service` to `api-service-dev`

### Example TrafficSplit Configuration:
```yaml
apiVersion: split.smi-spec.io/v1alpha2
kind: TrafficSplit
metadata:
  name: api-service-split
  namespace: apps
spec:
  service: api-service          # Original service name
  backends:
  - service: api-service-dev    # Dev service (100% traffic)
    weight: 100
  - service: api-service        # Original service (0% traffic)
    weight: 0
```

### Benefits:
- ✅ **No Service Changes**: Calling services still use `api-service` name
- ✅ **Universal Protocol Support**: Works with any TCP protocol
- ✅ **Automatic Cleanup**: TrafficSplit deletion restores normal traffic flow
- ✅ **Zero Downtime**: Seamless traffic diversion and restoration

## Configuration

### Environment Variables

- `KUBECONFIG`: Path to your Kubernetes config file (default: `~/.kube/config`)

### TUN Interface

The TUN interface requires root privileges for:
- Creating the TUN device
- Setting up routing rules
- Configuring iptables (Linux) or route tables (macOS)

## Troubleshooting

### Common Issues

1. **Permission denied for TUN interface**:
   ```bash
   sudo linkerdev -svc api-service.apps -p 8080 go run main.go
   ```

2. **Relay connection timeout**:
   - Check that the relay component is running: `kubectl get pods -n kube-system | grep linkerdev`
   - Verify Linkerd is installed: `linkerd check`

3. **Traffic not being diverted**:
   - Ensure Linkerd TrafficSplit resource is created: `kubectl get trafficsplit -n <namespace>`
   - Check that the dev service exists: `kubectl get svc -n <namespace> | grep -dev`
   - Verify TrafficSplit is routing 100% to dev service: `kubectl describe trafficsplit <service-name>-split -n <namespace>`

4. **Outbound connections not working**:
   - Verify TUN interface is created: `ip link show utun0` (Linux) or `ifconfig utun0` (macOS)
   - Check routing rules: `ip route show` (Linux) or `netstat -rn` (macOS)

### Debug Mode

Run linkerdev with verbose logging:
```bash
linkerdev -v -svc my-service.apps -p 8080 go run main.go
```

## Development

### Building from Source

```bash
# Build CLI
cd cmd/linkerdev
go build -o linkerdev .

# Build relay
cd ../relay
go build -o linkerdev-relay .
```

### Running Tests

```bash
go test ./...
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests
5. Submit a pull request

## License

MIT License - see [LICENSE](LICENSE) file for details.

## Support

- 📖 [Documentation](https://github.com/sopatech/linkerdev/wiki)
- 🐛 [Issue Tracker](https://github.com/sopatech/linkerdev/issues)
- 💬 [Discussions](https://github.com/sopatech/linkerdev/discussions)
