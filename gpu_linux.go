//go:build linux

package metrics

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const drmRoot = "/sys/class/drm"

var pciIDsPaths = []string{
	"/usr/share/hwdata/pci.ids",
	"/usr/share/misc/pci.ids",
}

var nvidiaSMI = runNvidiaSMI

type gpuFound struct {
	card string
	bdf  string
	gpu  GPU
}

func readGPU(drmRoot string, pciIDs []string, smi func() ([]byte, error)) (GPUSnapshot, error) {
	now := time.Now()
	entries, err := os.ReadDir(drmRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return GPUSnapshot{CollectedAt: now}, nil
		}
		return GPUSnapshot{}, err
	}

	var (
		found     []gpuFound
		issues    []Issue
		hasNvidia bool
	)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		if !strings.HasPrefix(name, "card") || strings.Contains(name, "-") {
			continue
		}
		dev := filepath.Join(drmRoot, name, "device")
		driver := gpuDriver(dev)
		if driver == "" {
			driver = gpuUeventDriver(dev)
		}
		if driver == "simpledrm" {
			continue
		}
		vendor, errV := readSysfsString(filepath.Join(dev, "vendor"))
		device, errD := readSysfsString(filepath.Join(dev, "device"))
		if errV != nil || errD != nil {
			if errV != nil {
				issues = append(issues, Issue{Source: filepath.Join(dev, "vendor"), Err: errV})
			}
			if errD != nil {
				issues = append(issues, Issue{Source: filepath.Join(dev, "device"), Err: errD})
			}
			continue
		}
		pci := formatPCIID(vendor, device)
		g := GPU{PCIID: pci, Driver: driver, Name: lookupPCIName(pciIDs, vendor, device)}
		if strings.HasPrefix(pci, "10de:") {
			hasNvidia = true
		}
		if busy, err := readSysfsInt(filepath.Join(dev, "gpu_busy_percent")); err == nil {
			g.Usage = GPUUsage{Fraction: float64(busy) / 100, Valid: true}
		}
		if c, ok := readGPUHwmonTemp(dev); ok {
			g.Celsius, g.TempValid = c, true
		}
		found = append(found, gpuFound{card: name, bdf: gpuBDF(dev), gpu: g})
	}

	if hasNvidia && smi != nil {
		out, err := smi()
		if err != nil {
			issues = append(issues, Issue{Source: "nvidia-smi", Err: err})
		} else {
			applyNvidiaCSV(found, out)
		}
	}

	sort.Slice(found, func(i, j int) bool {
		if found[i].gpu.PCIID != found[j].gpu.PCIID {
			return found[i].gpu.PCIID < found[j].gpu.PCIID
		}
		return found[i].card < found[j].card
	})
	out := make([]GPU, len(found))
	for i, g := range found {
		out[i] = g.gpu
	}
	return GPUSnapshot{CollectedAt: now, GPUs: out, Issues: issues}, nil
}

func gpuDriver(dev string) string {
	link, err := os.Readlink(filepath.Join(dev, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(link)
}

func gpuUeventDriver(dev string) string {
	body, err := os.ReadFile(filepath.Join(dev, "uevent"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "DRIVER=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "DRIVER="))
		}
	}
	return ""
}

func gpuBDF(dev string) string {
	link, err := os.Readlink(dev)
	if err != nil {
		return filepath.Base(dev)
	}
	if !filepath.IsAbs(link) {
		link = filepath.Join(filepath.Dir(dev), link)
	}
	return filepath.Base(filepath.Clean(link))
}

func formatPCIID(vendor, device string) string {
	return strings.ToLower(strings.TrimPrefix(vendor, "0x")) + ":" + strings.ToLower(strings.TrimPrefix(device, "0x"))
}

func readGPUHwmonTemp(dev string) (float64, bool) {
	matches, err := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*", "temp1_input"))
	if err != nil || len(matches) == 0 {
		return 0, false
	}
	sort.Strings(matches)
	milli, err := readSysfsInt(matches[0])
	if err != nil {
		return 0, false
	}
	return float64(milli) / 1000, true
}

func lookupPCIName(paths []string, vendor, device string) string {
	vendor = strings.ToLower(strings.TrimPrefix(vendor, "0x"))
	device = strings.ToLower(strings.TrimPrefix(device, "0x"))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if name := parsePCIName(string(body), vendor, device); name != "" {
			return name
		}
	}
	return ""
}

func parsePCIName(ids, vendor, device string) string {
	inVendor := false
	for _, line := range strings.Split(ids, "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "\t") {
			inVendor = strings.HasPrefix(strings.ToLower(line), vendor)
			continue
		}
		if !inVendor || strings.HasPrefix(line, "\t\t") {
			continue
		}
		fields := strings.SplitN(strings.TrimPrefix(line, "\t"), " ", 2)
		if len(fields) >= 2 && strings.ToLower(fields[0]) == device {
			return strings.TrimSpace(fields[1])
		}
	}
	return ""
}

func applyNvidiaCSV(found []gpuFound, out []byte) {
	rows := parseNvidiaCSV(out)
	for i := range found {
		for _, row := range rows {
			if !matchBDF(found[i].bdf, row.bdf) {
				continue
			}
			if !found[i].gpu.Usage.Valid && row.hasUtil {
				found[i].gpu.Usage = GPUUsage{Fraction: row.util / 100, Valid: true}
			}
			if !found[i].gpu.TempValid && row.hasTemp {
				found[i].gpu.Celsius, found[i].gpu.TempValid = row.temp, true
			}
			if found[i].gpu.Name == "" && row.name != "" {
				found[i].gpu.Name = row.name
			}
		}
	}
}

func parseNvidiaCSV(out []byte) []nvidiaRow {
	var rows []nvidiaRow
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		parts := strings.Split(string(line), ",")
		if len(parts) < 3 {
			continue
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		row := nvidiaRow{bdf: parts[0]}
		if u, err := strconv.ParseFloat(parts[1], 64); err == nil {
			row.util, row.hasUtil = u, true
		}
		if c, err := strconv.ParseFloat(parts[2], 64); err == nil {
			row.temp, row.hasTemp = c, true
		}
		if len(parts) > 3 {
			row.name = strings.TrimSpace(strings.Join(parts[3:], ","))
		}
		rows = append(rows, row)
	}
	return rows
}

type nvidiaRow struct {
	bdf     string
	util    float64
	hasUtil bool
	temp    float64
	hasTemp bool
	name    string
}

func matchBDF(sysfsBDF, smiBDF string) bool {
	sysfsBDF = strings.ToLower(sysfsBDF)
	smiBDF = strings.ToLower(smiBDF)
	strip := func(s string) string { return strings.TrimPrefix(s, "0000:") }
	return sysfsBDF == smiBDF || strip(sysfsBDF) == strip(smiBDF)
}

func runNvidiaSMI() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=pci.bus_id,utilization.gpu,temperature.gpu,name",
		"--format=csv,noheader,nounits")
	return cmd.Output()
}
