# FreeSWITCH Call Control API

A RESTful API service written in Go for controlling FreeSWITCH calls via Event Socket Library (ESL).

## Overview

This service provides a simple, stateless HTTP API for controlling FreeSWITCH call operations. It listens on localhost port 37274 and communicates with FreeSWITCH via the Event Socket Library.

## Features

- **33 API Endpoints**: 9 Call Control + 3 Query + 2 Registrations + 19 Callcenter Management
  - Call Control: Hangup, Transfer, Bridge, Answer, Hold/Unhold, Record, DTMF, Park, Originate
  - Query: List Calls, Call Details, FreeSWITCH Status
  - Registrations: List and count active SIP registrations, filtered by realm/domain
  - Callcenter: Queue CRUD, Agent CRUD, Tier CRUD, Member listing, counts
- **Bearer Token Authentication**: Secure remote access with configurable API tokens
  - Localhost requests bypass authentication for convenience
  - Multiple token support for different clients/users
  - Optional and backward compatible
- **Context-Based Authorization**: Optional multi-tenant security via `X-Allowed-Contexts` header
  - Restrict operations by FreeSWITCH context (e.g., domain/tenant)
  - Wildcard `*` support for super admin access
  - Backward compatible (no header = unrestricted access)
- **RESTful Design**: Clean JSON API with full [OpenAPI 3.0 specification](openapi.yaml)
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
wget https://github.com/emaktel/fs-api/releases/download/v0.3.0/fs-api-linux-amd64
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
| `FSAPI_AUTH_TOKENS` | Comma-separated Bearer tokens for authentication | *(none)* |

### Bearer Token Authentication

The API supports Bearer token authentication to secure remote access:

- **Localhost Bypass**: Requests from localhost (127.0.0.1, ::1) always bypass authentication
- **Remote Requests**: Require valid Bearer token when `FSAPI_AUTH_TOKENS` is configured
- **Multiple Tokens**: Supports comma-separated tokens for different clients/users
- **Backward Compatible**: If no tokens configured, behaves like version 0.2.0 (no auth required)

**Configuration**:
```bash
# Single token
export FSAPI_AUTH_TOKENS="your-secret-token-here"

# Multiple tokens (comma-separated)
export FSAPI_AUTH_TOKENS="token1,token2,token3"
```

**Usage**:
```bash
# Localhost - no auth required
curl http://localhost:37274/v1/status

# Remote - requires Bearer token
curl -H "Authorization: Bearer your-secret-token-here" http://example.com:37274/v1/status
```

### Configuration Examples

**Using Environment Variables (Recommended for Production)**:
```bash
# Set environment variables
export FSAPI_PORT="8080"
export ESL_HOST="freeswitch.example.com"
export ESL_PORT="8021"
export ESL_PASSWORD="MySecurePassword"
export FSAPI_AUTH_TOKENS="your-secret-token-here"

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
Environment="FSAPI_AUTH_TOKENS=your-secret-token-here"
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

## Authorization

### Context-Based Authorization

All call control and query endpoints support optional context-based authorization via the `X-Allowed-Contexts` header. This enables multi-tenant security by restricting API operations to specific FreeSWITCH contexts (domains/tenants).

#### Header Format

```
X-Allowed-Contexts: context1,context2,context3
```

Or for super admin access:
```
X-Allowed-Contexts: *
```

#### Authorization Modes

| Header Value | Behavior | Use Case |
|-------------|----------|----------|
| *(empty/missing)* | Unrestricted access | Internal API calls, backward compatibility |
| `*` | Unrestricted access (explicit) | Super admin, monitoring tools |
| `context1.com` | Single context only | Regular user assigned to one tenant |
| `context1.com,context2.com` | Multiple contexts | User managing multiple tenants |

#### How It Works

When a request includes the `X-Allowed-Contexts` header:

1. **Call operations** (hangup, transfer, hold, etc.): The API checks the call's `accountcode` field (which contains the FreeSWITCH context) and verifies it matches one of the allowed contexts
2. **Originate operations**: The API validates the requested `context` parameter against allowed contexts
3. **Bridge operations**: Both call UUIDs are validated against allowed contexts

#### Examples

**Unrestricted access (no header)**:
```bash
curl http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef
```

**Super admin with wildcard**:
```bash
curl -H "X-Allowed-Contexts: *" \
  http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef
```

**Single context restriction**:
```bash
curl -H "X-Allowed-Contexts: customer1.example.com" \
  -X POST http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef/hangup
```

**Multiple contexts**:
```bash
curl -H "X-Allowed-Contexts: customer1.example.com,customer2.example.com" \
  http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef
```

**Originate with context validation**:
```bash
curl -H "X-Allowed-Contexts: customer1.example.com" \
  -X POST http://localhost:37274/v1/calls/originate \
  -H "Content-Type: application/json" \
  -d '{
    "aleg": "user/1000",
    "bleg": "&park()",
    "context": "customer1.example.com"
  }'
```

#### Error Responses

**Call not found**:
```json
{
  "status": "error",
  "message": "Call a1b2c3d4-e5f6-7890-1234-567890abcdef not found"
}
```

**Unauthorized context (403 Forbidden)**:
```json
{
  "status": "error",
  "message": "Call a1b2c3d4-e5f6-7890-1234-567890abcdef belongs to context 'customer2.example.com' which is not in your allowed contexts: [customer1.example.com]"
}
```

**Originate with unauthorized context**:
```json
{
  "status": "error",
  "message": "Cannot originate call in context 'customer2.example.com' - not in your allowed contexts: [customer1.example.com]"
}
```

#### Protected Endpoints

The following endpoints enforce context authorization when `X-Allowed-Contexts` header is present:

- ✅ `GET /v1/calls/{uuid}` - Get call details
- ✅ `POST /v1/calls/{uuid}/hangup` - Hangup call
- ✅ `POST /v1/calls/{uuid}/transfer` - Transfer call
- ✅ `POST /v1/calls/{uuid}/answer` - Answer call
- ✅ `POST /v1/calls/{uuid}/hold` - Hold/unhold call
- ✅ `POST /v1/calls/{uuid}/record` - Start/stop recording
- ✅ `POST /v1/calls/{uuid}/dtmf` - Send DTMF
- ✅ `POST /v1/calls/{uuid}/park` - Park call
- ✅ `POST /v1/calls/bridge` - Bridge two calls (validates both UUIDs)
- ✅ `POST /v1/calls/originate` - Originate call (validates context parameter)
- ✅ All `/v1/callcenter/queues/*` endpoints - Validated by queue `name@domain`
- ✅ All `/v1/callcenter/agents/*` endpoints - Validated by `domain` in request body (agent names are UUIDs; domain lives in the `contact` field)
- ✅ All `/v1/callcenter/tiers/*` endpoints - Validated by queue `name@domain`
- ✅ `GET /v1/callcenter/queues` - List filtered by queue domain
- ✅ `GET /v1/callcenter/agents` - List filtered by `domain_name=` in agent contact
- ✅ `GET /v1/callcenter/tiers` - List filtered by queue domain
- ✅ `GET /v1/registrations` - List filtered by `realm` field
- ✅ `GET /v1/registrations/count` - Count filtered by `realm` field

**Unprotected Endpoints** (system-level, no context validation):
- `GET /v1/status` - FreeSWITCH status
- `GET /health` - Health check

---

## API Endpoints

### Health Check
```bash
GET /health
```

**Example**:
```bash
curl http://localhost:37274/health
```

**Response**:
```json
{
  "status": "healthy",
  "version": "0.4.1"
}
```

---

### 1. List All Calls
Retrieve a list of all active calls, filtered by allowed contexts.

```bash
GET /v1/calls
```

**Required Header**:
```
X-Allowed-Contexts: context1,context2,* (required)
```

**Description**: This endpoint returns all active calls filtered by the `X-Allowed-Contexts` header. The header is **mandatory** for this endpoint:
- `X-Allowed-Contexts: *` - Returns all active calls (super admin/unrestricted access)
- `X-Allowed-Contexts: context1.com` - Returns calls belonging to a single context
- `X-Allowed-Contexts: context1.com,context2.com` - Returns calls belonging to any of the specified contexts

**Examples**:

**Get all calls (unrestricted)**:
```bash
curl -H "X-Allowed-Contexts: *" http://localhost:37274/v1/calls
```

**Get calls for specific context**:
```bash
curl -H "X-Allowed-Contexts: customer1.example.com" http://localhost:37274/v1/calls
```

**Get calls for multiple contexts**:
```bash
curl -H "X-Allowed-Contexts: customer1.example.com,customer2.example.com" http://localhost:37274/v1/calls
```

**Response (Success)**:
```json
{
  "status": "success",
  "row_count": 2,
  "rows": [
    {
      "uuid": "a1b2c3d4-e5f6-7890-1234-567890abcdef",
      "direction": "inbound",
      "created": "2025-11-07 17:47:10",
      "created_epoch": "1762555630",
      "name": "sofia/internal/100@domain.com",
      "state": "CS_EXECUTE",
      "cid_name": "100",
      "cid_num": "100",
      "dest": "5146272886",
      "callstate": "ACTIVE",
      "accountcode": "customer1.example.com",
      "b_uuid": "e5f6-7890-1234-5678-90abcdef1234",
      "b_direction": "outbound",
      "b_created": "2025-11-07 17:47:10",
      "b_name": "sofia/external/+15551234567",
      "b_state": "CS_EXCHANGE_MEDIA",
      "b_cid_name": "Caller",
      "b_cid_num": "+15551234567",
      "b_callstate": "ACTIVE"
    },
    {
      "uuid": "b2c3d4e5-f6-7890-1234-567890abcdef2",
      "direction": "inbound",
      "created": "2025-11-07 17:48:15",
      "created_epoch": "1762555695",
      "name": "sofia/internal/101@domain.com",
      "state": "CS_EXECUTE",
      "cid_name": "101",
      "cid_num": "101",
      "dest": "5146272887",
      "callstate": "ACTIVE",
      "accountcode": "customer1.example.com",
      "b_uuid": ""
    }
  ]
}
```

**Error Response (Missing Header)**:
```json
{
  "status": "error",
  "message": "X-Allowed-Contexts header is required for this endpoint"
}
```

**Error Response (Unauthorized Context)**:
```json
{
  "status": "error",
  "message": "Call a1b2c3d4-e5f6-7890-1234-567890abcdef belongs to context 'other.example.com' which is not in your allowed contexts: [customer1.example.com]"
}
```

**Notes**:
- `row_count` shows the number of calls returned
- `rows` contains the list of all active calls matching the allowed contexts
- Each row contains call summary information from FreeSWITCH's `show calls` output
- Empty `rows` list means no active calls match the specified contexts
- This endpoint requires the `X-Allowed-Contexts` header (unlike other endpoints where it's optional)

---

### 2. Get Call Details
Retrieve complete call information including both A-leg and B-leg details.

```bash
GET /v1/calls/{uuid}
```

**Description**: This endpoint efficiently retrieves full call details using a 3-step process:
1. Looks up the call using `show calls as json` to get both A-leg and B-leg UUIDs
2. Dumps A-leg channel variables using `uuid_dump <uuid> json`
3. Dumps B-leg channel variables using `uuid_dump <uuid> json` (if B-leg exists)

All data is returned as properly structured JSON objects. You can query by either A-leg or B-leg UUID.

**Example**:
```bash
curl http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef
```

**Response (Bridged Call - Two Legs)**:
```json
{
  "status": "success",
  "call_info": {
    "uuid": "a1b2c3d4-e5f6-7890-1234-567890abcdef",
    "direction": "inbound",
    "created": "2025-11-07 17:47:10",
    "created_epoch": "1762555630",
    "name": "sofia/internal/100@domain.com",
    "state": "CS_EXECUTE",
    "cid_name": "100",
    "cid_num": "100",
    "dest": "5146272886",
    "callstate": "ACTIVE",
    "accountcode": "domain.com",
    "b_uuid": "e5f6-7890-1234-5678-90abcdef1234",
    "b_direction": "outbound",
    "b_created": "2025-11-07 17:47:10",
    "b_name": "sofia/external/+15551234567",
    "b_state": "CS_EXCHANGE_MEDIA",
    "b_cid_name": "Caller",
    "b_cid_num": "+15551234567",
    "b_callstate": "ACTIVE"
  },
  "aleg": {
    "uuid": "a1b2c3d4-e5f6-7890-1234-567890abcdef",
    "details": {
      "Channel-Name": "sofia/internal/100@domain.com",
      "Channel-State": "CS_EXECUTE",
      "Call-Direction": "inbound",
      "Caller-Caller-ID-Name": "100",
      "Caller-Caller-ID-Number": "100",
      "Answer-State": "answered",
      "Caller-Destination-Number": "5146272886"
    }
  },
  "bleg": {
    "uuid": "e5f6-7890-1234-5678-90abcdef1234",
    "details": {
      "Channel-Name": "sofia/external/+15551234567",
      "Channel-State": "CS_EXCHANGE_MEDIA",
      "Call-Direction": "outbound",
      "Caller-Caller-ID-Name": "Caller",
      "Caller-Caller-ID-Number": "+15551234567",
      "Answer-State": "answered"
    }
  }
}
```

**Response (Single Leg Call)**:
```json
{
  "status": "success",
  "call_info": {
    "uuid": "a1b2c3d4-e5f6-7890-1234-567890abcdef",
    "direction": "inbound",
    "created": "2025-11-07 18:06:21",
    "name": "sofia/internal/100@domain.com",
    "state": "CS_EXECUTE",
    "cid_name": "100",
    "cid_num": "100",
    "dest": "*9667",
    "callstate": "ACTIVE",
    "b_uuid": "",
    "b_direction": "",
    "b_name": ""
  },
  "aleg": {
    "uuid": "a1b2c3d4-e5f6-7890-1234-567890abcdef",
    "details": {
      "Channel-Name": "sofia/internal/100@domain.com",
      "Channel-State": "CS_EXECUTE",
      "Call-Direction": "inbound",
      "Caller-Caller-ID-Name": "100",
      "Caller-Caller-ID-Number": "100",
      "Answer-State": "answered"
    }
  }
}
```

**Notes**:
- `call_info` contains summary information from FreeSWITCH's `show calls` output
- `aleg` contains full channel details for the A-leg from `uuid_dump`
- `bleg` is only included if the call has a B-leg (bridged call)
- All b_ prefixed fields in `call_info` will be empty strings for single-leg calls
- You can query using either the A-leg UUID or B-leg UUID

**Error Response (Call Not Found)**:
```json
{
  "status": "error",
  "message": "Call a1b2c3d4-e5f6-7890-1234-567890abcdef not found"
}
```

---

### 3. Hangup Call
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

### 4. Transfer Call
Transfer a call to a new destination in the dialplan. Supports transferring A-leg (default), B-leg, or both legs.

```bash
POST /v1/calls/{uuid}/transfer
```

**Request Body**:
```json
{
  "destination": "5000",
  "leg": "aleg",
  "dialplan": "XML",
  "context": "internal"
}
```

**Parameters**:
- `destination` (required): Destination extension or number
- `leg` (optional): Which leg to transfer - `"aleg"` (default), `"bleg"`, or `"both"`
- `dialplan` (optional): Dialplan type - defaults to `"XML"` when context is provided
- `context` (optional): Dialplan context - if provided, dialplan will also be sent (defaults to "XML")

**Note**: `dialplan` and `context` are sent as a pair. If you omit `context`, the `dialplan` parameter is ignored.

**Example 1 - Basic transfer (A-leg, no context)**:
```bash
curl -X POST http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef/transfer \
  -H "Content-Type: application/json" \
  -d '{"destination":"5000"}'
```

**Example 2 - Transfer with context (dialplan defaults to XML)**:
```bash
curl -X POST http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef/transfer \
  -H "Content-Type: application/json" \
  -d '{"destination":"5000","context":"internal"}'
```

**Example 3 - Transfer B-leg**:
```bash
curl -X POST http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef/transfer \
  -H "Content-Type: application/json" \
  -d '{"destination":"*9664","context":"f1-dev.emaktech.com","leg":"bleg"}'
```

**Example 4 - Transfer both legs**:
```bash
curl -X POST http://localhost:37274/v1/calls/a1b2c3d4-e5f6-7890-1234-567890abcdef/transfer \
  -H "Content-Type: application/json" \
  -d '{"destination":"5000","context":"internal","leg":"both"}'
```

**Response**:
```json
{
  "status": "success",
  "message": "Call a1b2c3d4-e5f6-7890-1234-567890abcdef (A-leg) transferred to 5000 dialplan XML context internal"
}
```

---

### 5. Bridge Calls
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

### 6. Answer Call
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

### 7. Hold/Unhold Call
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

### 8. Record Call
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

### 9. Send DTMF
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

### 10. Park Call
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

### 11. Originate Call
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

### 12. Get FreeSWITCH Status
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

## Registrations API Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/v1/registrations` | List active SIP registrations (filtered by realm) |
| `GET` | `/v1/registrations/count` | Count active SIP registrations |

Both endpoints require the `X-Allowed-Contexts` header. Results are filtered by the `realm` field of each registration entry, which contains the SIP domain (e.g. `customer1.example.com`).

**List registrations for a specific tenant:**
```bash
curl http://localhost:37274/v1/registrations \
  -H "Authorization: Bearer <token>" \
  -H "X-Allowed-Contexts: customer1.example.com"
```

**Count registrations across multiple tenants:**
```bash
curl http://localhost:37274/v1/registrations/count \
  -H "Authorization: Bearer <token>" \
  -H "X-Allowed-Contexts: customer1.example.com,customer2.example.com"
```

Each registration row contains: `reg_user`, `realm`, `token`, `url`, `expires`, `network_ip`, `network_port`, `network_proto`, `hostname`, `metadata`.

---

## Callcenter API Endpoints

> Full details for all callcenter endpoints are in the [OpenAPI spec](openapi.yaml).

### Queue Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/v1/callcenter/queues` | List all queues (filtered by domain) |
| `GET` | `/v1/callcenter/queues/count` | Count queues |
| `GET` | `/v1/callcenter/queues/{queue_name}/agents` | List agents in a queue |
| `GET` | `/v1/callcenter/queues/{queue_name}/agents/count` | Count agents (supports `?status=` filter) |
| `GET` | `/v1/callcenter/queues/{queue_name}/members` | List members (callers) in a queue |
| `GET` | `/v1/callcenter/queues/{queue_name}/members/count` | Count members in a queue |
| `GET` | `/v1/callcenter/queues/{queue_name}/tiers` | List tiers in a queue |
| `GET` | `/v1/callcenter/queues/{queue_name}/tiers/count` | Count tiers in a queue |
| `POST` | `/v1/callcenter/queues/{queue_name}/load` | Load queue into memory |
| `POST` | `/v1/callcenter/queues/{queue_name}/unload` | Unload queue from memory |
| `POST` | `/v1/callcenter/queues/{queue_name}/reload` | Reload queue configuration |

Queue names use `name@domain` format (e.g. `support@customer1.example.com`).

### Agent Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/v1/callcenter/agents` | List all agents (filtered by contact domain) |
| `POST` | `/v1/callcenter/agents` | Add a new agent |
| `PUT` | `/v1/callcenter/agents/{agent_name}` | Set an agent attribute |
| `DELETE` | `/v1/callcenter/agents/{agent_name}` | Delete an agent |

Agent names are UUIDs. The `domain` field in the request body is used for authorization since the domain is stored in the agent's `contact` field (as `domain_name=<value>`), not in the agent name.

**Add agent**:
```bash
curl -X POST http://localhost:37274/v1/callcenter/agents \
  -H "Content-Type: application/json" \
  -H "X-Allowed-Contexts: customer1.example.com" \
  -d '{"name":"a1b2c3d4-e5f6-7890-1234-567890abcdef","type":"callback","domain":"customer1.example.com"}'
```

**Set agent status**:
```bash
curl -X PUT http://localhost:37274/v1/callcenter/agents/a1b2c3d4-e5f6-7890-1234-567890abcdef \
  -H "Content-Type: application/json" \
  -H "X-Allowed-Contexts: customer1.example.com" \
  -d '{"key":"status","value":"Available","domain":"customer1.example.com"}'
```

Valid agent set keys: `status`, `state`, `contact`, `type`, `max_no_answer`, `wrap_up_time`, `reject_delay_time`, `busy_delay_time`, `ready_time`.

### Tier Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/v1/callcenter/tiers` | List all tiers (filtered by queue domain) |
| `POST` | `/v1/callcenter/tiers` | Add a new tier |
| `PUT` | `/v1/callcenter/tiers` | Set a tier attribute |
| `DELETE` | `/v1/callcenter/tiers` | Delete a tier |

Tier operations use request bodies (not URL params) since both `queue` and `agent` are required.

**Add tier**:
```bash
curl -X POST http://localhost:37274/v1/callcenter/tiers \
  -H "Content-Type: application/json" \
  -H "X-Allowed-Contexts: customer1.example.com" \
  -d '{"queue":"support@customer1.example.com","agent":"a1b2c3d4-e5f6-7890-1234-567890abcdef","level":"1","position":"1"}'
```

**Delete tier**:
```bash
curl -X DELETE http://localhost:37274/v1/callcenter/tiers \
  -H "Content-Type: application/json" \
  -H "X-Allowed-Contexts: customer1.example.com" \
  -d '{"queue":"support@customer1.example.com","agent":"a1b2c3d4-e5f6-7890-1234-567890abcdef"}'
```

Valid tier set keys: `state`, `level`, `position`.

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
├── main.go           # Server initialization and routing
├── handlers.go       # Call control endpoint handlers
├── cc_handlers.go    # Callcenter endpoint handlers (queues, agents, tiers)
├── cc_parser.go      # Pipe-delimited output parser for mod_callcenter
├── cc_types.go       # Callcenter request/response types
├── auth.go           # Context authorization logic
├── middleware.go     # HTTP middleware functions
├── types.go          # Call control request/response structures
├── esl.go            # FreeSWITCH ESL client
├── utils.go          # Validation and logging helpers
├── openapi.yaml      # OpenAPI 3.0 specification
├── go.mod            # Go module definition
├── go.sum            # Go dependencies checksums
├── DEVELOPMENT.md    # Development guide
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

# Build the binary (compiles all .go files)
go build -o fs-api

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

- **Bearer Token Authentication** (v0.3.0+): Configure `FSAPI_AUTH_TOKENS` to require authentication for remote requests. Generate strong, random tokens (recommended: 32+ characters). Localhost requests bypass authentication for convenience.
- **Context-Based Authorization**: Use the `X-Allowed-Contexts` header to restrict API operations by FreeSWITCH context/tenant
- **Network Binding**: The service binds to all interfaces (0.0.0.0) by default - use a reverse proxy or firewall for external access control
- **HTTPS/TLS**: Use a reverse proxy (nginx, Caddy) to add HTTPS encryption for production deployments
- **Rate Limiting**: Implement rate limiting at the reverse proxy level to prevent abuse
- **ESL Password**: Change the default FreeSWITCH ESL password from "ClueCon" in production
- **Token Management**: Rotate authentication tokens periodically and revoke compromised tokens immediately

## License

This implementation follows the FreeSWITCH Call Control API specification v1.0.0.

## Support

For issues or questions:
- Check service logs: `journalctl -u fs-api.service -f`
- Verify FreeSWITCH ESL connectivity
- Review this README for common troubleshooting steps
