package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/OwlShack/meshcore-go/hardware/sx12xx"
	"periph.io/x/conn/v3/physic"
)

// The SPI hat presets, byte-identical to OwlShack's internal/modem/boards.json.
// Keep them in step: the pin maps are the vetted part, and a wrong one gives a
// radio that is silently dead rather than one that errors.
//
//go:embed modem_boards.json
var boardsJSON []byte

// boardsOverrideFile is an optional board list in the working directory,
// merged over the shipped presets by name. It lets an operator add a hat, or
// correct a community pin map, without waiting for a release.
const boardsOverrideFile = "boards.json"

// shippedBoards panics at startup on a bad embedded list: that is a build
// defect, and a test catches it before a binary does.
var shippedBoards = func() map[string]Board {
	b, err := parseBoards(boardsJSON)
	if err != nil {
		panic("shipped modem_boards.json is invalid: " + err.Error())
	}
	return b
}()

// Board is the wiring of one SPI radio hat, chosen by name rather than pin by pin.
type Board struct {
	Name  string
	Label string
	Chip  string
	// SPIPort is the periph port name the hat's chip-select is wired to.
	SPIPort string
	// MaxTxPower is the module's rating in dBm; past it the PA cooks.
	MaxTxPower uint8
	// Verified is "hardware" (run on the physical hat) or "community".
	Verified string
	Notes    string

	opts sx12xx.Opts
}

// boardFile is one entry in the JSON; pins are pointers because 0 is GPIO0, a
// real pin, and must not be confused with "absent".
type boardFile struct {
	Label string `json:"label"`
	Chip  string `json:"chip"`

	BusID int `json:"bus_id"`
	CSID  int `json:"cs_id"`

	CSPin    *int  `json:"cs_pin"`
	ResetPin *int  `json:"reset_pin"`
	BusyPin  *int  `json:"busy_pin"`
	IRQPin   *int  `json:"irq_pin"`
	TxEnPin  *int  `json:"txen_pin"`
	RxEnPin  *int  `json:"rxen_pin"`
	EnPins   []int `json:"en_pins"`
	// EnPin is openHop's singular spelling of a single enable line.
	EnPin    *int `json:"en_pin"`
	TxLedPin *int `json:"txled_pin"`
	RxLedPin *int `json:"rxled_pin"`

	TxPower         *int     `json:"tx_power"`
	UseDIO2RF       *bool    `json:"use_dio2_rf"`
	UseDIO3TCXO     *bool    `json:"use_dio3_tcxo"`
	DIO3TCXOVoltage *float64 `json:"dio3_tcxo_voltage"`
	RxBoostedGain   bool     `json:"rx_boosted_gain"`
	// GPIOChip is rejected outright: periph resolves pins by name on the
	// default chip, so a map naming another chip cannot be driven.
	GPIOChip *int `json:"gpio_chip"`

	Verified string `json:"verified"`
	Notes    string `json:"notes"`
}

func parseBoards(raw []byte) (map[string]Board, error) {
	var doc struct {
		Boards map[string]boardFile `json:"boards"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Boards) == 0 {
		return nil, fmt.Errorf("no boards defined")
	}
	out := make(map[string]Board, len(doc.Boards))
	for name, bf := range doc.Boards {
		b, err := bf.board(name)
		if err != nil {
			return nil, fmt.Errorf("board %q: %w", name, err)
		}
		out[name] = b
	}
	return out, nil
}

func (bf boardFile) board(name string) (Board, error) {
	switch bf.Verified {
	case "hardware", "community":
	default:
		return Board{}, fmt.Errorf("verified must be \"hardware\" or \"community\", got %q", bf.Verified)
	}
	if bf.TxPower == nil || *bf.TxPower <= 0 || *bf.TxPower > 30 {
		return Board{}, fmt.Errorf("tx_power must be set, 1-30 dBm")
	}
	reset, ok := pinName(bf.ResetPin)
	if !ok {
		return Board{}, fmt.Errorf("reset_pin is required")
	}
	busy, ok := pinName(bf.BusyPin)
	if !ok {
		return Board{}, fmt.Errorf("busy_pin is required")
	}
	if bf.GPIOChip != nil && *bf.GPIOChip != 0 {
		return Board{}, fmt.Errorf("gpio_chip %d: pins are resolved by name on the default chip, so this pin map cannot be driven", *bf.GPIOChip)
	}
	tcxo, delay, err := tcxoSetting(bf)
	if err != nil {
		return Board{}, err
	}

	irq, _ := pinName(bf.IRQPin)
	cs, _ := pinName(bf.CSPin)
	txen, _ := pinName(bf.TxEnPin)
	rxen, _ := pinName(bf.RxEnPin)
	txled, _ := pinName(bf.TxLedPin)
	rxled, _ := pinName(bf.RxLedPin)

	pins := slices.Clone(bf.EnPins)
	if bf.EnPin != nil {
		pins = append(pins, *bf.EnPin)
	}
	var enables []string
	for _, p := range pins {
		if n, ok := pinName(&p); ok {
			enables = append(enables, n)
		}
	}

	b := Board{
		Name:       name,
		Label:      bf.Label,
		Chip:       bf.Chip,
		SPIPort:    fmt.Sprintf("SPI%d.%d", bf.BusID, bf.CSID),
		MaxTxPower: uint8(*bf.TxPower),
		Verified:   bf.Verified,
		Notes:      bf.Notes,
		opts: sx12xx.Opts{
			Speed:             8 * physic.MegaHertz,
			ResetPin:          reset,
			BusyPin:           busy,
			Dio1Pin:           irq,
			CSPin:             cs,
			TxEnPin:           txen,
			RxEnPin:           rxen,
			TxLedPin:          txled,
			RxLedPin:          rxled,
			EnablePins:        enables,
			RegulatorMode:     sx12xx.RegulatorDCDCLDO,
			UseDIO2AsRfSwitch: bf.UseDIO2RF != nil && *bf.UseDIO2RF,
			TCXOVoltage:       tcxo,
			TCXODelay:         delay,
			BusyTimeout:       100 * time.Millisecond,
			RxBoostedGain:     bf.RxBoostedGain,
		},
	}
	if b.Label == "" {
		b.Label = name
	}
	return b, nil
}

// tcxoSetting maps the DIO3 TCXO voltage onto the driver's constant.
func tcxoSetting(bf boardFile) (voltage byte, delay time.Duration, err error) {
	if bf.UseDIO3TCXO == nil || !*bf.UseDIO3TCXO {
		return 0, 0, nil
	}
	v := 1.8
	if bf.DIO3TCXOVoltage != nil {
		v = *bf.DIO3TCXOVoltage
	}
	byVolts := map[float64]byte{
		1.6: sx12xx.TCXO1_6V, 1.7: sx12xx.TCXO1_7V, 1.8: sx12xx.TCXO1_8V,
		2.2: sx12xx.TCXO2_2V, 2.4: sx12xx.TCXO2_4V, 2.7: sx12xx.TCXO2_7V,
		3.0: sx12xx.TCXO3_0V, 3.3: sx12xx.TCXO3_3V,
	}
	c, ok := byVolts[v]
	if !ok {
		return 0, 0, fmt.Errorf("dio3_tcxo_voltage %.1f is not one the SX126x can produce", v)
	}
	return c, 10 * time.Millisecond, nil
}

// pinName converts a BCM number to periph's gpioreg name; nil and -1 mean absent.
func pinName(bcm *int) (string, bool) {
	if bcm == nil || *bcm < 0 {
		return "", false
	}
	return fmt.Sprintf("GPIO%d", *bcm), true
}

// lookupBoard finds a board in the shipped presets with boardsOverrideFile
// merged over them. The file is re-read on every modem setup, so an edited pin
// map takes effect on SIGHUP.
func lookupBoard(name string) (Board, error) {
	all := maps.Clone(shippedBoards)
	raw, err := os.ReadFile(boardsOverrideFile)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return Board{}, fmt.Errorf("reading %s: %w", boardsOverrideFile, err)
	default:
		extra, err := parseBoards(raw)
		if err != nil {
			return Board{}, fmt.Errorf("%s: %w", boardsOverrideFile, err)
		}
		maps.Copy(all, extra)
		slog.Info("loaded board overrides", "component", "modem",
			"file", boardsOverrideFile, "boards", slices.Sorted(maps.Keys(extra)))
	}

	b, ok := all[name]
	if !ok {
		return Board{}, fmt.Errorf("unknown spiBoard %q (known: %v)", name, slices.Sorted(maps.Keys(all)))
	}
	return b, nil
}
