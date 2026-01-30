package config

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// ParseMemory parses memory string (e.g., "1Gi", "2048", "1.5GB") to megabytes
func ParseMemory(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}

	s = strings.TrimSpace(s)

	// Pure number (assume MB for backward compatibility)
	if num, err := strconv.ParseInt(s, 10, 64); err == nil {
		return num, nil
	}

	// Parse with unit
	re := regexp.MustCompile(`^([0-9.]+)\s*([A-Za-z]+)$`)
	matches := re.FindStringSubmatch(s)
	if len(matches) != 3 {
		return 0, fmt.Errorf("invalid memory format: %s", s)
	}

	val, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory value: %s", matches[1])
	}

	unit := strings.ToLower(matches[2])
	var mb int64

	switch unit {
	case "b", "bytes":
		mb = int64(val / (1024 * 1024))
	case "k", "kb", "ki", "kib":
		mb = int64(val / 1024)
	case "m", "mb", "mi", "mib":
		mb = int64(val)
	case "g", "gb", "gi", "gib":
		mb = int64(val * 1024)
	case "t", "tb", "ti", "tib":
		mb = int64(val * 1024 * 1024)
	default:
		return 0, fmt.Errorf("unknown memory unit: %s", unit)
	}

	return mb, nil
}

// ParseCPU parses CPU string (e.g., "2", "0.5", "500m") to vCPU units
func ParseCPU(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}

	s = strings.TrimSpace(s)

	// Pure number (vCPU units)
	if num, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(num * 1024), nil
	}

	// Millicores (e.g., "500m" = 0.5 CPU)
	if strings.HasSuffix(s, "m") {
		milliStr := strings.TrimSuffix(s, "m")
		milli, err := strconv.ParseFloat(milliStr, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid CPU millicores: %s", s)
		}
		return int64(milli * 1024 / 1000), nil
	}

	return 0, fmt.Errorf("invalid CPU format: %s", s)
}

// RoundUpECSMemory rounds up memory to next valid ECS value
func RoundUpECSMemory(cpu, memory int64) int64 {
	validMemories := getValidECSMemories(cpu)
	if len(validMemories) == 0 {
		return memory
	}

	for _, valid := range validMemories {
		if memory <= valid {
			return valid
		}
	}

	// Return highest if over max
	return validMemories[len(validMemories)-1]
}

// RoundUpECSCPU rounds up CPU to next valid ECS value
func RoundUpECSCPU(cpu int64) int64 {
	validCPUs := []int64{256, 512, 1024, 2048, 4096, 8192, 16384}

	for _, valid := range validCPUs {
		if cpu <= valid {
			return valid
		}
	}

	return 16384 // Max
}

func getValidECSMemories(cpu int64) []int64 {
	validCombinations := map[int64][]int64{
		256:   {512, 1024, 2048},
		512:   {1024, 2048, 3072, 4096},
		1024:  {2048, 3072, 4096, 5120, 6144, 7168, 8192},
		2048:  {4096, 5120, 6144, 7168, 8192, 9216, 10240, 11264, 12288, 13312, 14336, 15360, 16384},
		4096:  {8192, 9216, 10240, 11264, 12288, 13312, 14336, 15360, 16384, 17408, 18432, 19456, 20480, 21504, 22528, 23552, 24576, 25600, 26624, 27648, 28672, 29696, 30720},
		8192:  {16384, 20480, 24576, 28672, 32768, 36864, 40960, 45056, 49152, 53248, 57344, 61440},
		16384: {32768, 40960, 49152, 57344, 65536, 73728, 81920, 90112, 98304, 106496, 114688, 122880},
	}

	return validCombinations[cpu]
}

// NormalizeECSResources normalizes and rounds up CPU and Memory for ECS
func NormalizeECSResources(cpuStr, memoryStr string) (string, string, error) {
	// Parse CPU
	cpuMB, err := ParseCPU(cpuStr)
	if err != nil {
		return "", "", fmt.Errorf("parse CPU: %w", err)
	}

	// Parse Memory
	memoryMB, err := ParseMemory(memoryStr)
	if err != nil {
		return "", "", fmt.Errorf("parse memory: %w", err)
	}

	// Round up CPU to valid ECS value
	cpuRounded := RoundUpECSCPU(cpuMB)

	// Round up Memory to valid ECS value based on CPU
	memoryRounded := RoundUpECSMemory(cpuRounded, memoryMB)

	return fmt.Sprintf("%d", cpuRounded), fmt.Sprintf("%d", memoryRounded), nil
}

// FormatK8sResource formats resource string for K8s
// If input is pure number, adds "Mi" suffix
func FormatK8sResource(s string, resourceType string) string {
	if s == "" {
		return ""
	}

	s = strings.TrimSpace(s)

	// Already has unit suffix
	re := regexp.MustCompile(`^[0-9.]+[A-Za-z]+$`)
	if re.MatchString(s) {
		return s
	}

	// Pure number - add appropriate suffix
	if resourceType == "memory" {
		// Assume MB and convert to Mi
		if num, err := strconv.ParseInt(s, 10, 64); err == nil {
			return fmt.Sprintf("%dMi", num)
		}
	} else if resourceType == "cpu" {
		// Already in vCPU units (1024 = 1 vCPU)
		if num, err := strconv.ParseInt(s, 10, 64); err == nil {
			if num >= 1024 {
				// Convert to whole CPUs
				cpus := float64(num) / 1024.0
				if math.Mod(float64(num), 1024) == 0 {
					return fmt.Sprintf("%d", int64(cpus))
				}
				return fmt.Sprintf("%.2f", cpus)
			} else {
				// Convert to millicores
				return fmt.Sprintf("%dm", num*1000/1024)
			}
		}
	}

	return s
}
