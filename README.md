# linkerdev

A development tool that enables seamless local development with Kubernetes services by creating dynamic DNS resolution and SSH tunnels. Designed for minikube in Docker mode with Linkerd service mesh.

## Overview

linkerdev consists of two components:
- **CLI**: Local command-line tool that manages the development environment
- **Relay**: In-cluster component that handles traffic forwarding

## Features

- 🔄 **Dynamic DNS Resolution**: Automatically resolves cluster services to localhost
- 🚇 **SSH Tunneling**: Creates secure tunnels from cluster to local services
- 🎯 **Service Targeting**: Route specific services to your local development environment
- 🔧 **Easy Setup**: Simple installation and configuration
- 🐳 **Minikube Docker Mode**: Optimized for minikube with Docker driver
- 🔗 **Linkerd Integration**: Works seamlessly with Linkerd service mesh

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

- **minikube** with Docker driver
- **Linkerd** service mesh installed in your cluster
- **macOS** (for DNS resolver functionality)

## Quick Start

1. **Install the relay component** (one-time setup):
   ```bash
   sudo linkerdev install
   ```

2. **Install DNS resolver** (macOS only):
   ```bash
   sudo linkerdev install-dns
   ```

3. **Run your service with linkerdev**:
   ```bash
   linkerdev -svc my-service.namespace -p 8080 go run main.go
   ```

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
- `linkerdev install-dns` - Install DNS resolver (macOS only)
- `linkerdev uninstall-dns` - Remove DNS resolver (macOS only)
- `linkerdev clean` - Clean up leftover Kubernetes resources
- `linkerdev version` - Show version information

## How It Works

1. **DNS Resolution**: linkerdev runs a local DNS server that resolves cluster service names to localhost
2. **SSH Tunneling**: Creates reverse SSH tunnels from the cluster to your local machine
3. **Traffic Forwarding**: The relay component forwards traffic from cluster services to your local development server
4. **Dynamic Routing**: Automatically updates routing as services change

## Architecture

```
┌─────────────────┐    ┌──────────────────┐    ┌─────────────────┐
│   Local Dev     │    │   Kubernetes     │    │   Cluster       │
│   Environment   │    │   Cluster        │    │   Services      │
│                 │    │                  │    │                 │
│  ┌───────────┐  │    │  ┌────────────┐  │    │  ┌───────────┐  │
│  │   Your    │  │◄───┤  │  linkerdev │  │◄───┤  │   NATS    │  │
│  │   API     │  │    │  │   Relay    │  │    │  │  Service  │  │
│  │ (port 8080)│  │    │  │            │  │    │  │           │  │
│  └───────────┘  │    │  └────────────┘  │    │  └───────────┘  │
│                 │    │                  │    │                 │
│  ┌───────────┐  │    │                  │    │  ┌───────────┐  │
│  │   DNS     │  │    │                  │    │  │  Other    │  │
│  │  Server   │  │    │                  │    │  │ Services  │  │
│  │(port 1053)│  │    │                  │    │  │           │  │
│  └───────────┘  │    │                  │    │  └───────────┘  │
└─────────────────┘    └──────────────────┘    └─────────────────┘
```

## Configuration

### Environment Variables

- `KUBECONFIG`: Path to your Kubernetes config file (default: `~/.kube/config`)
- `LINKERDEV_NAMESPACE`: Namespace for linkerdev resources (default: `kube-system`)

### DNS Configuration

On macOS, linkerdev automatically configures `/etc/resolver/svc.cluster.local` to point to its DNS server.

## Troubleshooting

### Common Issues

1. **Permission denied when installing DNS resolver**:
   ```bash
   sudo linkerdev install-dns
   ```

2. **SSH connection timeout**:
   - Ensure your cluster supports SSH access
   - Check that the relay component is running: `kubectl get pods -n kube-system | grep linkerdev`

3. **DNS resolution not working**:
   - Verify the DNS resolver is installed: `cat /etc/resolver/svc.cluster.local`
   - Check that linkerdev is running: `lsof -i :1053`

4. **Service not accessible**:
   - Ensure the service exists in the cluster: `kubectl get svc -n <namespace>`
   - Check that your local service is running on the specified port

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
