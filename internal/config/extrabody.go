package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxExtraBodyBytes bounds extra_body: it rides the daemon's command line.
const MaxExtraBodyBytes = 4096

// ExtraBodyReservedKeys returns the request-body keys the daemon sets itself for
// protocol, which extra_body must not contain; nil for a protocol that takes no
// extra_body. The result is sorted.
func ExtraBodyReservedKeys(protocol string) []string {
	switch protocol {
	case ProtocolOpenAIChat:
		return []string{"max_tokens", "messages", "model", "parallel_tool_calls", "stream", "stream_options", "tool_choice", "tools"}
	case ProtocolOpenAIResponses:
		return []string{"include", "input", "instructions", "max_output_tokens", "model", "parallel_tool_calls", "prompt_cache_key", "reasoning", "store", "stream", "tool_choice", "tools"}
	}
	return nil
}

// ValidateExtraBody reports why raw is not a usable extra_body for protocol:
// only openai-* protocols take one, and it must be valid UTF-8, at most
// MaxExtraBodyBytes, and a single JSON object with no reserved key (compared
// case-insensitively). "" is always valid.
func ValidateExtraBody(protocol, raw string) error {
	if raw == "" {
		return nil
	}
	reserved := ExtraBodyReservedKeys(protocol)
	if reserved == nil {
		return errors.New("extra_body is only valid for an openai-* protocol")
	}
	if !utf8.ValidString(raw) {
		return errors.New("extra_body must be valid UTF-8")
	}
	if len(raw) > MaxExtraBodyBytes {
		return fmt.Errorf("extra_body is %d bytes, over the %d-byte limit", len(raw), MaxExtraBodyBytes)
	}
	// Unmarshal also rejects anything after the object. A JSON null decodes to a
	// nil map without an error.
	var obj map[string]json.RawMessage
	err := json.Unmarshal([]byte(raw), &obj)
	if err == nil && obj == nil {
		err = errors.New("not an object")
	}
	if err != nil {
		return fmt.Errorf(`extra_body must be a JSON object, for example {"provider":{"only":["deepseek"]}} (%v)`, err)
	}
	// Go's JSON decoders match keys case-insensitively, so "Model" would be read as
	// "model" by an upstream gateway written in Go. reserved is sorted, so the
	// first match is the same one every time.
	for _, r := range reserved {
		for k := range obj {
			if strings.EqualFold(k, r) {
				return fmt.Errorf("extra_body must not set %q: the proxy sets it", r)
			}
		}
	}
	return nil
}

// extraBodyIsString reports an error naming the provider when its raw TOML table
// holds extra_body as anything but a string (a TOML table is the likely slip).
// Called by parse and ParseBundle before the table is re-encoded.
func extraBodyIsString(name string, table map[string]any) error {
	if v, ok := table["extra_body"]; ok {
		if _, isStr := v.(string); !isStr {
			return fmt.Errorf(`config: provider %q: extra_body must be a string holding a JSON object, for example extra_body = '{"provider":{"only":["deepseek"]}}'`, name)
		}
	}
	return nil
}
