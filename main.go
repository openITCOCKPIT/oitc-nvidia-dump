// oitc-nvidia-dump collects the output of read only commands of the NVIDIA driver (nvidia-smi), the Data Center GPU
// Manager (DCGM) and the kernel log and writes it to a JSON file. The openITCOCKPIT Monitoring Agent uses these dumps
// to develop and test its GPU check without access to the hardware.
//
// Only a fixed list of commands is executed, all of them only read information. Serial numbers, UUIDs and the
// host name are replaced by placeholders (disable with -raw).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const version = "1.1"

// Fields of nvidia-smi --query-gpu. Only the fields which the installed driver knows (--help-query-gpu) are queried,
// e.g. clocks_event_reasons.* replaced clocks_throttle_reasons.* in newer drivers.
var queryGpuFields = []string{
	"index", "uuid", "name", "serial", "pci.bus_id", "driver_version", "vbios_version", "inforom.img",
	"persistence_mode", "compute_mode", "driver_model.current", "mig.mode.current", "pstate", "fan.speed",
	"temperature.gpu", "temperature.memory", "temperature.gpu.tlimit",
	"utilization.gpu", "utilization.memory",
	"memory.total", "memory.reserved", "memory.used", "memory.free",
	"power.draw", "power.draw.average", "power.draw.instant", "power.limit", "enforced.power.limit", "power.max_limit",
	"clocks.sm", "clocks.mem", "clocks.max.sm", "clocks.max.mem",
	"clocks_event_reasons.active", "clocks_event_reasons.hw_slowdown", "clocks_event_reasons.hw_thermal_slowdown",
	"clocks_event_reasons.hw_power_brake_slowdown", "clocks_event_reasons.sw_thermal_slowdown", "clocks_event_reasons.sw_power_cap",
	"clocks_throttle_reasons.active", "clocks_throttle_reasons.hw_slowdown", "clocks_throttle_reasons.hw_thermal_slowdown",
	"clocks_throttle_reasons.hw_power_brake_slowdown", "clocks_throttle_reasons.sw_thermal_slowdown", "clocks_throttle_reasons.sw_power_cap",
	"ecc.mode.current", "ecc.errors.corrected.volatile.total", "ecc.errors.uncorrected.volatile.total",
	"ecc.errors.corrected.aggregate.total", "ecc.errors.uncorrected.aggregate.total",
	"retired_pages.single_bit_ecc.count", "retired_pages.double_bit.count", "retired_pages.pending",
	"pcie.link.gen.current", "pcie.link.gen.max", "pcie.link.width.current", "pcie.link.width.max",
}

// Kernel log messages of the NVIDIA driver, e.g. "NVRM: Xid (PCI:0000:3b:00): 79, ..." or the SXid of NVSwitches
var kernelLogPattern = regexp.MustCompile(`NVRM|Xid|nvidia`)

type commandResult struct {
	Name       string   `json:"name"`
	Command    []string `json:"command"`
	ExitCode   int      `json:"exit_code"`
	Stdout     string   `json:"stdout"`
	Stderr     string   `json:"stderr,omitempty"`
	Error      string   `json:"error,omitempty"`
	DurationMs int64    `json:"duration_ms"`
}

type dump struct {
	Tool       string            `json:"tool"`
	Version    string            `json:"version"`
	Created    string            `json:"created"`
	OS         string            `json:"os"`
	Anonymized bool              `json:"anonymized"`
	Platform   map[string]string `json:"platform"`
	Errors     []string          `json:"errors"`
	Commands   []*commandResult  `json:"commands"`
}

type options struct {
	nvidiaSmi  string // path of nvidia-smi, empty if not found
	dcgmi      string // path of dcgmi, empty if not found
	dcgmURL    string // metrics of the dcgm-exporter
	days       int    // kernel log / event log
	raw        bool
	cmdTimeout time.Duration
}

func main() {
	output := flag.String("o", "nvidia-dump.json", "output file")
	timeout := flag.Duration("timeout", 10*time.Minute, "timeout")
	days := flag.Int("days", 14, "days of the kernel log (Linux) or event log (Windows)")
	dcgmURL := flag.String("dcgm-exporter", "http://127.0.0.1:9400/metrics", "URL of the dcgm-exporter, empty to skip")
	raw := flag.Bool("raw", false, "do not replace serial numbers, UUIDs, the host name and user names")
	showVersion := flag.Bool("version", false, "print the version")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "oitc-nvidia-dump %s - collects the GPU information of the NVIDIA driver (read only) and writes it to a JSON file\n\nUsage: %s [options]\n\n", version, os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	opts := options{
		nvidiaSmi:  findNvidiaSmi(),
		dcgmi:      findExecutable("dcgmi"),
		dcgmURL:    *dcgmURL,
		days:       *days,
		raw:        *raw,
		cmdTimeout: 2 * time.Minute,
	}
	if opts.nvidiaSmi == "" {
		fmt.Println("nvidia-smi not found, only the other information is collected")
	} else {
		fmt.Println("Using", opts.nvidiaSmi)
	}

	d := collect(ctx, opts)
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*output, data, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	failed := 0
	for _, c := range d.Commands {
		if c.Error != "" || c.ExitCode != 0 {
			failed++
		}
	}
	fmt.Printf("%d commands (%d failed or not available), errors: %d\nWritten to %s\n", len(d.Commands), failed, len(d.Errors), *output)
}

func collect(ctx context.Context, opts options) *dump {
	d := &dump{
		Tool:       "oitc-nvidia-dump",
		Version:    version,
		Created:    time.Now().Format(time.RFC3339),
		OS:         runtime.GOOS + "/" + runtime.GOARCH,
		Anonymized: !opts.raw,
		Platform:   platformInfo(ctx),
		Errors:     []string{},
	}
	run := func(name string, args ...string) *commandResult {
		fmt.Println("Running", name, "...")
		c := runCommand(ctx, opts.cmdTimeout, name, args...)
		d.Commands = append(d.Commands, c)
		return c
	}

	if opts.nvidiaSmi == "" {
		d.Errors = append(d.Errors, "nvidia-smi not found")
	} else {
		smi := opts.nvidiaSmi
		run("version", smi, "--version")
		run("list", smi, "-L")
		run("query", smi, "-q", "-x")
		// The first call can be slow if the driver is not loaded permanently (persistence mode off)
		run("query (second run)", smi, "-q", "-x")
		help := run("help-query-gpu", smi, "--help-query-gpu")
		fields := supportedFields(help.Stdout, queryGpuFields)
		if len(fields) == 0 {
			fields = []string{"index", "uuid", "name", "temperature.gpu", "utilization.gpu", "memory.total", "memory.used", "power.draw"}
		}
		run("query-gpu", smi, "--query-gpu="+strings.Join(fields, ","), "--format=csv")
		run("compute-apps", smi, "--query-compute-apps=gpu_uuid,pid,process_name,used_memory", "--format=csv")
		run("nvlink-status", smi, "nvlink", "-s")
		run("nvlink-errors", smi, "nvlink", "-e")
		run("topology", smi, "topo", "-m")
	}

	if opts.dcgmi != "" {
		run("dcgmi-discovery", opts.dcgmi, "discovery", "-l")
	}
	if opts.dcgmURL != "" {
		fmt.Println("Running dcgm-exporter ...")
		d.Commands = append(d.Commands, httpGet(ctx, "dcgm-exporter", opts.dcgmURL))
	}

	switch runtime.GOOS {
	case "linux":
		if findExecutable("systemctl") != "" {
			// One line per unit: active, inactive, failed or unknown (not installed)
			run("services", "systemctl", "is-active", "nvidia-fabricmanager", "nvidia-persistenced", "nvidia-dcgm", "dcgm-exporter")
		}
		if fm := findExecutable("nv-fabricmanager"); fm != "" {
			run("fabricmanager-version", fm, "--version")
		}
		fmt.Println("Running kernel-log ...")
		d.Commands = append(d.Commands, kernelLog(ctx, opts))
	case "windows":
		// The NVIDIA driver logs errors like Xid into the System event log (source nvlddmkm). Get-WinEvent fails with a
		// generic "The parameter is incorrect" if the source is not registered, e.g. without NVIDIA driver.
		script := fmt.Sprintf(`if (-not (Get-WinEvent -ListProvider nvlddmkm -ErrorAction SilentlyContinue)) {
  [Console]::Error.WriteLine('The event source nvlddmkm of the NVIDIA driver is not registered (no NVIDIA driver installed)')
  exit 0
}
$events = Get-WinEvent -FilterHashtable @{LogName='System'; ProviderName='nvlddmkm'; StartTime=(Get-Date).AddDays(-%d)} -MaxEvents 500 -ErrorAction SilentlyContinue
if ($events) {
  $events | Select-Object @{n='time';e={$_.TimeCreated.ToString('o')}},Id,LevelDisplayName,Message | ConvertTo-Json
}
exit 0`, opts.days)
		run("eventlog", powershell(script)...)
	}

	if !opts.raw {
		anonymize(d)
	}
	return d
}

func runCommand(ctx context.Context, timeout time.Duration, name string, args ...string) *commandResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result := &commandResult{Name: name, Command: args}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = &limitedWriter{buffer: &stdout, limit: 32 << 20}
	cmd.Stderr = &limitedWriter{buffer: &stderr, limit: 1 << 20}
	start := time.Now()
	err := cmd.Run()
	result.DurationMs = time.Since(start).Milliseconds()
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
	case err != nil:
		result.ExitCode = -1
		result.Error = err.Error()
	}
	if ctx.Err() != nil {
		result.Error = "timeout after " + timeout.String()
	}
	return result
}

type limitedWriter struct {
	buffer *bytes.Buffer
	limit  int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if remaining := w.limit - w.buffer.Len(); remaining > 0 {
		if len(p) > remaining {
			w.buffer.Write(p[:remaining])
		} else {
			w.buffer.Write(p)
		}
	}
	return len(p), nil
}

func httpGet(ctx context.Context, name, url string) *commandResult {
	result := &commandResult{Name: name, Command: []string{"GET", url}}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	defer func() { result.DurationMs = time.Since(start).Milliseconds() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		result.ExitCode, result.Error = -1, err.Error()
		return result
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Usually not running
		result.ExitCode, result.Error = -1, err.Error()
		return result
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	result.Stdout = string(body)
	if resp.StatusCode != http.StatusOK {
		result.ExitCode = resp.StatusCode
	}
	return result
}

// kernelLog returns the messages of the NVIDIA driver of the last days (Linux)
func kernelLog(ctx context.Context, opts options) *commandResult {
	since := fmt.Sprintf("%d days ago", opts.days)
	if findExecutable("journalctl") != "" {
		// JSON without the host name, which the text formats contain
		c := runCommand(ctx, opts.cmdTimeout, "kernel-log", "journalctl", "-k", "--no-pager", "-o", "json", "--since", since)
		if c.ExitCode == 0 && c.Error == "" {
			c.Stdout = filterJournal(c.Stdout)
			return c
		}
	}
	c := runCommand(ctx, opts.cmdTimeout, "kernel-log", "dmesg", "--time-format", "iso")
	if c.ExitCode == 0 && c.Error == "" {
		var lines []string
		for _, line := range strings.Split(c.Stdout, "\n") {
			if kernelLogPattern.MatchString(line) {
				lines = append(lines, line)
			}
		}
		c.Stdout = strings.Join(lines, "\n")
	}
	return c
}

// filterJournal converts the JSON output of journalctl to lines "<time> <message>" of the NVIDIA driver
func filterJournal(output string) string {
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		var entry struct {
			Timestamp string          `json:"__REALTIME_TIMESTAMP"`
			Message   json.RawMessage `json:"MESSAGE"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		var message string
		if json.Unmarshal(entry.Message, &message) != nil {
			// Messages with control characters are an array of bytes
			var raw []byte
			if json.Unmarshal(entry.Message, &raw) != nil {
				continue
			}
			message = string(raw)
		}
		if !kernelLogPattern.MatchString(message) {
			continue
		}
		timestamp := entry.Timestamp
		if us, err := strconv.ParseInt(entry.Timestamp, 10, 64); err == nil {
			timestamp = time.UnixMicro(us).Format(time.RFC3339)
		}
		lines = append(lines, timestamp+" "+message)
	}
	return strings.Join(lines, "\n")
}

// supportedFields returns the fields which are listed in the output of nvidia-smi --help-query-gpu
func supportedFields(help string, fields []string) []string {
	known := map[string]bool{}
	quoted := regexp.MustCompile(`"([a-z0-9_.\[\]]+)"`)
	for _, line := range strings.Split(help, "\n") {
		// Field names are at the beginning of a line, e.g.: "clocks_event_reasons.active" or "clocks_throttle_reasons.active"
		if !strings.HasPrefix(strings.TrimSpace(line), `"`) {
			continue
		}
		for _, match := range quoted.FindAllStringSubmatch(line, -1) {
			known[match[1]] = true
		}
	}
	var result []string
	for _, field := range fields {
		if known[field] {
			result = append(result, field)
		}
	}
	return result
}

func platformInfo(ctx context.Context) map[string]string {
	info := map[string]string{}
	switch runtime.GOOS {
	case "linux":
		if data, err := os.ReadFile("/etc/os-release"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if value, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
					info["os_release"] = strings.Trim(value, `"`)
				}
			}
		}
		if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
			info["kernel"] = strings.TrimSpace(string(data))
		}
		if data, err := os.ReadFile("/proc/driver/nvidia/version"); err == nil {
			info["driver"] = strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
		}
		// GPUs with unified memory (e.g. DGX Spark / GB10) have no own memory, it is the memory of the system
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				for _, key := range []string{"MemTotal", "MemAvailable", "SwapTotal", "SwapFree"} {
					if value, ok := strings.CutPrefix(line, key+":"); ok {
						info[strings.ToLower(key)] = strings.TrimSpace(value)
					}
				}
			}
		}
	case "windows":
		c := runCommand(ctx, time.Minute, "os", powershell("$o = Get-CimInstance Win32_OperatingSystem; \"$($o.Caption) $($o.Version)|$($o.TotalVisibleMemorySize)\"")...)
		if parts := strings.SplitN(strings.TrimSpace(c.Stdout), "|", 2); len(parts) == 2 {
			info["os_release"] = parts[0]
			info["memtotal"] = parts[1] + " kB"
		}
	}
	return info
}

// powershell returns the command line of a PowerShell script. The output is UTF-8, otherwise it is encoded with the
// code page of the console (e.g. German umlauts of the event log messages).
func powershell(script string) []string {
	return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8\n" + script}
}

func findNvidiaSmi() string {
	if path := findExecutable("nvidia-smi"); path != "" {
		return path
	}
	if runtime.GOOS == "windows" {
		for _, path := range []string{
			filepath.Join(os.Getenv("SystemRoot"), "System32", "nvidia-smi.exe"),
			filepath.Join(os.Getenv("ProgramFiles"), "NVIDIA Corporation", "NVSMI", "nvidia-smi.exe"),
		} {
			if _, err := os.Stat(path); err == nil {
				return path
			}
		}
	}
	return ""
}

func findExecutable(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

var (
	uuidPattern      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	xmlSerialPattern = regexp.MustCompile(`<([a-z_]*serial[a-z_]*)>([^<]+)</`)
	csvSerialPattern = regexp.MustCompile(`(?i)\bserial="([^"]+)"`)
	hostnameLabel    = regexp.MustCompile(`(?i)\bhostname="[^"]*"`)
	// The process paths contain the user name, e.g. C:\Users\name\AppData\... or /home/name/...
	windowsUserPath = regexp.MustCompile(`(?i)(\\Users\\)[^\\"<>\r\n]+`)
	linuxUserPath   = regexp.MustCompile(`(/home/)[^/"<>\s]+`)
)

const serialPlaceholder = "SERIAL%04d"

// anonymize replaces UUIDs, serial numbers, the host name and user names in paths by placeholders. The same value gets the same
// placeholder in all outputs, so the GPUs can still be assigned between the outputs.
func anonymize(d *dump) {
	serials := map[string]string{}
	addSerial := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || strings.EqualFold(value, "N/A") || strings.HasPrefix(value, "[") || serials[value] != "" {
			return
		}
		// GeForce GPUs report the serial number 0, which identifies nothing. Short values would also replace parts of
		// other values, e.g. every 0 of all outputs.
		if len(value) < 4 || strings.Trim(value, "0") == "" {
			return
		}
		serials[value] = fmt.Sprintf(serialPlaceholder, len(serials)+1)
	}
	for _, c := range d.Commands {
		for _, match := range xmlSerialPattern.FindAllStringSubmatch(c.Stdout, -1) {
			addSerial(match[2])
		}
		for _, match := range csvSerialPattern.FindAllStringSubmatch(c.Stdout, -1) {
			addSerial(match[1])
		}
		if c.Name == "query-gpu" {
			for _, value := range csvColumn(c.Stdout, "serial") {
				addSerial(value)
			}
		}
	}
	// Longer values first, so a serial number which contains another one is replaced completely
	values := make([]string, 0, len(serials))
	for value := range serials {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })

	uuids := map[string]string{}
	hostname, _ := os.Hostname()
	replace := func(text string) string {
		text = uuidPattern.ReplaceAllStringFunc(text, func(uuid string) string {
			key := strings.ToLower(uuid)
			if uuids[key] == "" {
				uuids[key] = fmt.Sprintf("00000000-0000-0000-0000-%012d", len(uuids)+1)
			}
			return uuids[key]
		})
		for _, value := range values {
			text = replaceWholeValue(text, value, serials[value])
		}
		text = hostnameLabel.ReplaceAllString(text, `Hostname="host"`)
		text = windowsUserPath.ReplaceAllString(text, "${1}user")
		text = linuxUserPath.ReplaceAllString(text, "${1}user")
		if len(hostname) >= 3 {
			text = strings.ReplaceAll(text, hostname, "host")
		}
		return text
	}
	for _, c := range d.Commands {
		c.Stdout = replace(c.Stdout)
		c.Stderr = replace(c.Stderr)
		c.Error = replace(c.Error)
	}
}

// replaceWholeValue replaces the value only if it is not part of a longer alphanumeric value
func replaceWholeValue(text, value, replacement string) string {
	isAlnum := func(c byte) bool {
		return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	var b strings.Builder
	for {
		i := strings.Index(text, value)
		if i < 0 {
			b.WriteString(text)
			return b.String()
		}
		end := i + len(value)
		if (i > 0 && isAlnum(text[i-1])) || (end < len(text) && isAlnum(text[end])) {
			b.WriteString(text[:end])
		} else {
			b.WriteString(text[:i])
			b.WriteString(replacement)
		}
		text = text[end:]
	}
}

// csvColumn returns the values of a column of the CSV output of nvidia-smi
func csvColumn(output, column string) []string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return nil
	}
	index := -1
	for i, name := range strings.Split(lines[0], ",") {
		if strings.TrimSpace(name) == column {
			index = i
		}
	}
	if index < 0 {
		return nil
	}
	var values []string
	for _, line := range lines[1:] {
		if fields := strings.Split(line, ","); index < len(fields) {
			values = append(values, strings.TrimSpace(fields[index]))
		}
	}
	return values
}
