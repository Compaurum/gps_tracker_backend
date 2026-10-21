# gps_tracker_backend

AWS backend for GPS trackers. It isn't tied to one kind of hardware: any device that speaks the [device protocol](#device-protocol-v1)
over MQTT/TLS can report to it, and each device reports its hardware model (`hw`) so the backend knows what it is.
The first device is a LilyGO T-Call A7670 (ESP32 + LTE Cat-1 modem; firmware in `../gps_tracker`).

Trackers publish positions over MQTT to AWS IoT Core; an IoT rule hands each message to a Go Lambda,
which validates it and stores it in DynamoDB. Infrastructure is defined with AWS CDK (TypeScript).

```
 tracker (any hardware with an MQTT/TLS client)
     │  MQTT 3.1.1 over TLS 1.2, port 8883, X.509 client cert
     ▼
 AWS IoT Core ── policy: each thing may only use trackers/<its-name>/...
     │  topic rule: SELECT *, topic(2) AS deviceId, topic(3) AS msgType, timestamp() AS receivedAt
     │              FROM 'trackers/+/+' WHERE msgType is telemetry|status
     ▼
 Lambda "ingest" (Go, arm64) ──validate──▶ DynamoDB  Positions (history)
     │                                    DynamoDB  Devices   (latest state)
     │                                    DynamoDB  DeviceHistory (status & battery timeline)
     └── failures (after 2 retries) / rule errors ──▶ SQS IngestDlq (14 days)
```

## Repository layout

| Path | What |
|---|---|
| `bin/gps-tracker.ts` | CDK app entry point |
| `lib/gps-tracker-stack.ts` | The stack: tables, IoT policy and rule, Lambda, DLQ |
| `lib/go-function.ts` | Construct that builds a Go Lambda from `lambda/<name>` |
| `lib/github-oidc-stack.ts` | IAM role GitHub Actions deploys with (deployed by hand, once) |
| `.github/workflows/ci.yml` | Tests on every PR and push, deploys `main` |
| `lambda/` | Go module with all Lambda code |
| `lambda/ingest` | The ingest Lambda (`main` package) |
| `lambda/ingest/protocol` | Wire protocol v1: types and validation (unit tested) |
| `scripts/provision-device.sh` | Registers a tracker and downloads its certificate |

## Usage

Prerequisites: Node.js, Go 1.26 (or Docker — used to build the Lambda when Go isn't installed), AWS CLI with credentials.

```sh
npm install
npm run build          # type-check the CDK app (TypeScript 7)
npm test               # Go unit tests
npx cdk bootstrap      # once per account/region
npx cdk deploy
```

Everything deploys to `ap-southeast-2`. The region is fixed in `bin/gps-tracker.ts` and `scripts/provision-device.sh`.

### Continuous deployment (GitHub Actions)

`.github/workflows/ci.yml` runs on every PR and push: type-check, Go tests and vet, then `cdk synth`.
On `main` it then deploys the `GpsTracker` stack. CI authenticates with **OIDC**: GitHub issues a short-lived
token that AWS exchanges for the `GpsTrackerGithubDeploy` role, so no AWS keys are stored in GitHub.
That role can only assume the CDK bootstrap roles. It trusts only jobs from this repository running in the GitHub
environment `production`.

One-time setup:

1. Deploy the OIDC stack from your machine. CI never deploys this stack, so it can't change its own access:
   ```sh
   npx cdk bootstrap
   npx cdk deploy GpsTrackerGithubOidc   # prints DeployRoleArn
   ```
   If the account already has a GitHub OIDC provider (from another project), the deploy fails. In that case,
   import the existing provider in `lib/github-oidc-stack.ts` instead of creating a new one.
2. In GitHub, open **Settings → Environments → New environment** and create `production`.
   Under *Deployment branches*, allow only `main`. Optionally, add yourself as a required reviewer so each deploy waits for your approval.
3. In **Settings → Secrets and variables → Actions → Variables**, add `AWS_DEPLOY_ROLE_ARN` = the `DeployRoleArn` output.
   It's a variable, not a secret: the ARN isn't sensitive.

Register a tracker (after the stack is deployed — it owns the device policy):

```sh
scripts/provision-device.sh tracker-01
```

This creates the IoT thing, an active certificate with the device policy attached, and writes
`certs/tracker-01/{device.pem.crt,private.pem.key,AmazonRootCA1.pem}` (git-ignored). It prints the
account's ATS endpoint. The private key exists only in that directory — keep it safe.

Simulate a tracker without hardware (your IAM credentials bypass the device policy):

```sh
aws iot-data publish --topic trackers/tracker-01/telemetry --cli-binary-format raw-in-base64-out \
  --payload "{\"v\":1,\"ts\":$(date +%s)000,\"seq\":1,\"fixes\":[{\"ts\":$(date +%s)000,\"lat\":-32.9283,\"lon\":151.7817,\"sat\":8}]}"
```

## Decisions

### 1. AWS IoT Core as the device entry point

Chosen over API Gateway (HTTPS) and a self-hosted MQTT broker.

- **Mutual TLS with per-device X.509 certificates** gives each tracker its own identity, and a stolen device can be revoked on its own. No shared API keys in firmware.
- **Persistent MQTT connection**: after the TLS handshake (the expensive part on a cellular modem), each message costs only a few bytes of protocol overhead, and the server can push commands down to the device.
- **Serverless**: nothing to patch or scale; at hobby volume it costs cents a month.
- **Trade-off**: AWS lock-in, and certificate handling on the device is more work than an HTTP API key.

### 2. MQTT over TLS, from any hardware

The backend only cares about the protocol, not the device. A device needs:

- MQTT 3.1.1, port 8883, TLS 1.2 with its X.509 client certificate, server verified against Amazon Root CA 1.
- **Client ID must equal the thing name**: the IoT policy refuses any other.
- Keep-alive 60–300 s. Use QoS 1 for telemetry and status (QoS 2 is not supported by IoT Core).
- Rejected: MQTT over WebSockets on 443 (more overhead, SigV4 signing on the device) and HTTPS
  (a TLS handshake per request costs far more data and power than one long-lived connection).

Example, LilyGO T-Call A7670: the SIMCom A7670 modem has its own MQTT(S) client (`AT+CMQTT*`, with TLS via
`AT+CSSLCFG`), so the ESP32 doesn't need to run TLS or an MQTT library itself. Certificates are uploaded to the
modem's filesystem once (`AT+CCERTDOWN`). LilyGO's `LilyGo-Modem-Series` repository has MQTTS examples for it.
A device with Wi-Fi or a different modem would use its own MQTT/TLS client instead.

### 3. JSON payloads with short keys

Chosen over binary formats (Protobuf, CBOR, packed structs).

- IoT Core rules and CloudWatch logs read JSON directly, and it's easy to debug from a serial console or `aws iot-data publish`.
- Short keys (`v`, `seq`, `ts`, `alt`, `spd`, `crs`, `sat`, `batV`, `fw`) plus **batching** several fixes per message keep payloads small.
  The same names are used in DynamoDB. A 10-fix batch with every optional field is about 1.2 KB, under IoT Core's 5 KB billing increment, so it's billed as one message.
- If cellular data cost ever matters, a binary encoding can come in as protocol `v: 2` on the same topics.

### 4. IoT rule → Lambda → DynamoDB

Chosen over the rule's built-in DynamoDBv2 action.

- The direct action writes one item per message and can't split a batch of fixes, validate input, or keep a
  "latest position" that only moves forward. A small Lambda does all three.
- **Invalid payloads are logged and dropped**, because retrying them can't succeed. Infrastructure errors are returned, so Lambda retries
  (2×) and then sends the event to the **SQS DLQ**. The rule's error action sends to the same queue when IoT Core
  can't invoke the Lambda at all.

### 5. Go for Lambdas, TypeScript for CDK

- **Go Lambdas**: static binary on `provided.al2023`, arm64 (Graviton is cheaper), roughly 10–20 ms cold start, runs fine in 128 MB.
  All handlers live in one Go module under `lambda/`, one folder per function (`lambda/<name>/`, holding its `main` package and its own sub-packages). Code needed by more than one function goes in `lambda/internal/`.
  `lib/go-function.ts` builds them during `cdk synth` with `go build`, or in a `golang` Docker image when Go isn't installed.
- **TypeScript CDK**: CDK's native language, with the most examples and the newest constructs first. It runs through `tsx`
  (esbuild), so it doesn't depend on the TypeScript compiler API, and `tsc` (TypeScript 7) is used only for type-checking.

### 6. DynamoDB data model

Three on-demand tables (no capacity planning; idle cost is storage only). Point-in-time recovery is on and removal policy is RETAIN.

All timestamps are UTC epoch **milliseconds**. `ts` is the device's time (when something happened);
`receivedAt` is IoT Core's time (when the message arrived).

**Positions**: one item per fix, kept forever (no TTL).

| Attribute | Type | Notes |
|---|---|---|
| `deviceId` | S, partition key | thing name |
| `ts` | N, sort key | fix time (from the device) |
| `lat`, `lon` | N | WGS84 decimal degrees |
| `alt`, `spd`, `crs`, `sat`, `hdop` | N | optional, see protocol |
| `seq` | N | sequence number of the message that carried the fix, for spotting gaps |
| `receivedAt` | N | when IoT Core received the message |

- "Track for device X between A and B" is a single `Query` on `deviceId = X AND ts BETWEEN A AND B`.
- **Idempotent**: QoS 1 redeliveries and re-sent backlogs overwrite the same `(deviceId, ts)` item.

**Devices**: one item per tracker (`deviceId` partition key) holding its latest state.
IoT Core invokes the Lambda asynchronously, so messages can be processed out of order and retries can arrive late.
To stop older data overwriting newer data, fields are updated in groups. Each group has its own version attribute and is
written only if `version < :new`:

| Group | Attributes | Version attribute | Ordered by |
|---|---|---|---|
| Connectivity | — | `lastSeenAt` | IoT Core receive time |
| Health | `batV`, `rssi` | `healthAt` | device time (only messages that carry them) |
| Event | `lastEvent`, `fw`, `hw`, `uptime` | `lastEventAt` | device time |
| Position | `lastFix` | `lastFixTs` | fix time, so a late backlog never moves the position backwards |

Separate groups keep a message that's stale for one group (e.g. battery) from being dropped for another it's newest
for (e.g. its `boot` event). Measurements are ordered by device time, when they were true; `lastSeenAt` tracks
connectivity, so it uses the receive time.

**DeviceHistory**: the device's status and health timeline, kept forever (no TTL). `Devices` only holds the latest values;
this table keeps every one, for battery curves and past events.

| Attribute | Type | Notes |
|---|---|---|
| `deviceId` | S, partition key | thing name |
| `receivedAt` | N, sort key | when IoT Core received the message (unique per device, unlike device time) |
| `ts` | N | device time the message was created |
| `type` | S | `status` (every status message) or `telemetry` (telemetry messages that report `batV`/`rssi`) |
| `event`, `fw`, `hw`, `uptime` | S / S / S / N | status messages only |
| `batV`, `rssi` | N | when reported |
| `seq` | N | telemetry only |

- "Battery over the last week" is one `Query` on `deviceId = X AND receivedAt > :weekAgo`; plot it against `ts`.
- Lambda retries rewrite the same row. An MQTT QoS 1 redelivery gets a new receive time, so in rare cases a message appears twice.

### 7. Device provisioning by script, not CDK

CDK owns the shared device policy. Things and certificates are created by `scripts/provision-device.sh`
because the private key must reach the device and must not end up in CloudFormation state.
Fleet provisioning (claim certificates) can replace this if there are ever many devices.

## Device protocol (v1)

### Topics

`<id>` is the IoT thing name (`[a-zA-Z0-9_-]{1,64}`), which is also the MQTT client ID.

| Topic | Direction | QoS | Purpose |
|---|---|---|---|
| `trackers/<id>/telemetry` | device → cloud | 1 | GPS fixes (batched) |
| `trackers/<id>/status` | device → cloud | 1 | Lifecycle and health events |
| `trackers/<id>/cmd` | cloud → device | 1 | Commands. **Reserved**: the device may subscribe; nothing publishes yet |

The IoT policy only allows each device to publish to, and subscribe on, its own three topics.
The backend takes `deviceId` from the topic, so it's never part of the payload.

### Common rules

- UTF-8 JSON object. Every message has `"v": 1` and `ts`; a message missing either is rejected.
- Unknown fields are ignored, so new optional fields can be added to v1 without breaking older backends.
  A breaking change means a new `v`.
- **Timestamps are UTC epoch milliseconds.** `ts` is the device's clock: GPS time, or network/NTP time before
  the first fix (on the A7670, the modem's clock via `AT+CCLK`). A timestamp before 2020 or more than 5 minutes in the future is treated as
  "clock not set". IoT Core adds its own `receivedAt`.
- Optional fields are left out rather than sent as `null`.

### `telemetry`

```json
{
  "v": 1,
  "ts": 1760950001000,
  "seq": 1042,
  "fixes": [
    {"ts": 1760949990000, "lat": -32.928312, "lon": 151.781705, "alt": 12.3, "spd": 4.2, "crs": 270.0, "sat": 9, "hdop": 0.9},
    {"ts": 1760950000000, "lat": -32.928401, "lon": 151.781822}
  ],
  "batV": 3.98,
  "rssi": -71
}
```

| Field | Req | Type | Meaning |
|---|---|---|---|
| `ts` | yes | int | When the message was created (and `batV`/`rssi` measured) |
| `seq` | no | int | Message counter, increments per publish, wraps / resets on boot |
| `fixes` | yes | array, 1–100 | Oldest first |
| `fixes[].ts` | yes | int | Fix time |
| `fixes[].lat`, `lon` | yes | number | WGS84 decimal degrees, 6 decimals (~0.1 m) is plenty |
| `fixes[].alt` | no | number | Metres above mean sea level |
| `fixes[].spd` | no | number | Ground speed, km/h |
| `fixes[].crs` | no | number | Course over ground, degrees 0–360 |
| `fixes[].sat` | no | int | Satellites used in the fix |
| `fixes[].hdop` | no | number | Horizontal dilution of precision (lower is better; roughly × 3–5 m of error) |
| `batV` | no | number | Battery voltage, volts |
| `rssi` | no | number | Cellular signal strength, dBm |

The backend drops individual fixes that are implausible:
- `ts` missing, before 2020 or more than 5 minutes in the future
- latitude or longitude out of range
- exactly `0,0` (what many receivers report before they have a fix)
- wrong types

The rest of the message is still stored.

### `status`

```json
{"v": 1, "ts": 1760950001000, "event": "boot", "fw": "0.1.0", "hw": "lilygo-t-call-a7670", "batV": 4.12, "rssi": -69, "uptime": 4}
```

| Field | Req | Type | Meaning |
|---|---|---|---|
| `ts` | yes | int | When the event happened |
| `event` | yes | string | `boot`, `heartbeat`, `low_battery`, `shutdown` |
| `fw` | no | string | Firmware version |
| `hw` | no | string | Hardware model, e.g. `lilygo-t-call-a7670`. Lowercase, dash-separated, stable per device type. Send it on `boot` |
| `batV`, `rssi` | no | number | As in telemetry |
| `uptime` | no | int | Seconds since boot |

### `cmd` (reserved)

Planned shape, so the firmware can subscribe early and ignore what it doesn't understand:

```json
{"v": 1, "commandId": "c0ffee01", "command": "set_interval", "arguments": {"sampleIntervalSeconds": 10, "publishIntervalSeconds": 60}}
```

### Recommended device behaviour

- Sample GPS every 10 s while moving and publish a batch every 60 s. While stationary, send a `heartbeat` status every 15 minutes instead of telemetry.
- Publish a `boot` status, with `fw` and `hw`, as soon as the connection comes up.
- **Store-and-forward**: buffer unsent fixes in RAM/PSRAM while offline, then send the backlog oldest first, up to 100 fixes per
  message, after reconnecting. Resending is safe: storage is keyed on `(deviceId, ts)`.
- Treat a message as delivered only after its PUBACK.
