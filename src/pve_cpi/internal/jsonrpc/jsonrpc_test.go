package jsonrpc

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------

// readGolden returns the trimmed content of a testdata file.
func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("readGolden %q: %v", name, err)
	}
	return strings.TrimRight(string(b), "\n")
}

// captureEncode runs fn with a bytes.Buffer and returns trimmed output.
func captureEncode(t *testing.T, fn func(w io.Writer) error) string {
	t.Helper()
	var buf bytes.Buffer
	if err := fn(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// -----------------------------------------------------------------------
// Request JSON decoding (production decode is cmd/cpi's decodeRequest,
// which is plain encoding/json against jsonrpc.Request — no jsonrpc-level
// wrapper exists; these tests exercise the same encoding/json path so
// Request/Context field decoding stays covered).
// -----------------------------------------------------------------------

func TestRequestUnmarshalJSON_Valid(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("testdata/request.json")
	if err != nil {
		t.Fatalf("read testdata/request.json: %v", err)
	}

	var req Request
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatalf("Unmarshal: unexpected error: %v", err)
	}

	if req.Method != "create_vm" {
		t.Errorf("Method = %q, want %q", req.Method, "create_vm")
	}
	if len(req.Arguments) != 6 {
		t.Errorf("len(Arguments) = %d, want 6", len(req.Arguments))
	}
	if req.Context.DirectorUUID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("Context.DirectorUUID = %q, want 550e8400-...", req.Context.DirectorUUID)
	}
	if req.Context.RequestID != "cpi-abc123" {
		t.Errorf("Context.RequestID = %q, want cpi-abc123", req.Context.RequestID)
	}
	// Verify stemcell api_version encoded inside Context.VM map.
	vm, ok := req.Context.VM["stemcell"]
	if !ok {
		t.Fatal("Context.VM[\"stemcell\"] missing")
	}
	stemcellMap, ok := vm.(map[string]any)
	if !ok {
		t.Fatalf("Context.VM[\"stemcell\"] type = %T, want map[string]any", vm)
	}
	apiVer, ok := stemcellMap["api_version"]
	if !ok {
		t.Fatal("stemcell[\"api_version\"] missing")
	}
	// JSON numbers unmarshal as float64 into map[string]any.
	apiVerF, ok := apiVer.(float64)
	if !ok {
		t.Fatalf("api_version type = %T, want float64", apiVer)
	}
	if int(apiVerF) != 2 {
		t.Errorf("api_version = %v, want 2", apiVerF)
	}
	if req.APIVersion != 2 {
		t.Errorf("APIVersion = %d, want 2", req.APIVersion)
	}
}

// -----------------------------------------------------------------------
// Context.Extra capture (context config overrides — see
// config.ApplyContextOverrides and handlers.Deps.WithRequestOverrides)
// -----------------------------------------------------------------------

// TestContext_UnmarshalJSON_ExtraCapturesArbitraryPVEKeys is a decode
// round-trip test: a context object carrying pve_* keys BOSH's cpi-config
// feature merges in for a multi-cluster CPI entry must have those keys
// land in Context.Extra with their original JSON-decoded types preserved
// (string stays string, number decodes float64, bool stays bool), while the
// known typed fields (director_uuid, request_id) still decode exactly as
// before Extra capture existed.
func TestContext_UnmarshalJSON_ExtraCapturesArbitraryPVEKeys(t *testing.T) {
	t.Parallel()
	input := `{
		"director_uuid": "550e8400-e29b-41d4-a716-446655440000",
		"request_id": "cpi-abc123",
		"pve_host": "10.255.0.10",
		"pve_port": 8006,
		"pve_verify_ssl": false,
		"pve_vm_storage": "az2-vms"
	}`

	var ctx Context
	if err := json.Unmarshal([]byte(input), &ctx); err != nil {
		t.Fatalf("Unmarshal: unexpected error: %v", err)
	}

	if ctx.DirectorUUID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("DirectorUUID = %q, want the known UUID", ctx.DirectorUUID)
	}
	if ctx.RequestID != "cpi-abc123" {
		t.Errorf("RequestID = %q, want cpi-abc123", ctx.RequestID)
	}

	if got, want := len(ctx.Extra), 4; got != want {
		t.Fatalf("len(Extra) = %d, want %d (Extra=%#v)", got, want, ctx.Extra)
	}
	if host, ok := ctx.Extra["pve_host"].(string); !ok || host != "10.255.0.10" {
		t.Errorf("Extra[pve_host] = %#v, want string \"10.255.0.10\"", ctx.Extra["pve_host"])
	}
	// JSON numbers decode as float64 into map[string]any, matching every
	// other any-typed JSON decode in this codebase (see e.g.
	// TestDecode_Valid's api_version assertion above).
	if port, ok := ctx.Extra["pve_port"].(float64); !ok || port != 8006 {
		t.Errorf("Extra[pve_port] = %#v, want float64(8006)", ctx.Extra["pve_port"])
	}
	if verifySSL, ok := ctx.Extra["pve_verify_ssl"].(bool); !ok || verifySSL != false {
		t.Errorf("Extra[pve_verify_ssl] = %#v, want bool(false)", ctx.Extra["pve_verify_ssl"])
	}
	if storage, ok := ctx.Extra["pve_vm_storage"].(string); !ok || storage != "az2-vms" {
		t.Errorf("Extra[pve_vm_storage] = %#v, want string \"az2-vms\"", ctx.Extra["pve_vm_storage"])
	}

	// director_uuid/request_id must NOT also leak into Extra.
	if _, ok := ctx.Extra["director_uuid"]; ok {
		t.Error("Extra must not contain the known field \"director_uuid\"")
	}
	if _, ok := ctx.Extra["request_id"]; ok {
		t.Error("Extra must not contain the known field \"request_id\"")
	}
}

// TestContext_UnmarshalJSON_NoExtraKeysLeavesExtraNil confirms a context
// object carrying only the known fields (the overwhelming common case —
// single-CPI deployments never populate cpi-config properties) decodes with
// a nil Extra map, so config.ApplyContextOverrides' len(extra)==0 fast path
// (and every len(reqCtx.Extra)==0 check in handlers.Deps.WithRequestOverrides)
// is reached without any extra allocation.
func TestContext_UnmarshalJSON_NoExtraKeysLeavesExtraNil(t *testing.T) {
	t.Parallel()
	var ctx Context
	if err := json.Unmarshal([]byte(`{"director_uuid":"x","request_id":"y"}`), &ctx); err != nil {
		t.Fatalf("Unmarshal: unexpected error: %v", err)
	}
	if ctx.Extra != nil {
		t.Errorf("Extra = %#v, want nil", ctx.Extra)
	}
}

// TestContext_UnmarshalJSON_VMStemcellStillDecodeAsKnownFields guards against
// a regression where VM/Stemcell would be double-captured into Extra instead
// of (or in addition to) their typed fields.
func TestContext_UnmarshalJSON_VMStemcellStillDecodeAsKnownFields(t *testing.T) {
	t.Parallel()
	input := `{"vm":{"stemcell":{"api_version":2}},"stemcell":{"foo":"bar"},"pve_node":"pve02"}`
	var ctx Context
	if err := json.Unmarshal([]byte(input), &ctx); err != nil {
		t.Fatalf("Unmarshal: unexpected error: %v", err)
	}
	if ctx.VM == nil {
		t.Fatal("VM should decode into the typed VM field")
	}
	if ctx.Stemcell == nil {
		t.Fatal("Stemcell should decode into the typed Stemcell field")
	}
	if _, ok := ctx.Extra["vm"]; ok {
		t.Error("Extra must not contain the known field \"vm\"")
	}
	if _, ok := ctx.Extra["stemcell"]; ok {
		t.Error("Extra must not contain the known field \"stemcell\"")
	}
	if node, ok := ctx.Extra["pve_node"].(string); !ok || node != "pve02" {
		t.Errorf("Extra[pve_node] = %#v, want string \"pve02\"", ctx.Extra["pve_node"])
	}
}

// TestContext_UnmarshalJSON_Null confirms a null context (context field
// entirely absent, or explicitly null) decodes to a zero Context without error.
func TestContext_UnmarshalJSON_Null(t *testing.T) {
	t.Parallel()
	var ctx Context
	if err := json.Unmarshal([]byte(`null`), &ctx); err != nil {
		t.Fatalf("Unmarshal(null): unexpected error: %v", err)
	}
	if ctx.Extra != nil || ctx.DirectorUUID != "" || ctx.RequestID != "" {
		t.Errorf("Unmarshal(null) should leave a zero Context, got %#v", ctx)
	}
}

// TestRequestUnmarshalJSON_CapturesContextExtra is an end-to-end round trip
// through the Request envelope (the actual production decode path is
// cmd/cpi's decodeRequest, plain encoding/json against jsonrpc.Request),
// confirming context.Extra survives nested inside a full Request decode, not
// only a standalone Context decode.
func TestRequestUnmarshalJSON_CapturesContextExtra(t *testing.T) {
	t.Parallel()
	input := `{
		"method": "create_stemcell",
		"arguments": ["/tmp/image.tgz", {}],
		"context": {
			"director_uuid": "d1",
			"request_id": "r1",
			"pve_host": "10.255.0.10",
			"pve_api_token": "root@pam!cpi=deadbeef"
		}
	}`
	var req Request
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		t.Fatalf("Unmarshal: unexpected error: %v", err)
	}
	if req.Context.Extra["pve_host"] != "10.255.0.10" {
		t.Errorf("Context.Extra[pve_host] = %#v, want \"10.255.0.10\"", req.Context.Extra["pve_host"])
	}
	if req.Context.Extra["pve_api_token"] != "root@pam!cpi=deadbeef" {
		t.Errorf("Context.Extra[pve_api_token] = %#v, want the token string", req.Context.Extra["pve_api_token"])
	}
}

// -----------------------------------------------------------------------
// EncodeSuccess tests
// -----------------------------------------------------------------------

func TestEncodeSuccess(t *testing.T) {
	t.Parallel()
	got := captureEncode(t, func(w io.Writer) error {
		return EncodeSuccess(w, "vm-cid-001", "")
	})

	// Unmarshal into Response struct and verify fields rather than doing a
	// string comparison (map literal marshals keys alphabetically, struct does not).
	var resp Response
	if err := json.Unmarshal([]byte(got), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if resp.Result != "vm-cid-001" {
		t.Errorf("result = %v, want vm-cid-001", resp.Result)
	}
	if resp.Error != nil {
		t.Errorf("error = %v, want nil", resp.Error)
	}
	if resp.Log != "" {
		t.Errorf("log = %q, want empty", resp.Log)
	}
}

func TestEncodeSuccess_MatchesGoldenFile(t *testing.T) {
	t.Parallel()
	got := captureEncode(t, func(w io.Writer) error {
		return EncodeSuccess(w, "vm-cid-001", "")
	})

	// Golden file is compact JSON; strip trailing newline before compare.
	golden := readGolden(t, "response_ok.json")
	if got != golden {
		t.Errorf("EncodeSuccess golden mismatch\n got: %s\nwant: %s", got, golden)
	}
}

func TestEncodeSuccess_NilResult(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := EncodeSuccess(&buf, nil, ""); err != nil {
		t.Fatalf("EncodeSuccess: %v", err)
	}
	raw := strings.TrimRight(buf.String(), "\n")
	// result must be JSON null.
	var resp map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if string(resp["result"]) != "null" {
		t.Errorf("result = %s, want null", resp["result"])
	}
	if string(resp["error"]) != "null" {
		t.Errorf("error = %s, want null", resp["error"])
	}
}

func TestEncodeSuccess_NilWriter(t *testing.T) {
	t.Parallel()
	err := EncodeSuccess(nil, "x", "")
	if err == nil {
		t.Fatal("expected error for nil writer, got nil")
	}
}

func TestEncodeSuccess_WithLog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := EncodeSuccess(&buf, 42, "some log output"); err != nil {
		t.Fatalf("EncodeSuccess: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Log != "some log output" {
		t.Errorf("Log = %q, want %q", resp.Log, "some log output")
	}
	if resp.Error != nil {
		t.Errorf("Error = %v, want nil", resp.Error)
	}
}

// -----------------------------------------------------------------------
// EncodeError tests
// -----------------------------------------------------------------------

func TestEncodeError(t *testing.T) {
	t.Parallel()
	got := captureEncode(t, func(w io.Writer) error {
		return EncodeError(w, "Bosh::Clouds::CloudError", "something went wrong", false, "stack trace here")
	})

	golden := readGolden(t, "response_err.json")
	if got != golden {
		t.Errorf("EncodeError golden mismatch\n got: %s\nwant: %s", got, golden)
	}
}

func TestEncodeError_AllFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := EncodeError(&buf,
		"Bosh::Clouds::RetriableCloudError",
		"disk not found: vol-abc",
		true,
		"debug info",
	); err != nil {
		t.Fatalf("EncodeError: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Result != nil {
		t.Errorf("Result = %v, want nil", resp.Result)
	}
	if resp.Error == nil {
		t.Fatal("Error is nil")
	}
	if resp.Error.Type != "Bosh::Clouds::RetriableCloudError" {
		t.Errorf("Type = %q, want RetriableCloudError", resp.Error.Type)
	}
	if resp.Error.Message != "disk not found: vol-abc" {
		t.Errorf("Message = %q", resp.Error.Message)
	}
	if !resp.Error.OkToRetry {
		t.Error("OkToRetry = false, want true")
	}
	if resp.Log != "debug info" {
		t.Errorf("Log = %q, want %q", resp.Log, "debug info")
	}
}

func TestEncodeError_EmptyTypeDefaultsToCloudError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := EncodeError(&buf, "", "oops", false, ""); err != nil {
		t.Fatalf("EncodeError: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("Error is nil")
	}
	if resp.Error.Type != "Bosh::Clouds::CloudError" {
		t.Errorf("Type = %q, want Bosh::Clouds::CloudError", resp.Error.Type)
	}
}

func TestEncodeError_NilWriter(t *testing.T) {
	t.Parallel()
	err := EncodeError(nil, "Bosh::Clouds::CloudError", "msg", false, "")
	if err == nil {
		t.Fatal("expected error for nil writer, got nil")
	}
}

// -----------------------------------------------------------------------
// Round-trip test
// -----------------------------------------------------------------------

// TestRoundTrip decodes a request, encodes a success response, then decodes
// the response back to verify structural consistency across encode/decode.
func TestRoundTrip(t *testing.T) {
	t.Parallel()
	// 1. Decode request from testdata.
	b, err := os.ReadFile("testdata/request.json")
	if err != nil {
		t.Fatalf("read request.json: %v", err)
	}

	var req Request
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatalf("Unmarshal request: %v", err)
	}
	if req.Method != "create_vm" {
		t.Errorf("Method = %q, want create_vm", req.Method)
	}

	// 2. Encode a success response using a value derived from the request.
	result := "vm-cid-" + req.Context.RequestID
	var buf bytes.Buffer
	if err := EncodeSuccess(&buf, result, ""); err != nil {
		t.Fatalf("EncodeSuccess: %v", err)
	}

	// 3. Decode the response envelope back.
	raw := bytes.TrimRight(buf.Bytes(), "\n")
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	// result should round-trip as a JSON string.
	wantResult := `"vm-cid-cpi-abc123"`
	resultBytes, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if string(resultBytes) != wantResult {
		t.Errorf("result = %s, want %s", resultBytes, wantResult)
	}

	if resp.Error != nil {
		t.Errorf("Error = %v, want nil", resp.Error)
	}
	if resp.Log != "" {
		t.Errorf("Log = %q, want empty", resp.Log)
	}
}

// -----------------------------------------------------------------------
// Single-line output verification
// -----------------------------------------------------------------------

// TestOutputIsSingleLine verifies that encoder output does not contain
// embedded newlines (i.e. is not pretty-printed).
func TestOutputIsSingleLine(t *testing.T) {
	t.Parallel()
	t.Run("success", func(t *testing.T) {
		var buf bytes.Buffer
		if err := EncodeSuccess(&buf, map[string]any{"a": 1, "b": 2}, "log"); err != nil {
			t.Fatal(err)
		}
		line := strings.TrimRight(buf.String(), "\n")
		if strings.ContainsRune(line, '\n') {
			t.Errorf("success output contains embedded newline: %q", buf.String())
		}
	})

	t.Run("error", func(t *testing.T) {
		var buf bytes.Buffer
		if err := EncodeError(&buf, "Bosh::Clouds::CloudError", "msg", false, "log"); err != nil {
			t.Fatal(err)
		}
		line := strings.TrimRight(buf.String(), "\n")
		if strings.ContainsRune(line, '\n') {
			t.Errorf("error output contains embedded newline: %q", buf.String())
		}
	})
}
