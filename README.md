# oitc-nvidia-dump

`oitc-nvidia-dump` collects the information of the NVIDIA GPUs of a system (`nvidia-smi`, DCGM, the kernel or event
log of the NVIDIA driver) and writes it into a JSON file.

The [openITCOCKPIT Monitoring Agent](https://github.com/openITCOCKPIT/openitcockpit-agent-go) uses these dumps to develop
and test its GPU monitoring against many GPU models (data center GPUs like the H100/H200, DGX Spark / GB10, workstation
GPUs) and driver versions without access to the hardware.

## Safety: read only

The tool only runs a fixed list of commands, all of them only read information. It does not change any setting of
the GPUs or the driver and does not start any GPU workload.

| Command | Platform | Purpose |
|---|---|---|
| `nvidia-smi --version` | Linux, Windows | Versions of nvidia-smi, the driver and CUDA |
| `nvidia-smi -L` | Linux, Windows | List of the GPUs |
| `nvidia-smi -q -x` (twice) | Linux, Windows | All information of all GPUs (XML). The second run shows the duration if the driver is initialized |
| `nvidia-smi --help-query-gpu` | Linux, Windows | Fields which the installed driver supports |
| `nvidia-smi --query-gpu=... --format=csv` | Linux, Windows | Temperatures, utilization, memory, power, clocks, clock event (throttle) reasons, ECC, retired pages, PCIe |
| `nvidia-smi --query-compute-apps=... --format=csv` | Linux, Windows | Processes using the GPUs (pid, name, memory) |
| `nvidia-smi nvlink -s`, `nvidia-smi nvlink -e` | Linux, Windows | NVLink status and error counters |
| `nvidia-smi topo -m` | Linux, Windows | Topology (NVLink, PCIe, CPU affinity) |
| `dcgmi discovery -l` | if installed | GPUs and NVSwitches known to the Data Center GPU Manager |
| `GET http://127.0.0.1:9400/metrics` | if running | Metrics of the [dcgm-exporter](https://github.com/NVIDIA/dcgm-exporter) |
| `systemctl is-active nvidia-fabricmanager nvidia-persistenced nvidia-dcgm dcgm-exporter` | Linux | State of the NVIDIA services |
| `nv-fabricmanager --version` | Linux, if installed | Version of the Fabric Manager (NVSwitch systems) |
| `journalctl -k -o json` or `dmesg` | Linux | Messages of the NVIDIA driver (`NVRM`, Xid, SXid) of the last 14 days, only these lines are kept |
| `Get-WinEvent` (source `nvlddmkm`) | Windows | Events of the NVIDIA driver of the last 14 days |

Additionally the operating system, the kernel version, the driver version (`/proc/driver/nvidia/version`) and the size
of the memory (`MemTotal`, `MemAvailable`, `SwapTotal`, `SwapFree`) are read. GPUs with unified memory like the
DGX Spark have no own memory, `nvidia-smi` reports "Not Supported" there.

## What is in a dump

- GPU models, driver, CUDA and VBIOS versions, PCI bus ids
- Temperatures, utilization, memory, power draw and limits, clocks, clock event (throttle) reasons
- ECC errors, retired pages, remapped rows, NVLink status and errors, Xid errors
- The names and memory usage of the processes using the GPUs (e.g. `python3`, `ollama`)
- The duration of every command

By default serial numbers, UUIDs and the host name are replaced by placeholders (`SERIAL0001`,
`GPU-00000000-0000-0000-0000-000000000001`, `host`). The same value gets the same placeholder in all outputs. Use `-raw`
to keep the original values.

Please have a look at the JSON file before you send it, e.g. the process names and the messages of the kernel log.

## Usage

Download the binary for your system from the [releases](../../releases) or build it yourself (see below).
The tool is a single static binary without dependencies. It only needs the NVIDIA driver (`nvidia-smi`).

### Linux

Run the tool as root, otherwise the kernel log may not be readable:

```
chmod +x oitc-nvidia-dump-1.0-linux-amd64
sudo ./oitc-nvidia-dump-1.0-linux-amd64 -o nvidia-dump.json
```

Use `oitc-nvidia-dump-1.0-linux-arm64` on ARM systems like the DGX Spark or Grace (GH200/GB200) systems.

### Windows

Run the tool in a command prompt (as administrator to read the event log completely):

```
oitc-nvidia-dump-1.0-windows-amd64.exe -o nvidia-dump.json
```

### Output

```
Using /usr/bin/nvidia-smi
Running version ...
...
Running kernel-log ...
15 commands (2 failed or not available), errors: 0
Written to nvidia-dump.json
```

Commands which are not available on a system (e.g. `nvidia-smi nvlink` without NVLink, `dcgmi` or the dcgm-exporter
if not installed) are recorded with their exit code and error message, this is expected.

Options:

```
-o string              output file (default "nvidia-dump.json")
-days int              days of the kernel log (Linux) or event log (Windows) (default 14)
-dcgm-exporter string  URL of the dcgm-exporter, empty to skip (default "http://127.0.0.1:9400/metrics")
-raw                   do not replace serial numbers, UUIDs and the host name
-timeout duration      timeout (default 10m0s)
-version               print the version
```

## Sending a dump

Please open an issue in this repository and attach the JSON file. Please tell us

- the system and GPUs (e.g. HGX H200 8-GPU, DGX Spark, workstation with RTX 6000 Ada)
- whether we may publish the dump as test data in the repository of the openITCOCKPIT Monitoring Agent.
  Dumps without this permission are only used for tests which are not published.
- anything special about the system, e.g. a GPU with known errors or MIG instances

## Format of the dump

```json
{
  "tool": "oitc-nvidia-dump",
  "version": "1.0",
  "created": "2026-10-20T16:00:00+02:00",
  "os": "linux/amd64",
  "anonymized": true,
  "platform": { "os_release": "Ubuntu 24.04.3 LTS", "kernel": "6.8.0-85-generic", "memtotal": "2113741824 kB" },
  "errors": [],
  "commands": [
    { "name": "query", "command": ["/usr/bin/nvidia-smi", "-q", "-x"], "exit_code": 0, "stdout": "<?xml ...", "duration_ms": 412 }
  ]
}
```

## Building

Requires Go 1.24 or newer.

```
go test ./...
./build.sh
```

`build.sh` creates static binaries for Linux (amd64, arm64) and Windows (amd64), the licenses of the libraries which are
compiled into the binaries (`THIRD_PARTY_LICENSES.txt`) and the SHA256 checksums in `dist/`.

## License

Copyright (c) 2026 AVENDIS GmbH, MIT License, see [LICENSE](LICENSE).
