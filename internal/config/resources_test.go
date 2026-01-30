package config

import (
	"testing"
)

func TestParseMemory(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{"empty string", "", 0, false},
		{"pure number MB", "2048", 2048, false},
		{"Mi unit", "512Mi", 512, false},
		{"GB unit", "1GB", 1024, false},
		{"Gi unit", "2Gi", 2048, false},
		{"Ki unit", "1024Ki", 1, false},
		{"Ti unit", "1Ti", 1048576, false},
		{"unknown unit", "100X", 0, true},
		{"invalid format", "abc", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMemory(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseMemory(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseMemory(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseCPU(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{"empty string", "", 0, false},
		{"integer vCPU", "2", 2048, false},
		{"decimal vCPU", "0.5", 512, false},
		{"500m millicores", "500m", 512, false},
		{"1000m millicores", "1000m", 1024, false},
		{"250m millicores", "250m", 256, false},
		{"invalid format", "abc", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCPU(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseCPU(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseCPU(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestRoundUpECSCPU(t *testing.T) {
	tests := []struct {
		name  string
		input int64
		want  int64
	}{
		{"exact 256", 256, 256},
		{"exact 1024", 1024, 1024},
		{"300 rounds to 512", 300, 512},
		{"over max rounds to 16384", 20000, 16384},
		{"0 rounds to 256", 0, 256},
		{"513 rounds to 1024", 513, 1024},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RoundUpECSCPU(tt.input)
			if got != tt.want {
				t.Errorf("RoundUpECSCPU(%d) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestRoundUpECSMemory(t *testing.T) {
	tests := []struct {
		name   string
		cpu    int64
		memory int64
		want   int64
	}{
		{"cpu 256, memory fits 512", 256, 400, 512},
		{"cpu 256, memory fits 1024", 256, 600, 1024},
		{"cpu 256, memory over max", 256, 9999, 2048},
		{"cpu 1024, memory fits 2048", 1024, 1500, 2048},
		{"cpu 1024, memory over max", 1024, 99999, 8192},
		{"unknown cpu tier returns original", 999, 4096, 4096},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RoundUpECSMemory(tt.cpu, tt.memory)
			if got != tt.want {
				t.Errorf("RoundUpECSMemory(%d, %d) = %d, want %d", tt.cpu, tt.memory, got, tt.want)
			}
		})
	}
}

func TestNormalizeECSResources(t *testing.T) {
	tests := []struct {
		name    string
		cpu     string
		memory  string
		wantCPU string
		wantMem string
		wantErr bool
	}{
		{"valid combo", "1", "4Gi", "1024", "4096", false},
		{"invalid cpu", "abc", "1024", "", "", true},
		{"invalid memory", "1", "100X", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCPU, gotMem, err := NormalizeECSResources(tt.cpu, tt.memory)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizeECSResources(%q, %q) error = %v, wantErr %v", tt.cpu, tt.memory, err, tt.wantErr)
			}
			if !tt.wantErr {
				if gotCPU != tt.wantCPU {
					t.Errorf("NormalizeECSResources cpu = %q, want %q", gotCPU, tt.wantCPU)
				}
				if gotMem != tt.wantMem {
					t.Errorf("NormalizeECSResources memory = %q, want %q", gotMem, tt.wantMem)
				}
			}
		})
	}
}

func TestFormatK8sResource(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		resourceType string
		want         string
	}{
		{"empty string", "", "memory", ""},
		{"already has unit", "512Mi", "memory", "512Mi"},
		{"pure number memory", "2048", "memory", "2048Mi"},
		{"cpu 1024 -> 1", "1024", "cpu", "1"},
		{"cpu 512 -> 500m", "512", "cpu", "500m"},
		{"cpu 2048 -> 2", "2048", "cpu", "2"},
		{"cpu already has unit", "500m", "cpu", "500m"},
		{"unknown type returns as-is", "42", "other", "42"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatK8sResource(tt.input, tt.resourceType)
			if got != tt.want {
				t.Errorf("FormatK8sResource(%q, %q) = %q, want %q", tt.input, tt.resourceType, got, tt.want)
			}
		})
	}
}
