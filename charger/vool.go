package charger

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/bits"
	"strings"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/api/implement"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
)

// Vool charger implementation
type Vool struct {
	implement.Caps
	log     *util.Logger
	conn    *modbus.Connection
	enabled bool
	phase   uint16 // Netzphase für einphasiges Laden: 1..3
}

// VOOL Modbus registers (all holding registers, big-endian)
const (
	voolRegEvent        = 99
	voolRegChargerState = 100
	voolRegCurrents     = 102 // 102, 103, 104 (Eingang)
	voolRegVoltages     = 105 // 105, 106, 107 (Eingang)
	voolRegPower        = 108
	voolRegEnergy       = 200 // 200 (MSB), 201 (LSB)
	voolRegCommand      = 500
	voolRegCurrentLimit = 501
	voolRegPhases       = 502
	voolRegAuthorizeID  = 1100 // 1100..1117, 18 Register
)

const (
	voolCmdStart = 1
	voolCmdStop  = 2
)

func init() {
	registry.AddCtx("vool", NewVoolFromConfig)
}

// NewVoolFromConfig creates a Vool charger from generic config
func NewVoolFromConfig(ctx context.Context, other map[string]any) (api.Charger, error) {
	cc := struct {
		modbus.TcpSettings `mapstructure:",squash"`
		Phase              string
	}{
		TcpSettings: modbus.TcpSettings{ID: 255},
		Phase:       "L1",
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	phase, err := voolPhase(cc.Phase)
	if err != nil {
		return nil, err
	}

	return NewVool(ctx, cc.TcpSettings, phase)
}

// voolPhase converts the configured grid phase (L1, L2, L3 or 1, 2, 3) to a number
func voolPhase(s string) (int, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "", "L1", "1":
		return 1, nil
	case "L2", "2":
		return 2, nil
	case "L3", "3":
		return 3, nil
	default:
		return 0, fmt.Errorf("invalid phase %q: must be L1, L2 or L3", s)
	}
}

// NewVool creates Vool charger
func NewVool(ctx context.Context, settings modbus.TcpSettings, phase int) (api.Charger, error) {
	conn, err := settings.Connection(ctx)
	if err != nil {
		return nil, err
	}

	log := util.NewLogger("vool")
	conn.Logger(log.TRACE)

	wb := &Vool{
		Caps:    implement.New(),
		log:     log,
		conn:    conn,
		enabled: true,
		phase:   uint16(phase),
	}

	// Phasenumschaltung nur anbieten, wenn alle drei Netzphasen anliegen
	_, v2, v3, err := wb.Voltages()
	if err != nil {
		return nil, err
	}

	if v2 != 0 && v3 != 0 {
		wb.log.DEBUG.Println("detected 3p supply, phase switching available")
		implement.Has(wb, implement.PhaseSwitcher(wb.phases1p3p))
		implement.Has(wb, implement.PhaseGetter(wb.getPhases))
	} else {
		wb.log.DEBUG.Println("detected 1p supply")
	}

	return wb, nil
}

// readUint16 reads one holding register as unsigned 16 bit
func (wb *Vool) readUint16(reg uint16) (uint16, error) {
	b, err := wb.conn.ReadHoldingRegisters(reg, 1)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

// writeUint16 writes one holding register
func (wb *Vool) writeUint16(reg, val uint16) error {
	_, err := wb.conn.WriteSingleRegister(reg, val)
	return err
}

var _ api.Charger = (*Vool)(nil)

// Status implements the api.Charger interface
func (wb *Vool) Status() (api.ChargeStatus, error) {
	s, err := wb.readUint16(voolRegChargerState)
	if err != nil {
		return api.StatusNone, err
	}

	switch s {
	case 1, 7: // AVAILABLE, RESERVED
		return api.StatusA, nil
	case 2, 4, 5, 6: // PREPARING, SUSPENDED_EV, SUSPENDED_EVSE, FINISHING
		return api.StatusB, nil
	case 3, 10: // CHARGING, STARTING_CHARGING
		return api.StatusC, nil
	case 8, 9: // UNAVAILABLE, FAULTED
		return api.StatusNone, fmt.Errorf("charger not available: state %d", s)
	default: // 0 = UNDEFINED und unbekannte Werte
		return api.StatusNone, fmt.Errorf("invalid status: %d", s)
	}
}

// Enabled implements the api.Charger interface
func (wb *Vool) Enabled() (bool, error) {
	state, err := wb.readUint16(voolRegChargerState)
	if err != nil {
		return false, err
	}

	// Box hat selbst gestoppt: Ladung ist nicht freigegeben
	if state == 5 { // SUSPENDED_EVSE
		return false, nil
	}

	return wb.enabled, nil
}

// Enable implements the api.Charger interface
func (wb *Vool) Enable(enable bool) error {
	cmd := uint16(voolCmdStop)
	if enable {
		cmd = voolCmdStart
	}

	if err := wb.writeUint16(voolRegCommand, cmd); err != nil {
		return err
	}

	wb.enabled = enable
	return nil
}

// MaxCurrent implements the api.Charger interface
func (wb *Vool) MaxCurrent(current int64) error {
	return wb.MaxCurrentMillis(float64(current))
}

var _ api.ChargerEx = (*Vool)(nil)

// MaxCurrentMillis implements the api.ChargerEx interface
func (wb *Vool) MaxCurrentMillis(current float64) error {
	if current < 6 {
		return fmt.Errorf("invalid current %.1f", current)
	}

	// Einheit 0.01 A: 16 A -> 1600
	return wb.writeUint16(voolRegCurrentLimit, uint16(current*100))
}

var _ api.CurrentGetter = (*Vool)(nil)

// GetMaxCurrent implements the api.CurrentGetter interface
func (wb *Vool) GetMaxCurrent() (float64, error) {
	u, err := wb.readUint16(voolRegCurrentLimit)
	if err != nil {
		return 0, err
	}

	return float64(u) / 100, nil
}

var _ api.Meter = (*Vool)(nil)

// CurrentPower implements the api.Meter interface
func (wb *Vool) CurrentPower() (float64, error) {
	b, err := wb.conn.ReadHoldingRegisters(voolRegPower, 1)
	if err != nil {
		return 0, err
	}

	// int16, Einheit 0.01 kW -> W
	return float64(int16(binary.BigEndian.Uint16(b))) * 10, nil
}

var _ api.MeterEnergy = (*Vool)(nil)

// TotalEnergy implements the api.MeterEnergy interface
func (wb *Vool) TotalEnergy() (float64, error) {
	b, err := wb.conn.ReadHoldingRegisters(voolRegEnergy, 2)
	if err != nil {
		return 0, err
	}

	// uint32, Wh -> kWh
	return float64(binary.BigEndian.Uint32(b)) / 1e3, nil
}

// getPhaseValues returns 3 sequential signed register values
func (wb *Vool) getPhaseValues(reg uint16, divider float64) (float64, float64, float64, error) {
	b, err := wb.conn.ReadHoldingRegisters(reg, 3)
	if err != nil {
		return 0, 0, 0, err
	}

	var res [3]float64
	for i := range res {
		res[i] = float64(int16(binary.BigEndian.Uint16(b[2*i:]))) / divider
	}

	return res[0], res[1], res[2], nil
}

var _ api.PhaseCurrents = (*Vool)(nil)

// Currents implements the api.PhaseCurrents interface
func (wb *Vool) Currents() (float64, float64, float64, error) {
	return wb.getPhaseValues(voolRegCurrents, 100)
}

var _ api.PhaseVoltages = (*Vool)(nil)

// Voltages implements the api.PhaseVoltages interface
func (wb *Vool) Voltages() (float64, float64, float64, error) {
	return wb.getPhaseValues(voolRegVoltages, 10)
}

// phases1p3p implements the api.PhaseSwitcher interface
func (wb *Vool) phases1p3p(phases int) error {
	// 1p: gewählte Netzphase (L1 -> 0b001, L2 -> 0b010, L3 -> 0b100)
	mask := uint16(1) << (wb.phase - 1)
	if phases == 3 {
		mask = 0b111
	}

	return wb.writeUint16(voolRegPhases, mask)
}

// getPhases implements the api.PhaseGetter interface
func (wb *Vool) getPhases() (int, error) {
	u, err := wb.readUint16(voolRegPhases)
	if err != nil {
		return 0, err
	}

	return bits.OnesCount16(u & 0b111), nil
}

var _ api.Identifier = (*Vool)(nil)

// Identify implements the api.Identifier interface
func (wb *Vool) Identify() ([]string, error) {
	b, err := wb.conn.ReadHoldingRegisters(voolRegAuthorizeID, 18)
	if err != nil {
		return nil, err
	}

	// ASCII, nullterminiert
	id := strings.TrimRight(string(b), "\x00")
	if id == "" {
		return nil, nil
	}

	return []string{id}, nil
}

var _ api.Diagnosis = (*Vool)(nil)

// Diagnose implements the api.Diagnosis interface
func (wb *Vool) Diagnose() {
	// Event zuerst lesen: Das Lesen der Authorize Id setzt das Flag zurück
	if u, err := wb.readUint16(voolRegEvent); err == nil {
		fmt.Printf("Event:\t\t%016b\n", u)
	}
	if u, err := wb.readUint16(voolRegChargerState); err == nil {
		fmt.Printf("State:\t\t%d\n", u)
	}
	if u, err := wb.readUint16(voolRegPhases); err == nil {
		fmt.Printf("Phases (502):\t%03b\n", u&0b111)
	}
	if u, err := wb.readUint16(voolRegCurrentLimit); err == nil {
		fmt.Printf("Current limit:\t%.2f A\n", float64(u)/100)
	}
	if id, err := wb.Identify(); err == nil {
		fmt.Printf("Authorize Id:\t%v\n", id)
	}
}
