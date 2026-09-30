# Changelog

Notable changes per release. Dates are the tag date; unreleased work sits at the
top until tagged.

## Unreleased

Baseline `v1.0.6`. openHop Modems and SPI radio hats join KISS firmware, the MQTT status now uses
the firmware's own names and meanings, and brokers stay connected through outages and token
refreshes.

Config adds the `openhop://` and `spi://` connections and the `spiBoard`, `modemToken` and
`dutyCycle` settings. `nodeType` is no longer needed.

### Added

- **openHop Modems.** Connect with `openhop:///dev/ttyUSB0` over USB, or `openhop://host:port`
  over the network with `modemToken` as its password. The modem's radio is set to MeshCore's
  settings on every connect, and a dropped link reconnects on its own.
- **SPI radio hats on a Raspberry Pi.** Connect with `spi://` and choose the hat by name with
  `spiBoard`, from 15 SX1262 hats, the same list OwlShack has. A `tx` above the hat's rating is
  refused rather than turned down. To add a hat or correct one, put a `boards.json` in the
  directory the bot runs from.
- **Packets the bot sends appear on MQTT.** Each one is published with `"direction": "tx"`, as
  firmware nodes publish theirs. Signal fields are left off, since there is no received signal to
  report.
- **Received packets carry `score`, `duration` and `path`.** `score` and `duration` match the
  firmware's packet log. `path` is the source and destination prefixes, as the firmware prints them,
  not the route.
- **The status reports more of what the firmware does.** `mcu_temp_c`, `last_snr`, `last_rssi`,
  `rx_air_secs`, `tx_air_secs`, `flood_tx` and `direct_tx` are new in `stats`, and `repeat` is new
  at the top level. `mcu_temp_c` is left out on a board without a temperature sensor.
- **The status reports transmit and receive faults.** New counters in `stats` show packets
  retried, dropped or failed, and whether the channel was busy or the bot sent faster than the radio
  could keep up. They count everything this process sends and receives, not one bot.
- **SPI hats and openHop Modems report chip-level errors.** `crc_errors`, `driver_errors` and
  `recv_recoveries` appear in `stats` where the radio can measure them, and are left out on KISS.
- **`dutyCycle` limits how much of the time the radio transmits.** It is a percentage, like the
  firmware's `set dutycycle`, and takes fractions such as `0.1` for EU868 sub-bands. The default is
  unchanged, and the duty cycle in use is logged at startup.
- **Slow message handling is logged and counted.** One slow reply holds up every bot's receiving,
  so any that takes over half a second is logged and counted in `stats.handler_slow`.

### Changed

- **`recv_errors` now means what it means on a firmware node.** It counts packets the radio
  failed to receive. It used to count packets that arrived intact but did not decode, which is now
  `packet_parse_errors`. Firmware nodes publish `recv_errors` to the same brokers, so the two now
  agree.
- **`battery_percent` is replaced by `stats.battery_mv`.** The firmware reports millivolts, and the
  old percentage showed 100% on boards with no battery. `battery_mv` is left out when the board has
  no battery or has stopped answering.
- **`noise_floor` moved into `stats`.** It sits with the other radio readings, as the firmware
  reports it.
- **`stats.packets_received` is now `stats.recv`, with `stats.packets_recv` beside it.** These are
  the names the firmware and CoreScope use. `stats.sent` is published alongside
  `stats.packets_sent`.
- **`SNR` and `RSSI` are left out when a packet arrives without them.** They used to read 0, which
  looked like a real measurement.
- **The connection chooses the radio.** `nodeType` is ignored, so an existing config still loads
  and it can be deleted.

### Fixed

- **A broker that is down at startup is retried.** It used to be dropped until the bot was
  restarted or reloaded. It is now retried with a growing delay, up to 5 minutes.
- **Startup no longer waits for brokers.** Each unreachable broker could hold up startup for 10
  seconds. The bot now starts at once and connects in the background.
- **Shutdown and reload no longer hang on an unresponsive broker.** The last status sent on the way
  down now gives up after 5 seconds rather than waiting forever.
- **A failed token refresh no longer leaves a broker disconnected.** The broker used to stay
  down for 6 minutes or more. It now reconnects straight away.
- **Refreshing a token no longer makes the connection flap.** The old connection is closed before
  it can push the new one off.
- **`packets_sent` and `queue_len` report real values.** Both read 0 on every status since v1.0.6.
- **Every counter in a status is taken at the same moment.** A packet arriving mid-report could
  show `recv: 2` beside a `last_rssi` of 0.
- **Sends that meet a busy radio are retried again.** They had been given up on at the first try.
- **Long packets at slow radio settings are no longer counted as failed.** The bot now waits as
  long as the packet takes on air, rather than a fixed time that the largest packets could outlast.
- **A modem that stops answering no longer shows its last battery and temperature.** They are left
  out once it has been silent for 45 seconds.
- **A packet that failed to send no longer appears on MQTT as sent.**
- **A config mistake on reload keeps the running config.** An unknown connection used to stop the
  bot. It is now refused when the config loads, as is `spi://` without `spiBoard`.

### Upgrading

- **If you read this node's MQTT status, check `recv_errors` first.** It keeps its name but changes
  meaning, and nothing on the wire marks the change. A rate or delta over it will jump at the
  upgrade. Use `packet_parse_errors` if you wanted the old count.
- **Other MQTT fields to check:** `battery_percent` is gone, `noise_floor` has moved into `stats`,
  `stats.packets_received` is now `stats.recv`, and `SNR`, `RSSI`, `battery_mv` and `mcu_temp_c`
  can be absent.
- **Building from source needs Go 1.26.7.**
