package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// The machine's memory travels with a status reply, and an older shinyctl-api
// leaves it out: that is a nil, not a zero reading.
func TestStatusResponse_MachineMemoryRoundTrip(t *testing.T) {
	with := StatusResponse{MachineMemory: &MachineMemory{
		Low: true, TotalBytes: 4 << 30, AvailableBytes: 0, SwapTotalBytes: 2 << 30,
		SwapUsedBytes: 1 << 30, StallPercent: 23, Pressure: "warning",
	}}
	data, err := json.Marshal(with)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"available-bytes":0`) {
		t.Errorf("a zero available is not in the body: %s", data)
	}
	var got StatusResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.MachineMemory == nil || *got.MachineMemory != *with.MachineMemory {
		t.Errorf("round trip = %+v, want %+v", got.MachineMemory, with.MachineMemory)
	}

	data, err = json.Marshal(StatusResponse{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "machine-memory") {
		t.Errorf("an empty response has the key: %s", data)
	}
	got = StatusResponse{}
	if err := json.Unmarshal([]byte(`{"apps":[]}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.MachineMemory != nil {
		t.Errorf("absent field = %+v, want nil", got.MachineMemory)
	}
}
