# meshcore-bot

A configurable bot framework for [MeshCore](https://github.com/meshcore-dev/MeshCore) mesh networks, built with the pure Go [meshcore-go](https://github.com/meshcore-go/meshcore-go) library.

## Features

- **Trigger-based architecture**: Respond to group messages, private channel messages, or on a cron schedule.
- **Go template responses**: Access mesh data like sender, hops, path hashes, SNR, RSSI, and more.
- **Three kinds of radio**: MeshCore KISS firmware over USB or TCP, [openHop Modem](https://github.com/openhop-dev) firmware over USB or TCP, or a bare SX1262 hat on a Raspberry Pi's SPI bus. The same modems [OwlShack](https://github.com/meshcore-go/OwlShack) supports, publishing the same MQTT schema.
- **Private channel support**: Join private channels using a hex-encoded PSK.
- **MQTT integration**: Publish observed mesh traffic to MQTT brokers (e.g. [LetsMesh](https://letsmesh.net), [CoreScope](https://github.com/Kpa-clawbot/CoreScope)).
- **Hot-reload**: Reload configuration via `SIGHUP` without restarting. Reconnects the modem if connection settings change.
- **Multi-bot support**: Run multiple bots within a single instance.
- **Flexible configuration**: Supports TOML, YAML, and JSON formats.

## Installation

### Download a Release Binary (Recommended)

Pre-built binaries are available for Linux, macOS, and Windows on the [Releases](https://github.com/meshcore-go/meshcore-bot/releases) page.

1. Go to the [latest release](https://github.com/meshcore-go/meshcore-bot/releases/latest).
2. Download the binary for your platform (e.g. `meshcore-bot-linux-arm64` for a Raspberry Pi).
3. Make it executable and move it to a folder of your choosing:

```bash
chmod +x meshcore-bot-linux-arm64
sudo mv meshcore-bot-linux-arm64 /usr/local/bin/meshcore-bot
```

### Docker

Images are published to `ghcr.io/meshcore-go/meshcore-bot` for the following platforms:
`linux/386`, `linux/amd64`, `linux/arm/v6`, `linux/arm/v7`, `linux/arm64/v8`, `linux/ppc64le`, `linux/riscv64`, `linux/s390x`.

```bash
docker pull ghcr.io/meshcore-go/meshcore-bot:latest
```

### Build from Source

Requires Go 1.26.1+.

```bash
git clone https://github.com/meshcore-go/meshcore-bot.git
cd meshcore-bot
go build -o meshcore-bot
```

## Running

The bot looks for `config.toml`, `config.yaml`, `config.yml`, or `config.json` in the current directory.

### Step 1: Plug in your radio

Connect your MeshCore radio to your computer via USB. On Linux it usually shows up as `/dev/ttyACM0`. On macOS it's something like `/dev/cu.usbmodem*`.

On Linux, your user needs permission to access serial devices. Add yourself to the `dialout` group:

```bash
sudo usermod -a -G dialout $USER
```

Log out and back in (or reboot) for the change to take effect.

### Step 2: Create a config file

Create a file called `config.toml` in the same folder as the binary. Here's a minimal example that responds to "ping" on the `#testing` channel:

```toml
connection = "serial:///dev/ttyACM0"

freq = 917.375
bw = 62.50
sf = 7
cr = 8
tx = 2

[[bot]]
name = "My Bot"

[[bot.trigger]]
type = "channel"
template = "Pong! Hello {{.Sender}}"
channels = ["#testing"]
match = ["(?i)^ping"]
```

### Step 3: Run it

```bash
./meshcore-bot
```

That's it. The bot will connect to your radio, join the `#testing` channel, and reply "Pong! Hello \<sender\>" whenever someone sends a message starting with "ping".

### Using Docker

Mount your config file and pass through the serial device:

```bash
docker run -d \
  --device /dev/ttyACM0 \
  -v ./config.toml:/data/config.toml \
  ghcr.io/meshcore-go/meshcore-bot
```

For TCP connections (e.g. a serial-to-TCP bridge to the KISS device), no `--device` is needed:

```bash
docker run -d \
  -v ./config.toml:/data/config.toml \
  ghcr.io/meshcore-go/meshcore-bot
```

Pass CLI flags directly:

```bash
docker run -d \
  --device /dev/ttyACM0 \
  -v ./my-config.toml:/my-config.toml \
  ghcr.io/meshcore-go/meshcore-bot \
  meshcore-bot -c /my-config.toml -vvv
```

## Configuration Reference

### Connection

| Field | Description | Default |
|-------|-------------|---------|
| `connection` | Which radio, chosen by scheme. See [Radios](#radios) below. | `serial:///dev/ttyACM0` |
| `baudRate` | Serial baud rate for KISS. Ignored for openHop, whose firmware is fixed at 921600. | `115200` |
| `spiBoard` | The SPI hat, by name. Required for `spi://`. | |
| `modemToken` | Password for an openHop modem over TCP. Kept out of `connection` so it is not logged with it. | |
| `nodeType` | Ignored. Kept so older configs still load; the `connection` scheme picks the radio. | |
| `logLevel` | Log level: `debug`, `info`, `warn`, `error`, `trace` (overridden by `-v` flags) | `info` |

### Radio Settings

| Field | Description | Default |
|-------|-------------|---------|
| `freq` | Frequency in MHz | `917.375` |
| `bw` | Bandwidth in kHz | `62.50` |
| `sf` | Spreading Factor | `7` |
| `cr` | Coding Rate | `8` |
| `tx` | TX Power | `2` |
| `dutyCycle` | Transmit duty cycle as a percentage, greater than `0` and up to `100`. Named after the firmware's `set dutycycle`, but fractions are allowed: `0.1` is valid, which firmware's own knob rejects. Check your region's limit: EU868 sub-bands are capped at `1` or `0.1`, while NZ/AU 915-928 MHz has no duty-cycle limit. Applies process-wide (one radio mux serves every bot and the observer). | `50` |

### Radios

The `connection` scheme picks the radio:

| Connection | Radio |
|---|---|
| `serial:///dev/ttyACM0`, `tcp://host:port` | MeshCore KISS modem firmware |
| `openhop:///dev/ttyUSB0`, `openhop://host:port` | openHop Modem firmware. Over TCP, set `modemToken` if the modem has one. The driver reconnects on its own if the link drops. |
| `spi://` | A bare SX1262 on this host's SPI bus, with no firmware in front of it: the bot is the radio stack. Needs `spiBoard`. |

**SPI hats.** The pins come from a named preset, because a wrong pin gives a radio that is silently dead rather than one that errors. `spi://` on its own uses the board's own SPI device; `spi://SPI0.1` or `spi:///dev/spidev0.1` overrides it, with a warning if it disagrees with the board. `tx` is checked against the module's rating and refused above it rather than clamped. Boards marked `community` log a warning at startup: their pin maps come from other projects and have not been run on the hat here.

| `spiBoard` | Hat | Max TX | Pins |
|---|---|---|---|
| `bq-station-g3` | BQ Voyage Station G3 | 19 dBm | community |
| `meshadv` | MeshAdv | 22 dBm | community |
| `nebra-duo-hat` | NebraDuo-E22P-1W | 18 dBm | community |
| `nebrahat` | NebraHat-2W | 8 dBm | community |
| `pimesh-1w-v1` | PiMesh-1W (V1) | 18 dBm | community |
| `pimesh-1w-v2` | PiMesh-1W (V2) | 18 dBm | community |
| `rak6421-13300x-slot1` | RAK6421 with RAK1330x, IO slot 1 | 22 dBm | hardware |
| `rak6421-13300x-slot2` | RAK6421 with RAK1330x, IO slot 2 | 22 dBm | hardware |
| `uconsole-aio-v2` | uConsole LoRa Module aio v2 | 22 dBm | community |
| `ultrapeaterzero-e22` | Zindello UltraPeaterZero (E22, 1 W) | 22 dBm | hardware |
| `ultrapeaterzero-e22p` | Zindello UltraPeaterZero (E22P, 1 W) | 22 dBm | hardware |
| `waveshare` | Waveshare LoRa HAT | 22 dBm | community |
| `zebra` | ZebraHat-1W | 18 dBm | community |
| `zebra-duo-hat-r0` | ZebraHatDuo-R0-1W | 18 dBm | community |
| `zebra-duo-hat-r1` | ZebraHatDuo-R1-1W | 18 dBm | community |

To add a hat, or correct a pin map without waiting for a release, put a `boards.json` in the working directory. Entries are merged over the presets by name and re-read on `SIGHUP`. The format is the one in [modem_boards.json](modem_boards.json).

On Raspberry Pi OS, enable SPI (`sudo raspi-config`, Interface Options) and add your user to the `spi` and `gpio` groups. SPI has not been tested under Docker; run the binary on the Pi directly.

```toml
connection = "spi://"
spiBoard = "rak6421-13300x-slot1"
```

### Triggers

Each bot has a `name` and an array of `triggers`. Every trigger has a `type` and a `template`.

#### Channels

The `channels` field accepts either plain channel names (for public/hashtag channels) or objects with a `privateKey` for private channels:

```toml
# Public channels — just use the name
channels = ["#general", "#testing"]

# Private channels — use the [[bot.trigger.channels]] syntax
[[bot.trigger.channels]]
name = "Secret Ops"
privateKey = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"

# Mix of Private channels — use the [[bot.trigger.channels]] syntax
[[bot.trigger.channels]]
name = "Secret Ops"
privateKey = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"

[[bot.trigger.channels]]
name = "#general"

[[bot.trigger.channels]]
name = "#testing"
```

#### Channel Trigger (`type = "channel"`)

Fires when a message is received on a channel that matches one of the `match` patterns.

| Field | Description |
|-------|-------------|
| `channels` | Channels to listen on |
| `match` | Array of [Go regular expressions](https://pkg.go.dev/regexp/syntax) to match against incoming messages |
| `template` | Go text/template for the response |
| `retryTimeout` | Seconds to wait for a repeater echo before retrying | `5` |
| `maxRetries` | Total sends, counting the first: `3` sends once and resends up to twice | `3` |
| `charLimitBehaviour` | What to do when a message exceeds the character limit: `"truncate"` or `"split"` | — |
| `pathHashSize` | Path hash size: `0` = copy sender's setting, `1`/`2`/`3` = bytes per hash | `1` |

#### Cron Trigger (`type = "cron"`)

Fires on a schedule.

| Field | Description |
|-------|-------------|
| `schedule` | Cron expression (e.g. `"*/5 * * * *"`) |
| `channels` | Channels to send the message to |
| `template` | Go text/template for the message |
| `retryTimeout` | Seconds to wait for a repeater echo before retrying | `5` |
| `maxRetries` | Total sends, counting the first: `3` sends once and resends up to twice | `3` |
| `charLimitBehaviour` | What to do when a message exceeds the character limit: `"truncate"` or `"split"` | — |
| `pathHashSize` | Path hash size: `0` = copy sender's setting, `1`/`2`/`3` = bytes per hash | `1` |

After sending a message, the bot listens for the message to be repeated back by a repeater. If no echo is heard within `retryTimeout` seconds, the message is sent again, until it has been sent `maxRetries` times in all. This applies to both channel and cron triggers.

### Template Variables

**Channel Trigger:**
- `{{.Sender}}` — Sender's node name
- `{{.Channel}}` — Channel name
- `{{.Message}}` — Original message text
- `{{.Match}}` — Map of named regex capture groups
- `{{.Timestamp}}` — Message timestamp
- `{{.SNR}}` — Signal-to-Noise Ratio
- `{{.RSSI}}` — Received Signal Strength Indicator
- `{{.Hops}}` — Number of hops
- `{{.PathHashes}}` — Raw path hashes
- `{{.PathHashSize}}` — Size of path hashes

**Cron Trigger:**
- `{{.Time}}` — Current time
- `{{.Schedule}}` — The cron schedule string

**Built-in Functions:**
- `formatPathBytes` — Formats raw path hashes into a readable string.

## Example Configs

### KISS Node with Private Channel

```toml
connection = "serial:///dev/ttyACM0"
baudRate = 115200

freq = 917.375
bw = 62.50
sf = 7
cr = 8
tx = 22

[[bot]]
name = "Ping Bot"

[[bot.trigger]]
type = "channel"
template = "@[{{.Sender}}] 🦈={{.PathHashSize}} 🦘={{.Hops}} 🛣️={{.PathHashes | formatPathBytes}}"
match = ["(?i)^test", "(?i)^ping"]

[[bot.trigger.channels]]
name = "MyPrivateChannel"
privateKey = "7d78eab105a663ab3504d99a0e5b1891"
```

### Cron Trigger

```toml
[[bot.trigger]]
type = "cron"
schedule = "*/5 * * * *"
channels = ["#testing"]
template = "Periodic update: The time is {{.Time}}"
```

## CLI Flags

| Flag | Description |
|------|-------------|
| `-c, --config PATH` | Path to the configuration file |
| `-V, --version` | Print version and exit |
| `-v, --verbose` | Enable verbose debug logging |

## MQTT Integration

meshcore-bot can publish observed mesh traffic to MQTT brokers. This is used by services like [LetsMesh](https://letsmesh.net) to aggregate mesh network data.

MQTT lives under exactly one bot: define an optional `[bot.mqtt]` section on a single `[[bot]]`. At most one bot in the whole config may have it. A unique identity key file is used for authentication.

Configs from before this change (root-level `[[observer]]` blocks) are migrated automatically on first load: observers merge into the first bot's `[bot.mqtt]`, and the original file is backed up alongside as `config.toml.bak-<timestamp>` (the rewrite loses comments). A config with both formats fails loudly instead of guessing.

```toml
[[bot]]
name = "AKL Bot"

[bot.mqtt]
name = "AKL Observer"
iataCode = "AKL"
keyFile = "mqtt_identity.key"
statusInterval = 300

[bot.mqtt.advert]
enabled = true
interval = 86400
lat = -36.8485
lon = 174.7633

[[bot.mqtt.broker]]
name = "US West (LetsMesh v1)"
enabled = true
transport = "wss"
host = "mqtt-us-v1.letsmesh.net"
port = 443
topicPrefix = "meshcore"
retainStatus = false
tlsEnabled = true
authType = "token"
audience = "mqtt-us-v1.letsmesh.net"

[[bot.mqtt.broker]]
name = "Europe (LetsMesh v1)"
enabled = true
transport = "wss"
host = "mqtt-eu-v1.letsmesh.net"
port = 443
topicPrefix = "meshcore"
retainStatus = false
tlsEnabled = true
authType = "token"
audience = "mqtt-eu-v1.letsmesh.net"

[[bot.mqtt.broker]]
name = "CoreScope NZ"
enabled = true
transport = "wss"
host = "meshcore-mqtt-1.baird.io"
port = 443
topicPrefix = "meshcore"
retainStatus = false
tlsEnabled = true
authType = "token"
audience = "meshcore-mqtt-1.baird.io"
```

| MQTT Field | Description |
|----------------|-------------|
| `name` | Display name for this observer |
| `iataCode` | Location identifier (e.g. airport code) |
| `keyFile` | Path to the identity key file (created automatically if missing) |
| `statusInterval` | Seconds between status publishes (default: 300) |
| `owner` | Owner name included in MQTT token claims (optional) |
| `email` | Email included in MQTT token claims (optional) |

| Broker Field | Description |
|--------------|-------------|
| `name` | Display name for this broker |
| `enabled` | Enable/disable this broker |
| `dedup` | Enable per-broker packet deduplication (default: `false`) |
| `transport` | `"wss"` (WebSocket Secure) or `"tcp"` |
| `host` | Broker hostname |
| `port` | Broker port |
| `path` | WebSocket path (default: none) |
| `topicPrefix` | MQTT topic prefix |
| `disallowedPacketTypes` | Packet types to exclude (e.g. `["ack", "advert"]`) |
| `retainStatus` | Retain status messages on the broker |
| `tlsEnabled` | Enable TLS |
| `tlsInsecure` | Skip TLS certificate verification |
| `authType` | `"token"`, `"basic"`, or `"none"` |
| `username` | Username for basic auth |
| `password` | Password for basic auth |
| `audience` | Token audience (for token auth) |

| Advert Field | Description |
|--------------|-------------|
| `enabled` | Enable periodic advert broadcasting |
| `interval` | Seconds between adverts (default: `86400` / once per day) |
| `lat` | Latitude in decimal degrees (optional) |
| `lon` | Longitude in decimal degrees (optional) |

When enabled, the observer broadcasts a signed chat-node advert over the mesh on startup and then repeats at the configured interval. This allows the node to appear in the mesh network as a visible participant. If `lat` and `lon` are provided, the advert includes location data.

## Hot Reload

Send a `SIGHUP` signal to the process to reload the configuration without restarting:

```bash
kill -SIGHUP $(pgrep meshcore-bot)
```

If the connection settings, radio parameters, `spiBoard` or `modemToken` change, the bot reconnects the radio.

## License

See [LICENSE](LICENSE).
