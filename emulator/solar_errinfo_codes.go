package emulator

import "fmt"

// errInfoCodeName maps a puterrinfo event code to a short machine-readable
// name. Derived from Ghidra static analysis of B2500_All_HMJ.bin (HMJ fw 110).
//
// Each entry was traced from the call site of enqueue_event() in the firmware.
// The trigger column describes what hardware/software condition fires the event;
// the value column describes how to interpret the 32-bit value field.
//
// 49 distinct literal codes were found across 95 call sites. All codes are now
// named; codes outside this set are logged with name "unknown_<code>".
//
// Wire format recap:
//
//	Type 0 (battery slot 0): <uid>:<type>:<sw_ver>:<soc_pct>:<flags_4d>:<flags_4e>:<flags_4f>:<code>.<ts>.<value>,...
//	Type 1 (battery slot 1): <uid>:<type>:<sw_ver>:<soc_pct>:<flags_de>:<flags_df>:<flags_e0>:<a>.<b>.<c>.<d>.<value>,...
//	Type 2 (battery slot 2): <uid>:<type>:<sw_ver>:<soc_pct>:<flags_16f>:<flags_170>:<flags_171>:<a>.<b>.<c>.<d>.<value>,...
//
// Ghidra source: B2500_All_HMJ.bin loaded at 0x08000000, ARM:LE:32:Cortex,
// firmware version 110. See docs/firmware.md for full analysis notes.
var errInfoCodeName = map[int64]string{
	0:   "startup_init",
	12:  "soc_threshold_crossed",
	15:  "cell_overvoltage_charge",
	18:  "cell_voltage_high",
	24:  "discharge_status_flag",
	25:  "bms_probe_no_response",
	33:  "shelly_ct_meter_status",
	35:  "undervoltage_discharge",
	36:  "soc_zero_or_overvoltage_low",
	38:  "charge_voltage_limit",
	39:  "overcurrent_charge",
	42:  "discharge_protection_flag",
	43:  "battery_capacity_low",
	50:  "thermal_discharge_high",
	51:  "thermal_discharge_low",
	52:  "thermal_charge_high",
	53:  "thermal_charge_low",
	62:  "ble_energy_accumulated",
	64:  "battery_poll_status",
	65:  "reboot_pending",
	66:  "ble_soc_state_changed",
	73:  "bms_comm_watchdog",
	74:  "mqtt_ext_conn_failed",
	75:  "fault_flags_bitmap",
	77:  "soc_below_threshold",
	78:  "mqtt_ext_session_up",
	80:  "setreport_response_parsed",
	81:  "battery_pack_init_no_response",
	82:  "battery_pack_init_cell_fault",
	84:  "heartbeat",
	85:  "mqtt_ext_subscribe_failed",
	86:  "tls_cert_inventory_missing",
	87:  "tls_cert_slots_cleared",
	88:  "tls_user_key_written",
	89:  "charge_current_high_ch0",
	90:  "charge_current_high_ch1",
	91:  "http_post_failed",
	92:  "cert_slot_selected",
	93:  "cert_activated",
	95:  "cell_fault_flags_packed",
	96:  "mqtt_auth_max_retries",
	98:  "subsystem_state_event",
	99:  "cert_change_triggered",
	100: "charge_setpoint_exceeded",
	101: "passthrough_mode_changed",
	103: "ac_coupling_event",
	104: "battery_pack_reset_complete",
	105: "ble_adv_retry_exhausted",
	106: "wifi_disconnect",
}

// errInfoCodeDesc maps event codes to a one-line human-readable description
// of what fired the event and how to read the value field.
var errInfoCodeDesc = map[int64]string{
	0:   "System startup: battery poll init; value always 0",
	12:  "SoC counter threshold debounce fired OR HTTP response SoC == 100%; value = measured counter/SoC",
	15:  "Cell overvoltage during charge: battery voltage exceeded two-level threshold; value = mV",
	18:  "Cell voltage crossed high threshold (bms_threshold_monitor debounce); value = max cell voltage",
	24:  "Discharge status flag changed (state[0x4d] bit set); value = status flags byte",
	25:  "BMS communication probe failed (FUN_0801b30c returned non-zero); value = 0xFF constant",
	33:  "Shelly CT meter status byte at state+0x4d changed (top bit set); value = status byte",
	35:  "Undervoltage during discharge: battery_voltage < threshold (debounced); value = battery voltage mV",
	36:  "SoC == 0 (HTTP parse) OR battery voltage < low threshold (reset FSM); value = voltage or SoC",
	38:  "Charge voltage crossing boundary in MQTT publish FSM; value = battery voltage mV",
	39:  "Overcurrent during charge: battery current > threshold; value = current",
	42:  "Discharge protection flag: state[0x4d] bit 2 set; value = protection flag byte",
	43:  "Battery capacity low (poll timer expired); value = raw poll data",
	50:  "Thermal protection: pack max temperature exceeded discharge limit; value = temperature (i16)",
	51:  "Thermal protection: pack min temperature exceeded discharge limit; value = temperature (i16)",
	52:  "Thermal protection: pack max temperature exceeded charge limit; value = temperature (i16)",
	53:  "Thermal protection: pack min temperature exceeded charge limit; value = temperature (i16)",
	62:  "BLE energy accumulation complete (GATT notify FSM); value = accumulated energy (wH, i32)",
	64:  "Battery poll state changed (battery_data_poll_fsm); value = raw poll data",
	65:  "MCU reboot pending (before DataSynchronizationBarrier reset); value = 0",
	66:  "BLE SoC state changed (ble_conn_state_machine); value = state byte or CONCAT(v_raw, v_threshold)",
	73:  "BMS communication watchdog triggered (bms_fault_flags_monitor); value = 0x11 constant",
	74:  "Secondary MQTT client AT+QMTCONN rejected after 4 retries (auxiliary session used by http_getdateinfo); value = broker connect-reject status",
	75:  "BMS fault flags bitmap changed (bms_fault_flags_monitor) OR WiFi scan retries exhausted (wifi_scan_fsm); value = byte_field | (u16_field << 16), or 0 for wifi scan",
	77:  "SoC fell below charge threshold (mqtt_publish_fsm / battery_charge_monitor); value = SoC %",
	78:  "Secondary MQTT client AT+QMTSUB succeeded (session fully established); value = 1 or subscribed-topics feature-flag bitmap (flag_a0b<<3 | flag_a0c<<2 | flag_a0d<<1 | base)",
	80:  "setreport HTTP response JSON parsed via alternate key branch (setreport_response_handler); value = response flag byte",
	81:  "Battery pack init probe returned 0 or 0xFF (no reply from BMS over the 0x81 cmd); value = raw probe return",
	82:  "Battery pack init detected cell-voltage fault (one of three thresholds crossed during init: 0x31 / 0x12 / 0x50); value = fault bitmask byte (bits 3/4/5)",
	84:  "Heartbeat: periodic 60s timer OR HTTP response; value = 0 (timer), 327679/0x4FFFF (HTTP OK composite), or HTTP status code (e.g. 404) on non-OK response",
	85:  "Secondary MQTT client AT+QMTSUB rejected after 4 retries; value = broker subscribe-reject byte",
	86:  "TLS cert inventory check failed: AT+QFLST did not list all three expected files (User_Cert_1 / User_Key_1 / CA); triggers re-provisioning via state 0x14; value = inventory bitmap",
	87:  "TLS cert slots cleared: AT+QSSLCERT=\"CA\",0 → \"User_Cert\",0 → \"User_Key\",0 all OK (ready to flash new certs); value = retry counter at state+0xb2a",
	88:  "TLS User Key payload flashed to modem cert store (QFUPL → binary upload OK); value = 0",
	89:  "Charge current high channel 0: ch0 current > 16 A (60 s debounce); value = charge current or state",
	90:  "Charge current high channel 1: ch1 current > 16 A (60 s debounce); value = charge current or state",
	91:  "HTTP POST to puterrinfo returned non-200; value = HTTP status code or 0 on timeout",
	92:  "TLS cert slot selected for battery slot N (ssl_cert_provision_fsm); value = slot index (0-2)",
	93:  "TLS cert activated (mqtt_tls_config_fsm / mqtt_open_fsm); value = offset + slot_idx + soc*10000",
	95:  "Pack cell-fault flag bytes logged on every 50% SoC crossing (battery_cell_fault_handler); value = (flags[0x171]<<24) | (flags[0xe0]<<16) | (flags[0x4f]<<8) | (soc & 0xff)",
	96:  "MQTT connect auth exceeded max retries (mqtt_conn_auth_fsm); value = (b_retries << 16) | a_retries",
	98:  "Subsystem state transition event; value = subsystem-specific: 10001-10005 = AT modem error stage",
	99:  "TLS cert rotation triggered (mqtt_tls_config_fsm / mqtt_conn_auth_fsm); value = cert ID byte",
	100: "Charge setpoint voltage exceeded (at_modem_event_handler); value = CONCAT(u16 v0, u16 v1) or slot+v*10",
	101: "Pass-through mode changed via BLE or cloud command; value = 0 (disabled) or 1 (enabled)",
	103: "AC coupling event (at_modem_event_handler); value = 710/709 (timeout), 810/811 (fault), or v+8000",
	104: "Battery pack reset sequence completed (battery_pack_reset); value = 0",
	105: "BLE advertising retry counter at state+0x18 exceeded 3 (advertising restart give-up, ble_adv_state_machine); value = final retry count",
	106: "WiFi disconnected: supplicant reported reason 1 or 2 three times in a row (wifi_connect_fsm); value = disconnect reason code (1 or 2)",
}

// errInfoCodeLabel returns the short metric-safe name for a code, falling back
// to "unknown_N" for codes not in the dictionary.
func errInfoCodeLabel(code int64) string {
	if name, ok := errInfoCodeName[code]; ok {
		return name
	}
	return fmt.Sprintf("unknown_%d", code)
}

// errInfoCodeDescription returns the human-readable description for a code,
// falling back to an empty string for codes not in the dictionary.
func errInfoCodeDescription(code int64) string {
	return errInfoCodeDesc[code]
}
