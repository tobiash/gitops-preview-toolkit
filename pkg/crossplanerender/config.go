// Package crossplanerender implements the persistent Crossplane render plugin.
// Functions execute through official runtimes and an external real reconciler.
package crossplanerender

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const EngineVersion = "v2.4.2"

// Config is trusted OpenRender configuration, never populated from manifests.
// A Development target overrides Runtime for that specific Function only.
type Config struct {
	EngineBinary       string            `json:"engineBinary,omitempty"`
	Runtime            string            `json:"runtime,omitempty"`
	DevelopmentTargets map[string]string `json:"developmentTargets,omitempty"`
	DockerEnv          map[string]string `json:"dockerEnv,omitempty"`
	Timeout            string            `json:"timeout,omitempty"`
	MaxFunctions       int               `json:"maxFunctions,omitempty"`
}

func parseConfig(raw json.RawMessage) (Config, time.Duration, error) {
	cfg := Config{
		EngineBinary: "crossplane-core", Runtime: "Docker", Timeout: "1m", MaxFunctions: 32,
		DevelopmentTargets: map[string]string{}, DockerEnv: map[string]string{},
	}
	if len(raw) != 0 {
		if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
			return cfg, 0, fmt.Errorf("crossplane config must be a JSON object")
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, 0, fmt.Errorf("decode Crossplane config: %w", err)
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return cfg, 0, fmt.Errorf("crossplane config must contain one JSON object")
		}
	}
	if cfg.EngineBinary == "" {
		return cfg, 0, fmt.Errorf("engineBinary must not be empty")
	}
	if cfg.Runtime != "Docker" && cfg.Runtime != "Development" {
		return cfg, 0, fmt.Errorf("runtime must be Docker or Development")
	}
	duration, err := time.ParseDuration(cfg.Timeout)
	if err != nil || duration <= 0 || duration > 10*time.Minute {
		return cfg, 0, fmt.Errorf("timeout must be a positive duration no greater than 10m")
	}
	if cfg.MaxFunctions < 1 || cfg.MaxFunctions > 256 {
		return cfg, 0, fmt.Errorf("maxFunctions must be between 1 and 256")
	}
	for name, target := range cfg.DevelopmentTargets {
		host, port, err := net.SplitHostPort(target)
		if err != nil || host == "" || name == "" {
			return cfg, 0, fmt.Errorf("development target %q must be an explicit host:port", name)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return cfg, 0, fmt.Errorf("development target %q has an invalid port", name)
		}
	}
	for key, value := range cfg.DockerEnv {
		if key == "" || strings.ContainsAny(key, "=,\r\n") || strings.ContainsAny(value, ",\r\n") {
			return cfg, 0, fmt.Errorf("dockerEnv contains an invalid key or value")
		}
	}
	return cfg, duration, nil
}
