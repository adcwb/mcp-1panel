package shared

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// OperationTransport declares endpoint-v1 wire semantics. Its presence changes
// the policy hash; older Core implementations cannot silently grant these tools.
// BodySchema validates the complete path/query/body/form/headers/stream envelope.
type OperationTransport struct {
	Kind       string   `json:"kind"`
	Encoding   string   `json:"encoding"`
	FileFields []string `json:"fileFields,omitempty"`
	NodeQuery  bool     `json:"nodeQuery,omitempty"`
	Response   string   `json:"response,omitempty"`
}

// EndpointTransport executes only an operation in the current tool's manifest.
// Callers cannot supply a host, HTTP method, path template or authentication.
type EndpointTransport interface {
	RequestOperation(context.Context, string, map[string]any) (json.RawMessage, error)
}

func IsEndpointTool(definition ToolDefinition) bool {
	return len(definition.Operations) == 1 && definition.Operations[0].Transport != nil
}

var endpointSchemas sync.Map

// CompileEndpointSchema resolves only local references. Caching avoids repeating
// schema compilation for cloned catalog definitions on every invocation.
func CompileEndpointSchema(schema Schema) (*jsonschema.Resolved, error) {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	key := sha256.Sum256(encoded)
	if cached, ok := endpointSchemas.Load(key); ok {
		return cached.(*jsonschema.Resolved), nil
	}
	var value jsonschema.Schema
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, err
	}
	resolved, err := value.Resolve(nil)
	if err != nil {
		return nil, err
	}
	actual, _ := endpointSchemas.LoadOrStore(key, resolved)
	return actual.(*jsonschema.Resolved), nil
}

func ValidateEndpointSchema(schema Schema, value any) error {
	resolved, err := CompileEndpointSchema(schema)
	if err != nil {
		return ContractError("Invalid compiled API input schema")
	}
	validated, err := endpointValidationValue(value)
	if err != nil || resolved.Validate(validated) != nil {
		// Validator diagnostics can contain passwords and file contents.
		return InvalidArguments()
	}
	return nil
}

// jsonschema-go v0.4.3 treats json.Number as a string during type validation.
// Validate a numeric copy, retaining the original JSON numbers for forwarding.
func endpointValidationValue(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		if number, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			return number, nil
		}
		if number, err := strconv.ParseUint(string(v), 10, 64); err == nil {
			return number, nil
		}
		// Bound rational parsing, including extreme JSON exponents, before it
		// can allocate an arbitrarily large numerator or denominator.
		if len(v) > 128 {
			return nil, InvalidArguments()
		}
		if _, exponent, ok := strings.Cut(strings.ToLower(string(v)), "e"); ok {
			n, err := strconv.Atoi(exponent)
			if err != nil || n < -324 || n > 308 {
				return nil, InvalidArguments()
			}
		}
		number, ok := new(big.Rat).SetString(string(v))
		if !ok {
			return nil, InvalidArguments()
		}
		if number.IsInt() {
			if number.Num().IsInt64() {
				return number.Num().Int64(), nil
			}
			if number.Num().IsUint64() {
				return number.Num().Uint64(), nil
			}
			return nil, InvalidArguments()
		}
		fraction, err := v.Float64()
		if err != nil || math.IsNaN(fraction) || math.IsInf(fraction, 0) || math.Trunc(fraction) == fraction {
			return nil, InvalidArguments()
		}
		return fraction, nil
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, item := range v {
			converted, err := endpointValidationValue(item)
			if err != nil {
				return nil, err
			}
			result[key] = converted
		}
		return result, nil
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			converted, err := endpointValidationValue(item)
			if err != nil {
				return nil, err
			}
			result[i] = converted
		}
		return result, nil
	default:
		return value, nil
	}
}

// JSON Schema accepts 1.0 and 1e0 as integers, but Go's integer DTO decoder
// requires 1. Canonicalize exact integers only, after successful validation.
// Fractional values retain their original precision when forwarded.
func canonicalEndpointNumbers(value any) any {
	switch v := value.(type) {
	case json.Number:
		converted, _ := endpointValidationValue(v)
		switch n := converted.(type) {
		case int64:
			return json.Number(strconv.FormatInt(n, 10))
		case uint64:
			return json.Number(strconv.FormatUint(n, 10))
		}
	case map[string]any:
		for key, item := range v {
			v[key] = canonicalEndpointNumbers(item)
		}
	case []any:
		for i, item := range v {
			v[i] = canonicalEndpointNumbers(item)
		}
	}
	return value
}

// EndpointPath binds every declared path parameter to one escaped URL segment.
// Slash, percent and dot segments are rejected before transport to prevent a
// proxy's second URL decode from changing the authorized operation.
func EndpointPath(op Operation, args map[string]any) (string, map[string]string, error) {
	if op.Transport == nil || !strings.HasPrefix(op.Path, "/api/v2/") || strings.ContainsAny(op.Path, "?#%\\") {
		return "", nil, InvalidArguments()
	}
	values, _ := args["path"].(map[string]any)
	params := map[string]string{}
	parts := strings.Split(op.Path, "/")
	for i, part := range parts {
		if !strings.HasPrefix(part, ":") {
			continue
		}
		name := strings.TrimPrefix(part, ":")
		value, exists := values[name]
		if !exists {
			return "", nil, InvalidArguments()
		}
		text, err := EndpointScalar(value)
		if err != nil || text == "" || text == "." || text == ".." || strings.ContainsAny(text, "/\\%\x00\r\n") {
			return "", nil, InvalidArguments()
		}
		params[name], parts[i] = text, url.PathEscape(text)
	}
	return strings.Join(parts, "/"), params, nil
}

func EndpointScalar(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case json.Number:
		// Query and multipart scalars cannot use JSON's exponent notation when
		// the panel parses an integer with strconv.Atoi/ParseInt.
		if !strings.ContainsAny(string(v), ".eE") {
			return string(v), nil
		}
		if number, ok := new(big.Rat).SetString(string(v)); ok && number.IsInt() && number.Num().BitLen() <= 64 {
			return number.Num().String(), nil
		}
		return "", fmt.Errorf("API scalar numbers must be bounded integers")
	case bool:
		return strconv.FormatBool(v), nil
	default:
		return "", fmt.Errorf("not a scalar API parameter")
	}
}
