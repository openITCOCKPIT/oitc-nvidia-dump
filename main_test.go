package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const helpQueryGpu = `List of valid properties to query for the switch "--query-gpu=":

"timestamp"
The timestamp of when the query was made in format "YYYY/MM/DD HH:MM:SS.msec".

"index"
Zero based index of the GPU. Can change at each boot.

"uuid"
This value is the globally unique immutable alphanumeric identifier of the GPU.

"serial"
This number matches the serial number physically printed on each board.

"temperature.gpu"
 Core GPU temperature. in degrees C.

"clocks_event_reasons.active" or "clocks_throttle_reasons.active"
Bitmask of active clock event reasons. See "nvidia-smi -q -d PERFORMANCE".
`

func TestSupportedFields(t *testing.T) {
	fields := supportedFields(helpQueryGpu, []string{"index", "serial", "memory.used", "clocks_throttle_reasons.active", "clocks_event_reasons.active"})
	if strings.Join(fields, ",") != "index,serial,clocks_throttle_reasons.active,clocks_event_reasons.active" {
		t.Errorf("unexpected fields: %v", fields)
	}
	// Quoted text in the description is no field
	if fields := supportedFields(helpQueryGpu, []string{"YYYY/MM/DD HH:MM:SS.msec", "nvidia-smi -q -d PERFORMANCE"}); len(fields) != 0 {
		t.Errorf("unexpected fields: %v", fields)
	}
}

func TestFilterJournal(t *testing.T) {
	output := `{"__REALTIME_TIMESTAMP":"1792483200000000","MESSAGE":"NVRM: Xid (PCI:0000:3b:00): 79, pid=1234, name=python3, GPU has fallen off the bus."}
{"__REALTIME_TIMESTAMP":"1792483201000000","MESSAGE":"EXT4-fs (sda1): mounted filesystem"}
{"__REALTIME_TIMESTAMP":"1792483202000000","MESSAGE":[110,118,105,100,105,97,45,110,118,115,119,105,116,99,104,48,58,32,83,88,105,100]}
not json
`
	lines := strings.Split(filterJournal(output), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " NVRM: Xid (PCI:0000:3b:00): 79, pid=1234, name=python3, GPU has fallen off the bus.") ||
		!strings.HasSuffix(lines[1], " nvidia-nvswitch0: SXid") {
		t.Errorf("unexpected lines: %q", lines)
	}
	if want := time.UnixMicro(1792483200000000).Format(time.RFC3339); !strings.HasPrefix(lines[0], want) {
		t.Errorf("unexpected timestamp: %s", lines[0])
	}
}

func TestAnonymize(t *testing.T) {
	hostname, _ := os.Hostname()
	d := &dump{Commands: []*commandResult{
		{Name: "list", Stdout: "GPU 0: NVIDIA H200 (UUID: GPU-1a2b3c4d-1111-2222-3333-444455556666)\nGPU 1: NVIDIA H200 (UUID: GPU-ffffffff-1111-2222-3333-444455556666)\n"},
		{Name: "query", Stdout: "<serial>1650123456789</serial><uuid>GPU-1A2B3C4D-1111-2222-3333-444455556666</uuid><chassis_serial_number>N/A</chassis_serial_number>"},
		{Name: "query-gpu", Stdout: "index, uuid, serial\n0, GPU-1a2b3c4d-1111-2222-3333-444455556666, 1650123456789\n1, GPU-ffffffff-1111-2222-3333-444455556666, 1650987654321\n"},
		{Name: "dcgm-exporter", Stdout: fmt.Sprintf(`DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-1a2b3c4d-1111-2222-3333-444455556666",Hostname="%s",serial="1650123456789"} 35`, hostname)},
	}}
	anonymize(d)

	all := ""
	for _, c := range d.Commands {
		all += c.Stdout + "\n"
	}
	for _, secret := range []string{"1a2b3c4d", "1A2B3C4D", "ffffffff", "1650123456789", "1650987654321"} {
		if strings.Contains(all, secret) {
			t.Errorf("%s not replaced:\n%s", secret, all)
		}
	}
	if len(hostname) >= 3 && strings.Contains(all, hostname) {
		t.Errorf("host name not replaced:\n%s", all)
	}
	// The same GPU has the same placeholders in all outputs
	if !strings.Contains(d.Commands[0].Stdout, "GPU 0: NVIDIA H200 (UUID: GPU-00000000-0000-0000-0000-000000000001)") ||
		!strings.Contains(d.Commands[1].Stdout, "<uuid>GPU-00000000-0000-0000-0000-000000000001</uuid>") ||
		!strings.Contains(d.Commands[1].Stdout, "<serial>SERIAL0001</serial>") ||
		!strings.Contains(d.Commands[1].Stdout, "<chassis_serial_number>N/A</chassis_serial_number>") ||
		!strings.Contains(d.Commands[2].Stdout, "1, GPU-00000000-0000-0000-0000-000000000002, SERIAL0002") ||
		!strings.Contains(d.Commands[3].Stdout, `Hostname="host",serial="SERIAL0001"`) {
		t.Errorf("unexpected placeholders:\n%s", all)
	}
}

// fakeNvidiaSmi answers like nvidia-smi on a system with two H200
const fakeNvidiaSmi = `#!/bin/sh
case "$1" in
--version) echo "NVIDIA-SMI version  : 580.95.05" ;;
-L) echo "GPU 0: NVIDIA H200 (UUID: GPU-1a2b3c4d-1111-2222-3333-444455556666)" ;;
-q) echo "<?xml version=\"1.0\" ?><nvidia_smi_log><gpu id=\"00000000:3B:00.0\"><product_name>NVIDIA H200</product_name><serial>1650123456789</serial><uuid>GPU-1a2b3c4d-1111-2222-3333-444455556666</uuid></gpu></nvidia_smi_log>" ;;
--help-query-gpu) cat "$(dirname "$0")/help.txt" ;;
--query-gpu=*) echo "$1" > "$(dirname "$0")/query-gpu.txt"; echo "index, uuid, serial"; echo "0, GPU-1a2b3c4d-1111-2222-3333-444455556666, 1650123456789" ;;
--query-compute-apps=*) echo "gpu_uuid, pid, process_name, used_gpu_memory [MiB]" ;;
nvlink) echo "No NVLinks found" >&2; exit 6 ;;
topo) echo "	GPU0	CPU Affinity" ;;
*) echo "unknown $*" >&2; exit 2 ;;
esac
`

func TestCollect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake nvidia-smi is a shell script")
	}
	dir := t.TempDir()
	smi := filepath.Join(dir, "nvidia-smi")
	if err := os.WriteFile(smi, []byte(fakeNvidiaSmi), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "help.txt"), []byte(helpQueryGpu), 0o644); err != nil {
		t.Fatal(err)
	}
	exporter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-1a2b3c4d-1111-2222-3333-444455556666",Hostname="gpu-node-01"} 35`)
	}))
	defer exporter.Close()

	d := collect(context.Background(), options{nvidiaSmi: smi, dcgmURL: exporter.URL, days: 1, cmdTimeout: 10 * time.Second})

	commands := map[string]*commandResult{}
	for _, c := range d.Commands {
		commands[c.Name] = c
	}
	for _, name := range []string{"version", "list", "query", "query (second run)", "help-query-gpu", "query-gpu", "compute-apps", "topology", "dcgm-exporter"} {
		if c := commands[name]; c == nil || c.ExitCode != 0 || c.Error != "" {
			t.Errorf("%s failed: %+v", name, c)
		}
	}
	if c := commands["nvlink-status"]; c == nil || c.ExitCode != 6 || !strings.Contains(c.Stderr, "No NVLinks found") {
		t.Errorf("unexpected nvlink result: %+v", c)
	}
	// Only the fields of the installed driver are queried
	query, err := os.ReadFile(filepath.Join(dir, "query-gpu.txt"))
	if err != nil || strings.TrimSpace(string(query)) != "--query-gpu=index,uuid,serial,temperature.gpu,clocks_event_reasons.active,clocks_throttle_reasons.active" {
		t.Errorf("unexpected query: %s %v", query, err)
	}
	if !d.Anonymized || !strings.Contains(commands["query"].Stdout, "<serial>SERIAL0001</serial>") ||
		!strings.Contains(commands["query-gpu"].Stdout, "0, GPU-00000000-0000-0000-0000-000000000001, SERIAL0001") ||
		commands["dcgm-exporter"].Stdout != `DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-00000000-0000-0000-0000-000000000001",Hostname="host"} 35`+"\n" {
		t.Errorf("not anonymized: %+v %+v %+v", commands["query"], commands["query-gpu"], commands["dcgm-exporter"])
	}
	if d.Platform["memtotal"] == "" && runtime.GOOS == "linux" {
		t.Errorf("missing platform information: %v", d.Platform)
	}
}
