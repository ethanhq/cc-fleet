package workflow

import (
	"fmt"
	"strings"

	"go.starlark.net/lib/json"
	"go.starlark.net/starlark"
)

// jsonEncode / jsonDecode reuse go.starlark.net's tested JSON module instead of a
// bespoke Go<->Starlark converter. Both are called via starlark.Call UNDER THE GIL
// (like any Starlark op), so they participate in the single-interpreter discipline.
var (
	jsonEncode = json.Module.Members["encode"]
	jsonDecode = json.Module.Members["decode"]
)

// encodeSchema serializes a Starlark schema value to a JSON string (appended to the
// prompt) and extracts the top-level keys the reply must contain: the schema's
// `required` list if present, else its `properties` keys, else none (then validation
// is "valid JSON" only). Runs under the GIL. v1 is deliberately SHALLOW — deep,
// recursive JSON-Schema enforcement is a v3 concern.
func encodeSchema(thread *starlark.Thread, schema starlark.Value) (jsonText string, requiredKeys []string, err error) {
	enc, err := starlark.Call(thread, jsonEncode, starlark.Tuple{schema}, nil)
	if err != nil {
		return "", nil, err
	}
	s, ok := starlark.AsString(enc)
	if !ok {
		return "", nil, fmt.Errorf("json.encode did not return a string")
	}
	d, ok := schema.(*starlark.Dict)
	if !ok {
		return s, nil, nil // a non-dict schema → valid-JSON-only validation
	}
	if reqv, found, _ := d.Get(starlark.String("required")); found {
		if lst, ok := reqv.(*starlark.List); ok {
			iter := lst.Iterate()
			defer iter.Done()
			var x starlark.Value
			for iter.Next(&x) {
				if ks, ok := starlark.AsString(x); ok {
					requiredKeys = append(requiredKeys, ks)
				}
			}
		}
	} else if propv, found, _ := d.Get(starlark.String("properties")); found {
		if pd, ok := propv.(*starlark.Dict); ok {
			for _, k := range pd.Keys() {
				if ks, ok := starlark.AsString(k); ok {
					requiredKeys = append(requiredKeys, ks)
				}
			}
		}
	}
	return s, requiredKeys, nil
}

// decodeAndValidate parses the vendor reply as JSON into a Starlark value and checks
// every requiredKey is present (when the decoded value is an object). Runs under the
// GIL. A parse failure or a missing key is an error the caller retries on.
func decodeAndValidate(thread *starlark.Thread, reply string, requiredKeys []string) (starlark.Value, error) {
	v, err := starlark.Call(thread, jsonDecode, starlark.Tuple{starlark.String(stripCodeFence(reply))}, nil)
	if err != nil {
		return nil, fmt.Errorf("reply is not valid JSON: %v", err)
	}
	if len(requiredKeys) > 0 {
		d, ok := v.(*starlark.Dict)
		if !ok {
			return nil, fmt.Errorf("reply must be a JSON object containing keys %v", requiredKeys)
		}
		for _, k := range requiredKeys {
			if _, found, _ := d.Get(starlark.String(k)); !found {
				return nil, fmt.Errorf("reply is missing required key %q", k)
			}
		}
	}
	return v, nil
}

// stripCodeFence removes a leading/trailing markdown code fence (```json … ```), a
// common vendor habit despite the prompt's "no fences" instruction, so the JSON
// inside still decodes. Leaves un-fenced replies untouched.
func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	t = strings.TrimPrefix(t, "```")
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:] // drop the ```/```json info line
	}
	if i := strings.LastIndex(t, "```"); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}
