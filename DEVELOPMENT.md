# Development Guide

## Project Overview
FreeSWITCH Call Control API (fs-api) - A Go-based REST API for controlling FreeSWITCH calls via ESL (Event Socket Layer).

## Quick Start Development

1. **Edit code** in `main.go`
2. **Deploy and test** by running: `./dev-deploy.sh`
3. **Commit when ready** using git

## How It Works

- **Language**: Go 1.25.0
- **Main file**: `main.go`
- **Build output**: `builds/` folder (git-ignored)
- **Service**: Managed by systemd as `fs-api`
- **Port**: 37274 (configurable via `FSAPI_PORT` env var)
- **ESL Connection**: localhost:8021 (configurable via `ESL_HOST`, `ESL_PORT`, `ESL_PASSWORD`)

## dev-deploy.sh Script

Automates the development workflow:
1. Stops the service
2. Builds from source code
3. Copies binary to `/usr/local/bin/fs-api`
4. Restarts the service
5. Checks health endpoint

Run: `./dev-deploy.sh`

## Testing

After deployment, test with:
```bash
# Health check
curl http://localhost:37274/health

# Example: Get FreeSWITCH status
curl http://localhost:37274/v1/status
```

## Key Endpoints

- `POST /v1/calls/{uuid}/hangup` - Hang up a call
- `POST /v1/calls/{uuid}/transfer` - Transfer a call
- `POST /v1/calls/{uuid}/answer` - Answer a call
- `POST /v1/calls/{uuid}/hold` - Put call on hold
- `POST /v1/calls/{uuid}/record` - Start/stop recording
- `POST /v1/calls/{uuid}/dtmf` - Send DTMF digits
- `POST /v1/calls/{uuid}/park` - Park a call
- `POST /v1/calls/bridge` - Bridge two calls
- `POST /v1/calls/originate` - Originate a new call
- `GET /v1/status` - Get FreeSWITCH status
- `GET /health` - Health check

## Logs

View service logs: `journalctl -u fs-api -f`
