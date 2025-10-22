# FreeSWITCH Call Control API

A RESTful API service written in Go for controlling FreeSWITCH calls via Event Socket Library (ESL).

## Overview

This service provides a simple, stateless HTTP API for controlling FreeSWITCH call operations. It listens on localhost port 37274 and communicates with FreeSWITCH via the Event Socket Library.

## Features

- **10 API Endpoints**: 9 Call Control + 1 Status endpoint
  - Call Control: Hangup, Transfer, Bridge, Answer, Hold/Unhold, Record, DTMF, Park, Originate
  - System: FreeSWITCH Status
- **RESTful Design**: Clean JSON API following OpenAPI 3.0 specification
- **Production Ready**: UUID validation, request tracing, structured logging, graceful shutdown
- **Systemd Integration**: Runs as a system service with automatic restart
- **Health Monitoring**: Built-in health check endpoint with ESL connection testing

## Installation

### Quick Install (Recommended)

Use the automated installation script:

```bash
# Clone the repository
git clone https://github.com/emaktel/fs-api.git
cd fs-api

# Run the installer (auto-detects OS and architecture)
./install.sh
```

The installer will:
- Download the correct binary for your platform
- Install to `/usr/local/bin/fs-api`
- Set up systemd service (Linux only)
- Enable auto-start on boot

### Manual Installation

Download the binary for your platform from [releases](https://github.com/emaktel/fs-api/releases/latest):

```bash
# Linux AMD64
wget https://github.com/emaktel/fs-api/releases/download/v0.1.0/fs-api-linux-amd64
chmod +x fs-api-linux-amd64
sudo mv fs-api-linux-amd64 /usr/local/bin/fs-api

# Install systemd service (Linux)
sudo cp fs-api.service /etc/systemd/system/fs-api.service
sudo systemctl daemon-reload
sudo systemctl enable fs-api
sudo systemctl start fs-api
```

### Installation Locations

After installation:
- **Binary Location**: `/usr/local/bin/fs-api`
- **Service File**: `/etc/systemd/system/fs-api.service` (Linux)
- **Working Directory**: `/usr/local/bin`

## Configuration

The API can be configured using environment variables. If environment variables are not set, the following defaults are used:

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `FSAPI_PORT` | API server port | `37274` |
| `ESL_HOST` | FreeSWITCH ESL host address | `localhost` |
| `ESL_PORT` | FreeSWITCH ESL port | `8021` |
| `ESL_PASSWORD` | FreeSWITCH ESL password | `ClueCon` |

### Configuration Examples

**Using Environment Variables (Recommended for Production)**:
```bash
# Set environment variables
export FSAPI_PORT="8080"
export ESL_HOST="freeswitch.example.com"
export ESL_PORT="8021"
export ESL_PASSWORD="MySecurePassword"

# Run the service
fs-api
```

**Using Systemd with Environment Variables**:

Edit `/etc/systemd/system/fs-api.service` and add environment variables:
```ini
[Service]
Environment="FSAPI_PORT=8080"
Environment="ESL_HOST=freeswitch.example.com"
Environment="ESL_PORT=8021"
Environment="ESL_PASSWORD=MySecurePassword"
ExecStart=/usr/local/bin/fs-api
```

Then reload and restart:
```bash
systemctl daemon-reload
systemctl restart fs-api.service
```

### Default Settings (No Environment Variables)

If no environment variables are set, the API uses these defaults:

- **API Host**: All interfaces (0.0.0.0)
- **API Port**: 37274 (FSAPI)
- **Base Path**: `/v1`
- **ESL Host**: localhost
- **ESL Port**: 8021
- **ESL Password**: ClueCon

## Service Management

### Check Service Status
```bash
systemctl status fs-api.service
```

### Start/Stop/Restart Service
```bash
systemctl start fs-api.service
systemctl stop fs-api.service
systemctl restart fs-api.service
```

### Enable/Disable Auto-start on Boot
```bash
systemctl enable fs-api.service
systemctl disable fs-api.service
```

### View Logs
```bash
# Follow logs in real-time
journalctl -u fs-api.service -f

# View recent logs
journalctl -u fs-api.service -n 100

# View logs since boot
journalctl -u fs-api.service -b
```

## API Endpoints

### Health Check
```bash
GET /health
```

**Example**:
```bash
curl http://localhost:37274/health
```

**Response**: `OK`

---

### 1. Hangup Call
Terminate a specific call leg.

```bash
POST /v1/calls/{uuid}/hangup
```

**Request Body** (optional):
```json
{
  "cause": "USER_BUSY"
}
```

**Example**:
```bash
curl -X POST http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef/hangup \
  -H "Content-Type: application/json" \
  -d '{"cause":"NORMAL_CLEARING"}'
```

**Response**:
```json
{
  "status": "success",
  "message": "Call a1b2c3d4-e5f6-7890-1234-567890abcdef hung up with cause NORMAL_CLEARING"
}
```

---

### 2. Transfer Call
Transfer a call to a new destination in the dialplan.

```bash
POST /v1/calls/{uuid}/transfer
```

**Request Body** (required):
```json
{
  "destination": "5000",
  "context": "internal"
}
```

**Example**:
```bash
curl -X POST http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef/transfer \
  -H "Content-Type: application/json" \
  -d '{"destination":"5000","context":"internal"}'
```

**Response**:
```json
{
  "status": "success",
  "message": "Call a1b2c3d4-e5f6-7890-1234-567890abcdef transferred to 5000 in context internal"
}
```

---

### 3. Bridge Calls
Bridge two separate call legs together.

```bash
POST /v1/calls/bridge
```

**Request Body** (required):
```json
{
  "uuid_a": "a1b2c3d4-e5f6-7890-1234-567890abcdef",
  "uuid_b": "e5f6-7890-1234-5678-90abcdef1234"
}
```

**Example**:
```bash
curl -X POST http://localhost:37274/v1/calls/bridge \
  -H "Content-Type: application/json" \
  -d '{"uuid_a":"a1b2c3d4-e5f6-7890-1234-567890abcdef","uuid_b":"e5f6-7890-1234-5678-90abcdef1234"}'
```

**Response**:
```json
{
  "status": "success",
  "message": "Calls a1b2c3d4-e5f6-7890-1234-567890abcdef and e5f6-7890-1234-5678-90abcdef1234 bridged"
}
```

---

### 4. Answer Call
Answer a ringing call.

```bash
POST /v1/calls/{uuid}/answer
```

**Example**:
```bash
curl -X POST http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef/answer
```

**Response**:
```json
{
  "status": "success",
  "message": "Call a1b2c3d4-e5f6-7890-1234-567890abcdef answered"
}
```

---

### 5. Hold/Unhold Call
Place a call on hold or unhold it.

```bash
POST /v1/calls/{uuid}/hold
```

**Request Body** (required):
```json
{
  "action": "hold"
}
```

Valid actions: `hold` or `unhold`

**Example**:
```bash
# Put call on hold
curl -X POST http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef/hold \
  -H "Content-Type: application/json" \
  -d '{"action":"hold"}'

# Take call off hold
curl -X POST http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef/hold \
  -H "Content-Type: application/json" \
  -d '{"action":"unhold"}'
```

**Response**:
```json
{
  "status": "success",
  "message": "Call a1b2c3d4-e5f6-7890-1234-567890abcdef hold"
}
```

---

### 6. Record Call
Start or stop recording a call.

```bash
POST /v1/calls/{uuid}/record
```

**Request Body for Start**:
```json
{
  "action": "start",
  "filename": "/var/spool/fs/recordings/call_12345.wav"
}
```

**Request Body for Stop**:
```json
{
  "action": "stop"
}
```

**Example**:
```bash
# Start recording
curl -X POST http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef/record \
  -H "Content-Type: application/json" \
  -d '{"action":"start","filename":"/var/spool/fs/recordings/call_12345.wav"}'

# Stop recording
curl -X POST http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef/record \
  -H "Content-Type: application/json" \
  -d '{"action":"stop"}'
```

**Response**:
```json
{
  "status": "success",
  "message": "Recording start for call a1b2c3d4-e5f6-7890-1234-567890abcdef"
}
```

---

### 7. Send DTMF
Send DTMF digits to a call leg.

```bash
POST /v1/calls/{uuid}/dtmf
```

**Request Body**:
```json
{
  "digits": "1234#",
  "duration": 150
}
```

- `digits` (required): DTMF sequence to send
- `duration` (optional): Tone duration in milliseconds (default: 100)

**Example**:
```bash
curl -X POST http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef/dtmf \
  -H "Content-Type: application/json" \
  -d '{"digits":"1234#","duration":150}'
```

**Response**:
```json
{
  "status": "success",
  "message": "DTMF 1234# sent to call a1b2c3d4-e5f6-7890-1234-567890abcdef"
}
```

---

### 8. Park Call
Park a specific call leg.

```bash
POST /v1/calls/{uuid}/park
```

**Example**:
```bash
curl -X POST http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef/park
```

**Response**:
```json
{
  "status": "success",
  "message": "Call a1b2c3d4-e5f6-7890-1234-567890abcdef parked"
}
```

---

### 9. Originate Call
Initiate a new call between two endpoints.

```bash
POST /v1/calls/originate
```

**Request Body** (required):
```json
{
  "aleg": "sofia/default/1001@domain.com",
  "bleg": "9005551212",
  "dialplan": "XML",
  "context": "default",
  "caller_id_name": "John Doe",
  "caller_id_number": "5551234567",
  "timeout_sec": 60,
  "channel_variables": {
    "origination_caller_id_number": "9005551212",
    "ignore_early_media": true,
    "hangup_after_bridge": true
  }
}
```

**Required Fields**:
- `aleg`: The A-leg (originating) endpoint (e.g., `sofia/default/user@domain.com`)
- `bleg`: The B-leg destination - can be an extension number or an application (e.g., `5000` or `&bridge(sofia/default/1002@domain.com)`)

**Optional Fields**:
- `dialplan`: Dialplan to use (default: none)
- `context`: Dialplan context (default: none)
- `caller_id_name`: Caller ID name to display
- `caller_id_number`: Caller ID number to display
- `timeout_sec`: Call timeout in seconds
- `channel_variables`: Object containing FreeSWITCH channel variables as key-value pairs

**Example 1 - Dialplan-based call**:
```bash
curl -X POST http://localhost:37274/v1/calls/originate \
  -H "Content-Type: application/json" \
  -d '{
    "aleg": "sofia/default/1001@domain.com",
    "bleg": "9005551212",
    "dialplan": "XML",
    "context": "default",
    "caller_id_name": "Test Call",
    "caller_id_number": "5551234567"
  }'
```

**Example 2 - Direct bridge (bypass dialplan)**:
```bash
curl -X POST http://localhost:37274/v1/calls/originate \
  -H "Content-Type: application/json" \
  -d '{
    "aleg": "sofia/default/1001@domain.com",
    "bleg": "&bridge(sofia/default/1002@domain.com)",
    "channel_variables": {
      "origination_caller_id_number": "9005551212",
      "ignore_early_media": true
    }
  }'
```

**Response**:
```json
{
  "status": "success",
  "data": {
    "response": "a1b2c3d4-e5f6-7890-1234-567890abcdef"
  }
}
```

**Description**: Initiates a new call using FreeSWITCH's originate command. The response contains the UUID of the originated call or job UUID if using bgapi.

---

### 10. Get FreeSWITCH Status
Retrieve detailed status information from the FreeSWITCH server.

```bash
GET /v1/status
```

**Example**:
```bash
curl http://localhost:37274/v1/status
```

**Response**:
```json
{
  "status": "success",
  "data": {
    "systemStatus": "ready",
    "uptime": {
      "years": 0,
      "days": 1,
      "hours": 2,
      "minutes": 30,
      "seconds": 45,
      "milliseconds": 123,
      "microseconds": 456
    },
    "version": "1.10.8-dev git 4187483 2022-09-06 17:54:16Z 64bit",
    "sessions": {
      "count": {
        "total": 203,
        "active": 5,
        "peak": 22,
        "peak5Min": 14,
        "limit": 5000
      },
      "rate": {
        "current": 0,
        "max": 250,
        "peak": 12,
        "peak5Min": 12
      }
    },
    "idleCPU": {
      "used": 0,
      "allowed": 98.67
    },
    "stackSizeKB": {
      "current": 240,
      "max": 8192
    }
  }
}
```

**Description**: Returns detailed, structured JSON status information from FreeSWITCH, including:
- System status (ready/not ready)
- Server uptime breakdown
- FreeSWITCH version information
- Session statistics (current, peak, limits)
- Call rate information
- CPU usage metrics
- Stack size information

---

## Error Responses

All endpoints return error responses in the following format:

```json
{
  "status": "error",
  "message": "Details about the error that occurred"
}
```

**Example Error Scenarios**:
- Invalid request body: `400 Bad Request`
- Missing required fields: `400 Bad Request`
- ESL command failure: `500 Internal Server Error`

## Architecture

### Technology Stack
- **Language**: Go 1.25.0
- **Router**: Gorilla Mux
- **Protocol**: HTTP/REST
- **FreeSWITCH Communication**: Event Socket Library (ESL)

### Project Structure
```
/root/fs-api/
├── main.go           # Main application code
├── go.mod            # Go module definition
├── go.sum            # Go dependencies checksums
├── fs-api            # Compiled binary
└── README.md         # This file
```

### ESL Integration

The API uses the `github.com/percipia/eslgo` library for FreeSWITCH Event Socket Library communication. This provides a production-ready ESL client with connection pooling and automatic reconnection.

## Building from Source

If you need to rebuild the application:

```bash
# Clone the repository
git clone https://github.com/emaktel/fs-api.git
cd fs-api

# Build the binary
go build -o fs-api main.go

# Install it
sudo mv fs-api /usr/local/bin/fs-api

# Restart the service (if using systemd)
sudo systemctl restart fs-api.service
```

## Troubleshooting

### Service Won't Start
```bash
# Check for errors in the logs
journalctl -u fs-api.service -n 50

# Check if port is already in use
ss -tlnp | grep 37274

# Verify binary exists and is executable
ls -l /root/fs-api/fs-api
```

### API Not Responding
```bash
# Verify service is running
systemctl status fs-api.service

# Test with health endpoint
curl http://localhost:37274/health

# Check if listening on correct port
ss -tlnp | grep 37274
```

### FreeSWITCH Connection Issues
- Verify FreeSWITCH is running
- Check ESL is enabled in FreeSWITCH configuration
- Verify ESL password matches (default: ClueCon)
- Check firewall rules if FreeSWITCH is on a different host

## Security Considerations

- The service binds to `localhost` only for security
- To expose externally, modify the bind address in `main.go` and add proper authentication
- Consider implementing API key authentication for production use
- Use HTTPS/TLS for encrypted communication
- Implement rate limiting to prevent abuse

## License

This implementation follows the FreeSWITCH Call Control API specification v1.0.0.

## Support

For issues or questions:
- Check service logs: `journalctl -u fs-api.service -f`
- Verify FreeSWITCH ESL connectivity
- Review this README for common troubleshooting steps
