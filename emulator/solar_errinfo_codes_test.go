package emulator

import "testing"

func TestDecodeErrInfoCode75ObservedValues(t *testing.T) {
	tests := []struct {
		name       string
		value      int64
		wantRaw    uint32
		wantReason uint8
		wantRSSI   int16
	}{
		{name: "ffb90002", value: -4653054, wantRaw: 0xffb90002, wantReason: 2, wantRSSI: -71},
		{name: "ffba0002", value: -4587518, wantRaw: 0xffba0002, wantReason: 2, wantRSSI: -70},
		{name: "ffbb0002", value: -4521982, wantRaw: 0xffbb0002, wantReason: 2, wantRSSI: -69},
		{name: "ffbc0002", value: -4456446, wantRaw: 0xffbc0002, wantReason: 2, wantRSSI: -68},
		{name: "ffbd0003", value: -4390909, wantRaw: 0xffbd0003, wantReason: 3, wantRSSI: -67},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeErrInfoCode75(tc.value)
			if got.ScanTimeout {
				t.Fatalf("ScanTimeout=true for non-zero value %d", tc.value)
			}
			if got.RawU32 != tc.wantRaw {
				t.Fatalf("raw=0x%08x, want 0x%08x", got.RawU32, tc.wantRaw)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("reason=%d, want %d", got.Reason, tc.wantReason)
			}
			if got.RSSIDBm != tc.wantRSSI {
				t.Fatalf("rssi_dbm=%d, want %d", got.RSSIDBm, tc.wantRSSI)
			}
		})
	}
}

func TestDecodeErrInfoCode75ScanTimeoutZero(t *testing.T) {
	got := decodeErrInfoCode75(0)
	if !got.ScanTimeout {
		t.Fatalf("ScanTimeout=false, want true")
	}
	if got.RawU32 != 0 {
		t.Fatalf("raw=0x%08x, want 0x00000000", got.RawU32)
	}
	if got.ReasonPresent {
		t.Fatalf("ReasonPresent=true, want false")
	}
	if got.Reason != 0 || got.RSSIDBm != 0 {
		t.Fatalf("reason/rssi should be zero for timeout case, got reason=%d rssi=%d", got.Reason, got.RSSIDBm)
	}
}
