package codexproxy

import "encoding/json"

// extraMembers returns the members of a validated extra_body object in the form
// they are appended to a request body: compact, without the braces. nil when
// there is nothing to add.
func extraMembers(raw string) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &obj) != nil || len(obj) == 0 {
		return nil
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return b[1 : len(b)-1]
}

// withExtra appends members to body, a marshaled JSON object with members of its own.
func withExtra(body, members []byte) []byte {
	if len(members) == 0 {
		return body
	}
	out := make([]byte, 0, len(body)+len(members)+1)
	out = append(out, body[:len(body)-1]...)
	out = append(out, ',')
	out = append(out, members...)
	return append(out, '}')
}
