# MeshCentral Client

Simple client for [MeshCentral](https://github.com/Ylianst/MeshCentral) using the WebSocket API. Not affiliated with MeshCentral.

## Features

* List/search devices
* TCP port forwarding (Meshrouter replacement)
* SSH connections with proxy mode support
* Direct shell access (cmd/powershell/bash)
* Multi-profile management
* Secure password storage (OS keyring)
* Cross-platform (Windows, Linux, macOS)

## Installation
```bash
# Build current platform
make build

# Build all platforms
make build-all

# Or build directly
go build -o mcc
```

## Usage
```bash
# Interactive device search
mcc search

# List all online devices
mcc ls

# TCP port forwarding
mcc route -L 8080:127.0.0.1:80 -i <nodeid>
mcc route -L 8080:80              # Interactive search, omit target IP
mcc route -L 80                   # Random local port

# SSH (interactive mode)
mcc ssh -i <nodeid>
mcc ssh user@192.168.1.1 -i <nodeid>  # SSH to network device via mesh node

# SSH proxy mode (VSCode Remote, etc.)
mcc ssh -i <nodeid> --proxy

# Direct shell access
mcc shell -i <nodeid>              # Linux/Mac: bash, Windows: cmd
mcc shell -i <nodeid> --powershell # Windows: PowerShell

# Profile management
mcc profile add -n work -s mesh.company.com -u admin -p password
mcc profile list
mcc profile default work
mcc profile rm work

# View config location
mcc config
```

### Port Forward Format
```
[localport]:[target]:[remoteport]
```

- `localport` - Optional, random if omitted
- `target` - Optional, defaults to 127.0.0.1
- `remoteport` - Required

Examples:
- `8080:192.168.1.1:80` - Local 8080 â†’ 192.168.1.1:80
- `8080:80` - Local 8080 â†’ 127.0.0.1:80
- `80` - Random local port â†’ 127.0.0.1:80

## Flags

### Global
- `-C, --config` - Alternate config file
- `-P, --profile` - Override active profile
- `-k, --insecure` - Skip TLS certificate verification (testing only)
- `--debug` - Enable debug logging

### Command-Specific
- `-i, --nodeid` - Target device ID (omit for interactive search)
- `-L, --bind-address` - Port forward specification
- `-p, --port` - SSH remote port (default: 22)
- `--proxy` - SSH proxy mode for ProxyCommand
- `--powershell` - Use PowerShell instead of cmd.exe

## Security

**Password Storage Migration (v2.0+)**

Passwords now stored in OS-native secure storage:
- **Linux**: Secret Service API (gnome-keyring/kwallet)
- **macOS**: Keychain
- **Windows**: Credential Manager

Existing plaintext passwords migrate automatically on first run. Config file only stores server/username.

**TLS Verification**

Use `--insecure` only for testing with self-signed certificates. Not recommended for production.

## Configuration

Default config: `~/.config/mcc/meshcentral-client.json`

Profiles store server URL, username. Passwords stored separately in system keyring.

## Development
```bash
make build        # Build current platform
make build-all    # Cross-compile all platforms
make version      # Show version info
make clean        # Remove build artifacts
```

## License

MIT License - see [LICENSE](LICENSE)