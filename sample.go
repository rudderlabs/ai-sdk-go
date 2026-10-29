package rudderstackai

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
)

// sampleRandom draws for events without an interaction_id; replaced in tests.
var sampleRandom = rand.Float64

// WithSampleRates sends a share of each listed event, by event name, from 0 to 1:
// {"ai_llm_response_received": 0.1, "mcp_tool_called": 0.05}. The draw is keyed on
// interaction_id, so the sampled events of one interaction are kept or dropped together (at
// random for events without one); kept events carry sample_rate. Events not listed are all
// sent: leave ai_response_received out, as cost totals need every answer. It covers MCP server
// events too. Replaces the previous rates; nil clears them.
func WithSampleRates(rates map[string]float64) ConfigOption {
	return func(c *config, _ *pending) error {
		for event, rate := range rates {
			if event == "" {
				return errors.New("rudderstackai: sample rates need non-empty event names")
			}
			if !(rate >= 0 && rate <= 1) {
				return fmt.Errorf("rudderstackai: sample rate for %q must be from 0 to 1", event)
			}
		}
		c.sampleRates = maps.Clone(rates)
		return nil
	}
}

// sampleBucket is where an interaction falls in [0, 1): the first 4 bytes of SHA-256 over its id.
// Every event of the interaction gets the same draw, so they are kept or dropped together.
// Identical in Node.js and Python.
func sampleBucket(interactionID string) float64 {
	sum := sha256.Sum256([]byte(interactionID))
	return float64(binary.BigEndian.Uint32(sum[:4])) / (1 << 32)
}

// sample is the properties to send under rates: unchanged when the event has no rate below 1,
// a copy with sample_rate when kept; kept is false when the event is sampled out.
func sample(event string, properties map[string]any, rates map[string]float64) (_ map[string]any, kept bool) {
	rate, ok := rates[event]
	if !ok || rate >= 1 {
		return properties, true
	}
	var draw float64
	if id, _ := properties["interaction_id"].(string); id != "" {
		draw = sampleBucket(id)
	} else {
		draw = sampleRandom()
	}
	if draw >= rate {
		debugf("sampled out", "event", event)
		return nil, false
	}
	out := maps.Clone(properties)
	if out == nil {
		out = map[string]any{}
	}
	out["sample_rate"] = rate
	return out, true
}
